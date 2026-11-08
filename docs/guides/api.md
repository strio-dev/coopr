# Use the local execution API

Build images and components through HTTP on a private Unix socket. Use the [Go service client](client.md) from Go, or send requests directly as shown below. Ordinary Coopr commands do not need a service.

## Before you start

[Install Coopr](../getting-started/index.md) and configure rootless builds on Linux. This example also uses curl and jq. It copies a local file into a scratch image without pulling a base image or running a container command.

## Start the service

In one terminal, create a private working directory and print its path:

```sh
work=$(mktemp -d)
printf '%s\n' "$work"
coopr system service --socket "$work/api.sock"
```

Leave the service running. In a second terminal under the same account, set `work` to the printed directory, then check the service:

```sh
work=/tmp/REPLACE_WITH_PRINTED_DIRECTORY
curl --fail --unix-socket "$work/api.sock" http://localhost/v1/info
```

The response includes `apiVersion: 1` and an `execute` capability. The URL's `localhost` is a placeholder; curl connects through the Unix socket.

## Prepare a request

Create the input and output parent directories:

```sh
mkdir "$work/context" "$work/exports"
printf 'Hello from the API!\n' > "$work/context/message.txt"
```

Write a request that copies the message into an image and exports its filesystem:

```sh
jq -n --arg work "$work" '{
  kind: "execute",
  program: {
    states: [{
      id: "result",
      from: {image: "scratch"},
      operations: [{kind: "copy", sources: ["message.txt"], destination: "/message.txt"}]
    }],
    output: "result"
  },
  options: {
    contextDir: ($work + "/context"),
    store: {runRoot: ($work + "/run"), graphRoot: ($work + "/graph"), driver: "vfs"},
    output: {oci: ($work + "/exports/image"), filesystem: ($work + "/exports/files")}
  }
}' > "$work/request.json"
```

Paths refer to files on the service host. Keep stores and outputs outside the input context. Output paths must not already exist; their parent directories must exist.

## Build and read the result

Choose a fresh execution ID and submit the request:

```sh
id=$(tr -d '-' < /proc/sys/kernel/random/uuid)
curl --fail --no-buffer --unix-socket "$work/api.sock" \
  -H 'Content-Type: application/json' --data-binary @"$work/request.json" \
  "http://localhost/v1/executions/$id" > "$work/events.jsonl"
jq -e 'select(.type == "result") | if .error then error(.error.message) else .result end' \
  "$work/events.jsonl"
cat "$work/exports/files/message.txt"
```

Expected file contents:

```{ .text .no-copy }
Hello from the API!
```

The terminal `result` event reports the image identity and export paths. HTTP success alone does not mean the build succeeded. Logs arrive as separate events with base64-encoded byte chunks; production clients must validate the complete stream. The [Go client](client.md) handles this for you.

To repeat the example, choose fresh output paths and a new execution ID. IDs cannot be reused during a service's lifetime.

## Cancel and stop

If a request is interrupted or its outcome is uncertain, cancel that execution and wait for the response:

```sh
curl --fail --unix-socket "$work/api.sock" -X DELETE \
  "http://localhost/v1/executions/$id"
```

A `cleanup_failed` error means cleanup could not be confirmed. Keep input and staging paths available until it is resolved. When finished, stop the foreground service with Ctrl-C and wait for it to exit. Successful outputs and stores remain in `work`.

A supervising application can pass `--parent-fd FD`, an inherited pipe descriptor numbered 3 or above. Pipe EOF or SIGTERM requests cancellation and shutdown. Keep the socket directory private: the API runs with the service account's filesystem and registry access, and is not a remote or cross-account interface.

For native definitions, component loading and artifact transfers, see the [API reference](../reference/api.md). For host requirements and cleanup limits, see [security](security.md).
