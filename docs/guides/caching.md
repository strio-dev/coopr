# Caching

Coopr can reuse local instruction snapshots, component invocation results, and package results. RUN cache mounts hold mutable tool data and are separate from immutable result caches.

```sh
coopr build image.coopr --cache oci-layout:.coopr-cache
coopr component build ./components/settings/component.coopr --cache oci-layout:.coopr-cache
coopr build image.coopr --cache-from registry:registry.example.com/team/cache --cache-to oci-layout:.coopr-cache
```

`--cache` is read/write shorthand. `--cache-from` reads and `--cache-to` writes; each is repeatable. Local OCI layouts and registry repositories can be combined.

## Local and portable results

Local RUN, COPY, ADD, and WORKDIR snapshots reuse matching selected inputs and effective controls. Portable instruction results include conventional networked RUNs and declared mounts when their input identities are complete, local copies/additions, and workdir operations. Device-backed RUNs are excluded. Package-result caches use a separate key covering the selected producer closure, fixed arguments, platform, immutable inputs, and context when used.

Networked package RUNs and declared mounts can use conventional result caching. Credentials, network responses, and cache-mount contents do not invalidate a cached result. Unpinned remote ADD, nested component operations, and elevated host/insecure/device operations can bypass whole-package reuse when their input or authorization closure is incomplete.

## Refresh deliberately

`--no-cache` bypasses result lookups and saves fresh results; it does not erase cache-mount contents. `--cache-ttl DURATION` limits reads by publication age. An explicit zero bypasses reads while recording fresh results. Reproducible image timestamps do not determine cache age.

Secret and SSH mount declarations enter cache identity; their values do not. Changed credentials may therefore require `--no-cache` or an authored argument to refresh a result. Host volume/device contents follow the same conventional cache rule. Elevated entitlements are checked even on a cache hit.

## Cache mounts

```kdl
run "go build ./..." {
    mount "cache" target="/root/.cache/go-build"
}
```

A mount without an `id` receives a deterministic scope-specific name. Explicit IDs keep the selected sharing mode. Cache mounts persist in the execution store and should be treated as disposable accelerators. Cache hits are not proof that undeclared inputs are reproducible. The [execution contract](../reference/execution.md) describes identities and eligibility in detail.
