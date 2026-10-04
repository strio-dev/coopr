# Caching

Use a local OCI layout to share cached results across builds:

```sh
coopr build image.coopr --cache oci-layout:.coopr-cache
coopr component build ./components/settings/component.coopr --cache oci-layout:.coopr-cache
```

`--cache` reads and writes. To use different sources and destinations, supply `--cache-from` and `--cache-to`; both are repeatable:

```sh
coopr build image.coopr --cache-from registry:registry.example.com/team/cache --cache-to oci-layout:.coopr-cache
```

## Local and portable results

Coopr caches instruction snapshots, component invocation results, and package results. Local snapshots and cache mounts use the selected [image store](storage.md). Portable results can be stored in OCI layouts or registry repositories.

Reuse depends on the selected inputs and execution controls. Device-backed RUNs cannot use portable instruction results. Unpinned remote ADD, nested component operations, and elevated operations can prevent whole-package reuse. See the [execution reference](../reference/execution.md) for eligibility and key contents.

## Refresh deliberately

Rebuild when changed external data or credentials must affect the result:

```sh
coopr build image.coopr --no-cache
coopr build image.coopr --cache oci-layout:.coopr-cache --cache-ttl 24h
```

`--no-cache` bypasses result reads and saves fresh results. It does not clear cache mounts. `--cache-ttl` limits reads by publication age; `--cache-ttl 0` disables reads while saving fresh results. Image timestamps do not determine cache age.

Network responses, credential values, cache-mount contents, and mutable host-volume/device contents do not invalidate cached results. Secret and SSH declarations do enter cache identity. Use `--no-cache` or change an authored argument when these inputs must force a rebuild. Required entitlements are checked even on a cache hit.

## Cache mounts

Keep disposable tool data between RUNs:

```kdl
run "go build ./..." {
    mount "cache" target="/root/.cache/go-build"
}
```

Mounts without an `id` receive deterministic names scoped to their use. Explicit IDs retain the selected sharing mode. Mount contents persist in the execution store independently of result caches. Treat them as accelerators; a cache hit does not prove the build's external inputs are reproducible.
