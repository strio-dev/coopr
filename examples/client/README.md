# Standalone API client consumer

This separate Go module imports only `github.com/strio-dev/coopr/client` and
`github.com/strio-dev/coopr/api`. It builds with CGO disabled and never imports
the native execution library or starts a service process.

The local `replace ../..` is for checking this repository's unreleased client.
It is not a published version pin or a Stoke migration. A consuming project
should use the first published Coopr module containing the client and a service
that advertises API v1 plus the capabilities its requests need. Coopr `0.5.0`
does not provide this service/client boundary. Release work remains held.

## Run a request

Start an explicitly built, compatible Coopr service in another terminal, using
a fresh private directory:

```sh
coopr_api_dir=$(mktemp -d)
coopr system service --socket "$coopr_api_dir/api.sock"
```

Keep that foreground process under the caller's supervision. Use its actual
absolute socket path when invoking this consumer from `examples/client`:

```sh
CGO_ENABLED=0 go run . \
  -socket /absolute/private/api.sock \
  -request /absolute/request.json
```

`-request -` reads the request from stdin. Supply one request object using the
public `api.Request` schema. The request can select typed execution, native
image/component builds, or explicit publication. Paths in the request refer to
the service host: prepare its context, store/cache roots, policy and output
paths before sending it. See the [API reference](../../docs/reference/api.md) for complete examples and
operation requirements.

The consumer writes the structured `api.Result` as JSON on stdout. Decoded log
chunks go to stderr. A failed execution can still return a partial result,
including publication outcomes for destinations that succeeded before another
failed. Check the exit status; JSON output alone is not evidence of success.
Structured execution errors retain an execution ID for later reconciliation.

Interrupt or SIGTERM cancels the request. The client independently waits for
cancellation acknowledgement; an acknowledgement failure remains an error with
the execution ID. Closing this consumer only closes idle transport connections.
The caller still owns the service process and must drain/stop it separately.
Do not remove its socket directory while it is running.

## Prove independence

From this directory:

```sh
CGO_ENABLED=0 GOTOOLCHAIN=local go test ./...
CGO_ENABLED=0 GOTOOLCHAIN=local go build -trimpath -o client-consumer .
go version -m ./client-consumer
```

`TestDependencyIndependence` examines `go list -deps -json .` with CGO disabled.
It allows the standard library, this consumer, `coopr/api` and `coopr/client`,
and rejects every selected package with CgoFiles or another import path. The
Nix `client-consumer` check repeats that proof without inheriting Coopr's native
build inputs, and records dependency and binary build information with its
output. This proves package/build independence, not that the remote Coopr
service can execute containers without its native runtime prerequisites.

The consumer's socket and partial-result tests use an in-process fake service.
Real native execution, cancellation, publication and relocation qualification
are separate engine/client checks. No ARM execution is implied by a successful
x86_64 build.
