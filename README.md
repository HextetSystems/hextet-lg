# Hextet LG

Hextet LG supplies a modern, read-only web interface for the `openbgpd/openbgpd` Docker image, replacing the CGI looking-glass frontend that the distributed docker image does not include. It queries the image's `bgplgd` HTTP API and presents IPv4 and IPv6 routes, BGP neighbors, and RTR sessions. Hextet LG and OpenBGPD run separately.

## Features

- Search live routes by prefix, source ASN, BGP community, neighbor, and supported OpenBGPD filters.
- Query multiple backends independently, with clear complete, partial, or failed status.
- Inspect route paths, BGP neighbors, and RTR sessions; share searches with bookmarkable URLs or plain-text output. Neighbor addresses on the Neighbors page link to all routes from that neighbor, which may take time to load.
- Customize the site with optional branding, themes, community definitions, and bounded DNS enrichment.

Results use a short-lived, bounded in-memory cache. Route searches offer both live links and self-contained snapshots of the currently displayed page. BGP path-map links also carry bounded snapshots; these URLs are not stored by the application.

## Get started

- [Install and deploy](INSTALL.md) with Docker Compose or a Linux binary.
- [Develop and contribute](https://github.com/HextetSystems/hextet-lg/blob/main/DEVELOPING.md) from a local checkout.
- [Architecture](docs/ARCHITECTURE.md) describes the main components and request flow.

## Guides

- Deployment: [OpenBGPD setup](docs/OPENBGPD.md) · [Security](docs/SECURITY.md) · [Releases](https://github.com/HextetSystems/hextet-lg/blob/main/docs/RELEASING.md)
- Features: [Community definitions](docs/COMMUNITIES.md) · [Site appearance](docs/THEMING.md)
- [Testing](https://github.com/HextetSystems/hextet-lg/blob/main/docs/TESTING.md)

Hextet LG is licensed under the [BSD 2-Clause License](LICENSE).
