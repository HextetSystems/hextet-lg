# Hextet LG

Hextet LG is a read-only web looking glass for one or more OpenBGPD `bgplgd` APIs. It provides IPv4 and IPv6 route searches, BGP neighbor and RTR session views, and shareable search URLs in a server-rendered web interface. OpenBGPD is deployed separately.

## Features

- Search live routes by prefix, source ASN, BGP community, neighbor, and supported OpenBGPD filters.
- Query multiple backends independently, with clear complete, partial, or failed status.
- Inspect route paths, BGP neighbors, and RTR sessions; share searches with bookmarkable URLs or plain-text output.
- Customize the site with optional branding, themes, community definitions, and bounded DNS enrichment.

Results use a short-lived, bounded in-memory cache. Links are live queries, not historical snapshots.

## Get started

- [Install and deploy](INSTALL.md) with Docker Compose or a Linux binary.
- [Develop and contribute](https://github.com/HextetSystems/hextet-lg/blob/main/DEVELOPING.md) from a local checkout.
- [Architecture](https://github.com/HextetSystems/hextet-lg/blob/main/ARCHITECTURE.md) describes the main components and request flow.

## Guides

- Deployment: [OpenBGPD setup](docs/OPENBGPD.md) · [Security](docs/SECURITY.md) · [Releases](https://github.com/HextetSystems/hextet-lg/blob/main/docs/RELEASING.md)
- Features: [Community definitions](docs/COMMUNITIES.md) · [Session visibility](https://github.com/HextetSystems/hextet-lg/blob/main/docs/SESSION_VISIBILITY.md) · [Branding](docs/BRANDING.md) · [Themes](docs/THEMING.md) · [PTR enrichment](https://github.com/HextetSystems/hextet-lg/blob/main/docs/PTR_ENRICHMENT.md)
- [Testing](https://github.com/HextetSystems/hextet-lg/blob/main/docs/TESTING.md)

Hextet LG is licensed under the [BSD 2-Clause License](LICENSE).
