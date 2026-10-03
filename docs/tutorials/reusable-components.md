# Reuse two components

Apply shared settings, then a policy check, to the image from the [first tutorial](first-image.md). Each component captures its own files during publication; the consuming build needs only the component reference.

Run from the repository root with the development shell and native CLI available. This tutorial relies on the locally named `coopr-demo:base` image.

## Package settings

`examples/reusable-components/components/settings/component.coopr` is:

```kdl
package as="payload"
copy "settings.conf" "/settings.conf"

extend as="configured"
arg "channel" "stable"
env demo_channel="${channel}"
copy "/settings.conf" "/etc/demo/settings.conf" from="payload"
```

The `package` stage captures the file into an immutable snapshot. The `extend` stage starts from the consuming image. `channel` is declared after `extend`, so invocation can choose it without rebuilding the package.

```sh
coopr component build examples/reusable-components/components/settings/component.coopr --tag coopr-demo-settings
```

## Package a policy

The policy component checks that settings have already been installed, then adds its own file:

```kdl
package as="payload"
copy "policy.txt" "/policy.txt"

extend as="checked"
run "test -f /etc/demo/settings.conf" network="none"
copy "/policy.txt" "/etc/demo/policy.txt" from="payload"
```

```sh
coopr component build examples/reusable-components/components/policy/component.coopr --tag coopr-demo-policy
```

The check executes when the component is invoked, against the caller's filesystem. It does not run while packaging the policy.

## Compose the image

`examples/reusable-components/image.coopr` invokes the two local tags in order:

```kdl
from "coopr-demo:base"
component "local:coopr-demo-settings" channel="preview"
component "local:coopr-demo-policy"
cmd {
    exec "cat" "/etc/demo/settings.conf" "/etc/demo/policy.txt"
}
```

```sh
coopr build examples/reusable-components/image.coopr --tag coopr-demo:configured
coopr copy coopr-demo:configured podman:localhost/coopr-demo:configured
podman run --rm --network=none localhost/coopr-demo:configured
podman inspect localhost/coopr-demo:configured --format '{{json .Config.Env}}'
```

This example replaces the base image's default command with `cat` to inspect both packaged files. Its environment contains `demo_channel=preview`. Reversing the calls fails the policy check on a fresh build because settings do not exist yet.

For registry reuse, copy each component to your registry and use that reference in the definition. Pin the component and its nested OCI inputs by digest when stable selection matters. See [packaging components](../guides/components.md).
