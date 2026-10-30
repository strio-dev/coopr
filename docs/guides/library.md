# Use the Go execution library

Import `github.com/strio-dev/coopr` to execute a build graph from Go.
Your application constructs typed states and operations directly. Coopr's CLI
uses the same supervised native backend; the library does not invoke the CLI or
parse a Coopr definition.

The library requires Linux, CGO, GPGME and libseccomp at build time, and their
shared libraries at runtime when dynamically linked. Build with the repository's
Nix environment to use its pinned Go toolchain and native dependencies. The
repository enables `exclude_graphdriver_btrfs`, `systemd` and `seccomp` Go build
tags. RUN operations require an OCI runtime such as `crun`. Rootless execution
also requires working user namespaces, subordinate UID/GID ranges and the
corresponding mapping helpers. Host and nested-container policy can prevent
these facilities; importing the library does not grant them.

## Bootstrap your executable

Call `coopr.InitReexec()` before parsing flags, opening services or starting
other application work, and return immediately when it returns true. The native
backend starts workers by reexecuting your application's binary. Those workers
need this dispatch entry point. A single `Run.Args` invocation does not require
the separate `coopr-run-sequence` helper or an installed Coopr CLI.

The engine runs with your application's OS authority. Bootstrap dispatches
helpers before normal argument checks; it does not authenticate requests.
Installing the application setuid, granting it file capabilities, or exposing
it through a restricted sudo rule can therefore bypass restrictions implemented
by its CLI. The same concern applies to a security-domain transition that gives
the executable additional permissions. The library provides no privilege
separation for those deployments.
Run it as the intended execution account, normally rootless. An operator may
also run builds as root or through sudo with full engine authority. Services
must authenticate build requests before calling `Execute`.

`Execute` applies the native `containers.conf` execution defaults, including
SELinux and seccomp settings, before starting workers. Invalid configured
security profiles fail the request.

The following program builds from UBI, runs a shell command, and exports both an
OCI image layout and a filesystem tree. Choose fresh output paths before running
it; their parent directory must already exist:

```go
package main

import (
    "context"
    "log"

    "github.com/strio-dev/coopr"
)

func main() {
    if coopr.InitReexec() {
        return
    }
    program := coopr.Program{
        States: []coopr.State{{
            ID:   "result",
            From: coopr.Source{Image: "docker.io/redhat/ubi9:latest"},
            Operations: []coopr.Operation{
                coopr.Run{Args: []string{
                    "sh", "-c", "printf 'hello\\n' > /message",
                }},
            },
        }},
        Output: "result",
    }
    result, err := coopr.Execute(context.Background(), program, coopr.Options{
        Store: coopr.StoreOptions{
            RunRoot:   "/tmp/library-run",
            GraphRoot: "/tmp/library-graph",
            Driver:    "vfs",
        },
        Runtime:   "crun",
        Isolation: "rootless",
        Output: coopr.Output{
            OCI:        "/tmp/library-image",
            Filesystem: "/tmp/library-filesystem",
        },
    })
    if err != nil {
        log.Fatal(err)
    }
    log.Printf("image %s exported to %s", result.ManifestDigest, result.Filesystem)
}
```

`Source.Image` selects an image reference, including `scratch`. `Source.State`
selects an earlier state. Specify exactly one. States must be in topological
order, with unique IDs and an explicit final `Program.Output`. Independent
states may run concurrently up to `Options.Jobs`; a state's source and `Copy`
operations establish dependencies. An empty `Copy.From` uses `ContextDir`.

Operations are the value types `Run`, `Copy`, `Env` and `Workdir`. Strings are
literal: the library does not expand Coopr ARG or ENV expressions. `Run.Args`
is an argument vector, so use an explicit shell when shell syntax is needed.
Mounts support bind, cache and tmpfs. Cache mount IDs can be supplied explicitly
or assigned by the native backend; sharing may be `shared`, `private` or `locked`. Coopr's frontend continues to own its
component, package, extend, ARG, ENV and ONBUILD language semantics.

## Own stores, cache and outputs

The caller owns the context directory, native store roots, portable cache and
successful output paths. Use absolute, distinct store roots. `Output.OCI` is
required; `Output.Filesystem` is optional. Each output destination must be fresh
and absolute, with an existing parent directory. The library never overwrites
an existing output. Successful outputs are promoted individually; promoting
both outputs is not one filesystem transaction.

Private export staging is excluded from the build context. Files you leave in
the context, including successful outputs from earlier builds, remain inputs.

Set `Cache.LocalDir` to an absolute directory to enable portable instruction
caching. `Result.Cache` reports portable hits, misses, stored entries, skips and
errors. Native Buildah layer reuse is separate and is not counted there. Reusing
a portable cache with a fresh native store demonstrates portable restoration.
Writable bind mounts (`ReadOnly: false`) operate on a disposable build context,
so later context copies can see their writes without changing the original host
context. Select `ReadOnly: true` for a read-only bind.

The cache mount contents themselves are disposable accelerators; programs must
remain correct when those contents are absent.

Use a cancellable `context.Context` to stop work. `Execute` owns its workers and
staging paths, and waits for cancellation cleanup before returning. The caller
remains responsible for its persistent store/cache directories and successful
outputs. Writers supplied in `Options` receive worker output; synchronize them
when sharing them across concurrent calls.

`RegistryOptions` selects native authentication, certificate, TLS, pull and
signature-policy behavior. Follow your deployment's image policy when using
registry sources. The example above pulls `docker.io/redhat/ubi9:latest` using that native policy.

## Run the independent consumer

[`examples/library`](https://github.com/strio-dev/coopr/tree/main/examples/library)
is a separate Go module named `example.com/library-consumer`. Its only Coopr
import is the public `coopr` package. A checkout-local `replace` directive
points at the repository root; an external application should require its
selected Coopr version instead.

From the repository root, enter the runtime development shell and run:

```sh
nix develop .#runtime
cd examples/library
go run . -busybox "$(command -v busybox)" -work "$PWD/proof"
```

Choose a fresh absolute work directory. The consumer leaves its caller-owned
stores, portable cache and exported outputs there for inspection. Its JSON
report is printed only after all assertions pass:

- Two independent branches rendezvous through a shared cache mount before
  either completes, proving overlap without an elapsed-time threshold.
- A dependent copy joins their files and the exported filesystem contains the
  expected result.
- A second execution uses a fresh native store and requires portable cache hits.
- A build copying the whole context exports inside it without copying private
  staging.
  After removing its successful outputs, a fresh-store repeat must hit the
  portable cache with the same caller inputs.
- A writable bind changes a disposable build context, a subsequent copy sees the
  change, and the original host input remains unchanged. An environment value
  containing `${literal}` also reaches the command unchanged.
- Another execution is cancelled after worker readiness; the consumer checks
  process termination, empty container records, absent cancelled outputs and
  removal of disposable staging paths.

The existing Nix Go checks build and test this separate module with its own
vendor hash. The rootless native VM check runs the installed consumer under an
ordinary user with subordinate IDs, using the existing OCI runtime fixtures.
