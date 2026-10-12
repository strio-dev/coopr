# Package reusable components

Package two components with their own files, then apply both to a UBI9 image. The first installs settings; the second checks for those settings before adding a policy. Unlike a [local file reference](local-components.md), a packaged component can be used without its source checkout.

You need [Coopr](../getting-started/index.md), Podman, and access to Docker Hub. Run Coopr and Podman as the same user with the same storage settings.

Create a workspace and enter it:

```sh
mkdir -p reusable-components
cd reusable-components
mkdir -p components/settings components/policy
```

Run the remaining commands from this directory.

## Package settings

Save this as `components/settings/settings.conf`:

```ini
enabled=true
```

Save this as `components/settings/component.coopr`:

```kdl
// Capture the settings file when packaging the component.
package as="payload" {
    copy "settings.conf" "/settings.conf"
}

// Apply the packaged settings to the calling image.
extend as="configured" {
    arg "channel" "stable"
    env demo_channel="${channel}"
    copy "/settings.conf" "/etc/demo/settings.conf" from="payload"
}
```

The `package` stage starts empty and captures the file. `extend` begins the steps applied to the calling image. Because `channel` is declared inside `extend`, each caller can choose its value without rebuilding the package.

```sh
coopr component build components/settings/component.coopr --tag coopr-demo-settings
```

Passing a definition file uses its parent directory as the build context. Here, `copy "settings.conf"` reads the file beside `component.coopr`.

## Package a policy

Save this as `components/policy/policy.txt`:

```text
Use approved configuration.
```

Save this as `components/policy/component.coopr`:

```kdl
// Capture the policy file when packaging the component.
package as="payload" {
    copy "policy.txt" "/policy.txt"
}

// Check the calling image before adding the packaged policy.
extend as="checked" {
    run "test -f /etc/demo/settings.conf"
    copy "/policy.txt" "/etc/demo/policy.txt" from="payload"
}
```

```sh
coopr component build components/policy/component.coopr --tag coopr-demo-policy
```

The check belongs to the consuming image's build, where it can read the caller's filesystem. Packaging the policy captures its file without running the check.

## Compose the image

Save this as `image.coopr`:

```kdl
from "docker.io/redhat/ubi9:latest" {
    // Apply the packaged settings, then check them with the policy.
    component "coopr-demo-settings" {
        arg "channel" "preview"
    }
    component "coopr-demo-policy"

    // Print both files when the container starts.
    cmd {
        exec "cat" "/etc/demo/settings.conf" "/etc/demo/policy.txt"
    }
}
```

These names select the artifacts you just packaged in Coopr's component store. They do not read the source definitions. The settings call overrides the default `channel` with `preview`.

```sh
coopr build image.coopr --tag coopr-demo:configured
podman run --rm localhost/coopr-demo:configured
```

Expected container output:

```{ .text .no-copy }
enabled=true
Use approved configuration.
```

The order matters: placing the policy call first fails its check because the settings file has not been installed yet.

Inspect the image's environment to see the argument override:

```sh
podman inspect localhost/coopr-demo:configured --format '{{json .Config.Env}}'
```

The result includes `demo_channel=preview`.

## Change the settings

After editing `components/settings/settings.conf`, rerun the settings component build, then the image build. A packaged component keeps its captured files until you rebuild it; rebuilding the image alone does not reread those files.

Changing only the `arg "channel" "preview"` binding in `image.coopr` requires rebuilding the image, without repackaging either component.

## Share the components

These commands keep the packaged components local. To share them with other repositories, [publish to an OCI registry](../guides/components.md#build-an-oci-component). Use digest references for the component and its external inputs when the build must select fixed content.

The same definitions and payloads are available in the [checked-in example](https://github.com/strio-dev/coopr/tree/main/examples/reusable-components).
