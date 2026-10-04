# Examples

[Install Coopr](../docs/getting-started/index.md), then run the example commands from the repository root. Examples that run containers also need Podman.

| Example | Purpose |
| --- | --- |
| [First image](first-image/README.md) | Build and run an NGINX image. |
| [Reusable components](reusable-components/README.md) | Package two components and apply them to an image. |
| [Local components](local-components/README.md) | Share one local component between two image definitions. |
| [Cache mount](cache-mount/README.md) | Reuse Go compiler cache across builds. |
| [Published components](published-components/README.md) | Package files and a tool, then consume selected component outputs. |

Base-image pulls need Docker Hub access; the first image also needs UBI's public package repositories. The local examples require no component registry or publication.
