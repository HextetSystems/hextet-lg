# Security policy and deployment

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting feature for this repository when it is available. If it is unavailable, contact the maintainers privately before public disclosure; do not report suspected vulnerabilities in public issues or pull requests. Include the affected release, impact, and a reproducible description, but do not include production credentials or private backend data.

## Deployment security

Hextet LG provides application-level controls for untrusted browser input and backend data. Those controls do not secure the host, network, reverse proxy, OpenBGPD instance, or a public deployment by themselves. Operators must protect those parts of the deployment.

### Public access

Terminate TLS at a maintained reverse proxy and configure HSTS there. Keep the application's plain HTTP listener on a private interface or network reachable only by that proxy. Apply per-client request rate limits and connection limits at the proxy: Hextet LG's built-in limiter sees the proxy as its direct peer, and forwarded client identity headers are intentionally ignored.

The source-checkout `compose.yaml` publishes port 8080 on all host interfaces by default. The published-image example at `examples/compose.yaml` binds it to loopback. Do not expose the application port directly to the public internet beside the proxy. Bind it to loopback when the proxy runs on the same host, or use a private container network. For IPv4 and IPv6 access, publish A and AAAA records and have the proxy listen on both families. The proxy-to-application and application-to-backend links may use IPv4; the application handles IPv4 and IPv6 route data independently of the visitor's connection family.

### Backend and management interfaces

The configured bgplgd API is an unauthenticated management interface. Restrict it to the Hextet LG host or its private application network, and restrict BGP port 179 to intended peers. Do not publish either interface broadly. Keep backend URLs and credentials, if any are used by surrounding infrastructure, out of public configuration and logs.

The optional Prometheus metrics listener is separate from the public HTTP listener and is not published by either Compose file. Bind it to a management interface and allow scrapes only from trusted monitoring systems. Do not put it on a public listener or rely on forwarded headers for protection.

Keep runtime YAML, branding assets, and community definitions readable only by the operator and the application. Compose mounts configuration read-only; preserve that property in other deployment methods.

Application security properties and trust boundaries are described in the [architecture guide](ARCHITECTURE.md). They do not secure a deployment by themselves. Proxy configuration, TLS certificates, firewall policy, container runtime, host patching, dependency and image vulnerability status, and the reachability of backend and metrics interfaces remain deployment responsibilities. Test those controls in the actual environment.

### Optional DNS enrichment

ASN descriptions query Team Cymru DNS, and PTR enrichment uses DNS reverse lookups. Enabling either feature discloses eligible visible ASNs or next-hop addresses to the configured resolver and the relevant DNS operators. Lookups are asynchronous and bounded. Status endpoints read cached results only and do not initiate lookups. Both enrichment features are optional and disabled by default.
