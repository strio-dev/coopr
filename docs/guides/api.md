# Use the local execution API

Coopr can serve versioned execution requests over HTTP on a private Unix socket. This is useful for a workflow tool that owns Coopr's lifetime and needs structured execution results without embedding native engine libraries or parsing human CLI output.

The service is currently an unreleased local implementation. Published Coopr `0.5.0` does not include `coopr system service`. Use an explicitly built compatible executable while developing; do not assume an ordinary release installer provides this interface.

Go consumers can use the [lightweight public client](client.md), which shares the API types and owns transport/negotiation/cancellation without importing the native engine. The application still owns the service lifetime.

## Start an owned service

Create a private directory owned by the service account and supply an absolute, unused socket path:

```sh
coopr_api_dir=$(mktemp -d)
chmod 700 "$coopr_api_dir"
coopr system service --socket "$coopr_api_dir/api.sock"
```

This command runs in the foreground. Send requests from another terminal with the same account and socket path. A supervising application can additionally pass `--parent-fd FD`, where `FD` is an inherited pipe descriptor numbered `3` or above. EOF on that pipe cancels the service and drains active requests. The pipe is marked close-on-exec so native workloads do not inherit the parent-lifetime authority.

Normal Coopr build/component commands remain daemonless. The existing [Go library](library.md) remains supported for embedding consumers; this API is another explicit invocation surface, not a requirement to run a shared system daemon.

## Negotiate before execution

Read service identity and capabilities:

```sh
curl --unix-socket /absolute/private/api.sock http://localhost/v1/info
```

Require `apiVersion: 1` and the capability your operation needs. The response advertises `execute`, `build`, `component`, `publish`, `registry-cache`, `named-contexts` and `operation-progress`. Require `registry-cache` before supplying execution cache `registryFrom` or `registryTo` arrays. A binary version string alone is not protocol negotiation. The URL host is an HTTP placeholder; the connection uses only the Unix socket.

Each operation uses a fresh client-generated ID of 32 lowercase hexadecimal characters. Send one JSON request object to `POST /v1/executions/ID`; consume every NDJSON event until the single terminal `result` event. Logs are byte chunks encoded as base64 JSON strings, not necessarily complete text lines. Never scrape log text for artifact identities or treat an HTTP 200 response as execution success.

For a request saved as `request.json`, replace `ID` with a fresh valid execution ID:

```sh
curl --no-buffer --unix-socket /absolute/private/api.sock \
  -H 'Content-Type: application/json' --data-binary @request.json \
  http://localhost/v1/executions/ID
```

To cancel that exact request and wait for backend acknowledgement:

```sh
curl --unix-socket /absolute/private/api.sock -X DELETE \
  http://localhost/v1/executions/ID
```

See [the API reference](../reference/api.md) for exact request fields, result formats and cancellation.

## Select the execution frontend

| Request kind | Operation |
| --- | --- |
| `execute` | Literal typed program: argv, environment, working directory, copy and supported mounts. No native definition expression expansion. |
| `build` | Parse and build native Coopr image definitions with their normal frontend/component semantics. |
| `component` | Build native Coopr component definitions into the caller-selected component store. |
| `publish` | Explicitly transfer an existing image or component to declared destinations. |

Builds do not implicitly publish. Invoke publication after the caller's test/workflow gates. Publication can partially complete across destinations; preserve individual result entries even when the terminal error is nonnil.

Paths are on the service host, not uploaded request files. The caller owns input context, definition files, store/cache roots and successful output destinations. Keep stores, outputs and cache outside the input context. Native image IDs are meaningful only with their originating store; OCI descriptors/exports are the portable boundary.

Image/literal execution outputs are OCI layouts with optional flattened filesystem exports. Component `output.oci` is an **OCI archive file**, not a layout directory; component filesystem export is rejected. The component store defaults beside the native graph root unless explicitly selected.

## Cancel and drain

Keep the execution ID before sending a request. On cancellation, connection loss, malformed/truncated events or an uncertain response, send `DELETE /v1/executions/ID` and wait for its status response. The response arrives after the backend has returned from cancellation/cleanup. An unknown ID becomes a canceled tombstone, so a racing late POST cannot start that ID afterward.

The service records IDs for its lifetime; IDs are single-use. It accepts at most 4,096 execution records, including cancellation tombstones. Start a new owned service when that bound is reached rather than reusing IDs.

Parent-pipe EOF or SIGTERM stops new execution admission, cancels active requests and drains their backends before shutdown. Caller-owned successful outputs and persistent stores/cache survive shutdown. A `cleanup_failed` result means cleanup is incomplete; service exit or forced process termination alone must not be presented as a successful cleanup acknowledgement.

## Authority and deployment

The listener requires an absent absolute socket path in a mode-0700 directory owned by the service account, then creates a mode-0600 socket. Keep that directory private for the entire service lifetime. The API is same-account execution with that account's filesystem, registry credentials and native container authority. It is not a privilege broker, a remote transport, or authentication against other processes with the same account authority.

Do not install Coopr setuid, grant it file capabilities, expose the socket over TCP, or rely on restricted privileged-launcher frontend checks to constrain native helper dispatch. Rootless prerequisites and host/container policy still apply. Schema/lifecycle checks are bounded evidence, not a full security proof for every workload. Read [security and rootless builds](security.md).
