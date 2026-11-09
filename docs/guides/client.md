# Use the Go service client

Use `github.com/strio-dev/coopr/client` and the types in
`github.com/strio-dev/coopr/api` to call a local Coopr service from Go. The client
builds with CGO disabled; the service needs Coopr's native runtime dependencies.

## Run the example client

Follow the [API guide](api.md#start-the-service) to start a service and
[prepare its input and request](api.md#prepare-a-request). Stop before submitting
the request so its output paths are still unused.

From a Coopr checkout, run the separate example module, replacing `work` with
the private directory printed by the service setup:

```sh
work=/tmp/REPLACE_WITH_PRINTED_DIRECTORY
cd examples/client
CGO_ENABLED=0 go run . -socket "$work/api.sock" -request "$work/request.json"
cat "$work/exports/files/message.txt"
```

The client writes logs to stderr and the structured result to stdout. The file
contains `Hello from the API!`. Check the command's exit status even if it prints
a partial result. Use fresh output paths before running it again.

## Add the client to your application

In your application's Go module, add the dependency:

```sh
go get github.com/strio-dev/coopr/client
```

Given the service's absolute socket path and an `api.Request`, execute it with:

```go
package integration

import (
    "context"
    "os"

    "github.com/strio-dev/coopr/api"
    "github.com/strio-dev/coopr/client"
)

func invoke(ctx context.Context, socket string, request api.Request) (api.Result, error) {
    connection, err := client.New(socket, client.Options{})
    if err != nil {
        return api.Result{}, err
    }
    defer connection.CloseIdleConnections()
    return connection.Execute(ctx, request, client.StreamOptions{
        Stdout: os.Stdout,
        Stderr: os.Stderr,
    })
}
```

`Execute` accepts typed programs, native image and component builds, and explicit
publication requests. Request paths refer to files on the service host. See the
[API reference](../reference/api.md) for request fields and examples.

## Negotiate capabilities

The client automatically checks API compatibility and the capabilities needed
by each request before submitting it. Use `Info(ctx)` to inspect the service,
or `Negotiate(ctx, caps...)` to require features in advance.

| Feature | Capability |
| --- | --- |
| Typed program | `execute` |
| Native image or component definition | `build` or `component` |
| Artifact publication | `publish` |
| Publication from an OCI image layout | `publish-oci-layout` |
| Component archive loading | `component-load` |
| Named component delivery | `component-load-name` |
| Explicit RUN network | `run-network` |
| Explicit native build platform | `build-platform` |
| Registry cache sources or destinations | `registry-cache` |
| Named contexts | `named-contexts` |
| Image/context/host RUN mounts or persistent cache mount directory | `run-mounts` |

Compatibility depends on the API version and capabilities, not identical client
and executable version strings. A client caches the service version and
capabilities; create a new client when restarting or replacing the service.
`Info` refreshes those values.

## Handle results and cancellation

Inspect both the result and error. Publication to several destinations can
partially succeed; the result retains successful destinations even when another
fails. Use `errors.As` to inspect structured `*api.Error`, `*client.HTTPError`
and `*client.ExecutionError` values. An execution error includes its ID, cause
and any cleanup error.

Cancel the context to stop a request. After cancellation or a broken execution
stream, the client sends a separate cancellation request and waits for cleanup
acknowledgement. The default wait is thirty seconds; set
`Options.CancellationTimeout` to another positive duration if needed. Execution
requests are never automatically retried.

If `errors.Is(err, client.ErrCleanupFailed)` is true, cleanup failed or could
not be confirmed. Keep input trees and retained staging paths available until
cleanup is resolved. `CancelWait(ctx, id)` retries cancellation acknowledgement;
`Get(ctx, id)` reads status without cancelling. Killing the service or closing a
connection does not confirm cleanup.

Stream writers and progress callbacks run synchronously. They must return for
the request to progress; synchronize them when shared across concurrent calls.
`CloseIdleConnections` releases idle connections without stopping the service.

## Load components and select networking

Use `LoadComponent(ctx, api.ComponentLoad{Archive: archivePath,
ComponentStoreDir: storePath}, streams)` to import a component archive. Paths
must be absolute. Set `Name` to distinguish local deliveries when
several archives share a root digest. Build and publish using the same component
store. See [component loading](../reference/api.md#load-components) for capabilities and naming.

Set `api.Options.Network` on execution, build or component requests to choose
the RUN network default. For example, `Network: "none"` disables networking for
RUNs without an explicit override. Registry transfers are separate. Publication
and archive loading reject this option; see [execution controls](../reference/execution.md).
