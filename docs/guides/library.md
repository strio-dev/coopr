# Use the Go execution library

Import `github.com/strio-dev/coopr` to execute a build graph from Go.
Your application constructs typed states and operations directly. Coopr's CLI
uses the same supervised native backend; the library does not invoke the CLI or
parse a Coopr definition. For a separate executable with a lightweight Go
consumer, use the [service client](client.md) instead.

The library requires Linux, CGO, GPGME and libseccomp at build time, and their
shared libraries at runtime when dynamically linked. Build with the repository's
Nix environment to use its pinned Go toolchain and native dependencies. The
repository enables `exclude_graphdriver_btrfs`, `systemd` and `seccomp` Go build
tags. RUN operations require an OCI runtime such as `crun`. Rootless execution
also requires working user namespaces, subordinate UID/GID ranges and the
corresponding mapping helpers. Host or container security policy can prevent
rootless execution. Check them on the machine that will run your application.

## Bootstrap your executable

Call `coopr.InitReexec()` before parsing flags, opening services or starting
other application work, and return immediately when it returns true. The native
backend starts workers by reexecuting your application's binary. Those workers
need this dispatch entry point. A single `Run.Args` invocation does not require
the separate `coopr-run-sequence` helper or an installed Coopr CLI.

The engine runs with your application's OS authority. `InitReexec` dispatches
helpers before normal argument checks; it does not authenticate requests.
Installing the application setuid, granting it file capabilities, or exposing
it through a restricted sudo rule can therefore bypass restrictions implemented
by its CLI. Do not rely on application argument checks to restrict a binary
that gains additional permissions when it starts. The library does not separate those
privileges from build execution.

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
        Network:   "none",
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
or assigned by the native backend; sharing may be `shared` or `locked`. The
backend rejects `private`. Use a native definition through the CLI or
[service client](client.md) for component, package, extend, ARG and ENV
features. The typed library clears inherited ONBUILD triggers without running them.

## Manage stores, cache and outputs

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
errors. Native Buildah layer reuse is separate and is not counted there. Reuse
the portable cache with a fresh native store to test restoration independently
of native layer reuse.

Context bind mounts without `HostPath` operate on a disposable build context.
When `ReadOnly: false`, later context copies can see their writes without
changing the original host context. Select `ReadOnly: true` for a read-only
context bind. An explicit absolute `HostPath` selects a live host directory;
writes there are real host effects. Live host binds bypass result reuse, even
when read-only. Coopr does not copy or isolate their contents.

Cache mounts hold disposable tool data. Builds must still work when those
mounts are empty.

Use a cancellable `context.Context` to stop work. `Execute` owns its workers and
staging paths and drains cancellation cleanup before reporting its outcome.
Check `errors.Is(err, coopr.ErrCleanupFailed)` even when the error also matches
`context.Canceled`: worker cleanup failed or could not be confirmed. Coopr
retains its private execution files and refuses output promotion in that case.
Keep input trees and directories containing retained staging available
until cleanup is confirmed.

The caller remains responsible for its persistent store/cache directories and
successful outputs. Writers supplied in `Options` receive worker output;
synchronize them when sharing them across concurrent calls.

`RegistryOptions` selects native authentication, certificate, TLS, pull and
signature-policy behavior. Follow your deployment's image policy when using
registry sources. The example above pulls `docker.io/redhat/ubi9:latest` using that native policy;
its RUN network is separately disabled. Pin an image digest when
you need subsequent builds to resolve the same image. The example application below
uses only a local static BusyBox and scratch images.

## Run the example application

[`examples/library`](https://github.com/strio-dev/coopr/tree/main/examples/library)
is a separate Go module named `example.com/library-consumer`. Its only Coopr
import is the public `coopr` package. A checkout-local `replace` directive
points at the repository root; an external application should require its
selected Coopr version instead.

From the repository root, enter the runtime development shell and run:

```sh
nix develop --no-update-lock-file path:.#runtime
cd examples/library
go run . -busybox "$(command -v busybox)" -network none -work "$PWD/proof"
```

The consumer's `-network` option forwards `Options.Network`. Empty preserves the
native default; `none` disables RUN networking for this example. A
networked build can explicitly select a provisioned mode such as
`slirp4netns:enable_ipv6=false`. RUN network selection does not block registry
resolution or establish a global offline policy. See [execution
controls](../reference/execution.md).

Choose a fresh absolute work directory. The example leaves its stores, portable
cache and exported outputs there for inspection. It demonstrates concurrent
states, cache reuse, writable context binds and cancellation, then prints a JSON
summary.
