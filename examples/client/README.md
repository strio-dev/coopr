# Go service client

Send a JSON build or transfer request to a running Coopr service. This separate Go module imports `github.com/strio-dev/coopr/client` and `github.com/strio-dev/coopr/api`. It builds with CGO disabled; only the service needs Coopr's native runtime dependencies.

The local `replace` directive uses the Coopr source in this checkout. The client checks API compatibility and required capabilities before submitting a request.

## Run a request

Follow the [API guide](../../docs/guides/api.md#start-the-service) through **Prepare a request** to start the service and create its input files. Leave the request unsubmitted so its output paths remain unused. From `examples/client`, set `work` to the service directory printed by the guide:

```sh
work=/tmp/REPLACE_WITH_PRINTED_DIRECTORY
CGO_ENABLED=0 go run . -socket "$work/api.sock" -request "$work/request.json"
cat "$work/exports/files/message.txt"
```

The exported file contains `Hello from the API!`. Choose fresh output paths before running the request again.

Use `-request -` to read one `api.Request` JSON object from stdin. Requests can execute typed programs, build images or components, or transfer artifacts. All paths refer to the service host. Prepare the context, storage directories, signature policy and output paths there before sending the request. The [API reference](../../docs/reference/api.md) describes each operation.

## Read results and stop work

The example writes an `api.Result` as JSON to stdout and decoded logs to stderr. Check its exit status: a failed request can still return a partial result, including destinations that received an artifact before another transfer failed. Execution errors include an ID you can use to query the service.

Interrupt or SIGTERM requests cancellation. The client waits for acknowledgement even after its request context is canceled. If acknowledgement fails, it returns an error with the execution ID so you can check whether the work stopped.

Exiting the example closes idle client connections. It leaves the service running. Stop the service separately before removing its socket directory.

## Build and test

From `examples/client`:

```sh
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go build -o client-consumer .
```
