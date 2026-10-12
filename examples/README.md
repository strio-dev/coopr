# Examples

[Install Coopr](../docs/getting-started/index.md), then run these examples from the repository root. To run the resulting containers, use Podman as the same user with the same storage settings as Coopr.

| Example | Purpose |
| --- | --- |
| [First image](first-image/README.md) | Generate, package, and apply a hello-world component to an OCI image. |
| [Reusable components](reusable-components/README.md) | Package two components and apply them to an image. |
| [Local components](local-components/README.md) | Share one local component between two image definitions. |
| [Cache mount](cache-mount/README.md) | Reuse Go compiler cache across builds. |
| [Published components](published-components/README.md) | Package files and a tool, then consume selected component outputs. |

Base-image pulls need registry access. Examples with package-installation steps also need access to the base distribution's package repositories. Local component examples need no component registry or publication.
