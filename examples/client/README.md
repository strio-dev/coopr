# Standalone API client consumer

This separate Go module imports only `github.com/strio-dev/coopr/client` and
`github.com/strio-dev/coopr/api`. It builds with CGO disabled and never imports
the native execution library or starts a service process.

The local `replace ../..` uses the Coopr source in this checkout. The client
negotiates API compatibility and the capabilities required by each request.

## Run a request

Follow the [API guide](../../docs/guides/api.md#start-the-service) to start a service
and prepare its context and request. Stop before submitting the request so the
output paths remain unused. From this directory, set `work` to the printed
service directory and run:

```sh
work=/tmp/REPLACE_WITH_PRINTED_DIRECTORY
CGO_ENABLED=0 go run . -socket "$work/api.sock" -request "$work/request.json"
cat "$work/exports/files/message.txt"
```

The exported file contains `Hello from the API!`. Use fresh output paths before
running the same request again.

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

## Build and test

```sh
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go build -o client-consumer .
```

The service still needs Coopr's native runtime dependencies; the client does not.
