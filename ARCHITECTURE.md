# Architecture

Hextet LG replaces the CGI looking-glass frontend not included in the `openbgpd/openbgpd` Docker image. It is a read-only Go application that queries the image's `bgplgd` API to show routes and session information. Hextet LG and OpenBGPD run separately; route results stay in bounded in-memory caches.

## Components

- `cmd/hextet-lg` loads configuration, constructs the application, and manages listeners and shutdown.
- `internal/config` decodes and validates YAML at startup.
- `internal/bgp` builds and validates native `bgplgd` queries, calls the configured backend endpoints, and decodes responses.
- `internal/web` owns public HTTP routes, request controls, templates, presentation, caching, and integration with enrichment services.
- `internal/asn` and `internal/ptr` provide optional, bounded DNS enrichment. `internal/visibility` holds the route exclusion policy.
- `internal/communities`, `internal/site`, and `internal/theme` handle operator-supplied community definitions, deployment identity and assets, and generated theme stylesheets.
- `internal/obs` and `internal/version` provide logging/observability and build identity.

Templates and static files live under `internal/web` and are embedded in the binary. The interface is server-rendered with Go templates; HTMX enhances ordinary forms and links.

## Request and data flow

```text
GET search URL
    │
    ▼
parse, validate, and canonicalize query
    │
    ├── per configured backend, in parallel
    │       │
    │       ├── result cache / shared in-flight query
    │       └── bounded GET /rib with request deadline
    │                    │
    │                    ▼
    │             size-limited streaming JSON decode
    │                    │
    │             route visibility policy
    │                    │
    │             retained-memory budget
    │
    ▼
paginate and render HTML or streamed plain text
```

Search filters are validated and canonicalized centrally in `internal/bgp/query.go`, including IPv4/IPv6 values and mutually exclusive options. Each configured backend is queried independently; one backend's failure does not hide another's result. Routes are decoded incrementally, and a response is complete only after its full JSON document validates. Complete routes from an interrupted response may be shown as partial, with the reason made visible. Failed or partial responses are never presented as a complete empty result.

The startup-compiled visibility policy is applied before routes are retained, counted, cached, paginated, or passed to enrichment. Route validity, RPKI origin validation, and ASPA validation remain separate fields. `/neighbors` and `/rtr` use the backend's session endpoints; the Neighbors page also uses backend `/summary` data. `/summary` is not a public page. Route maps describe routes on the current page, and plain-text output streams route results with completion headers.

## Resource model

Request parsing, upstream response bodies, route retention, backend fan-out, and concurrent route queries are bounded. The result cache is short-lived, process-local, and memory-bounded; it stores validated results rather than failures. Concurrent identical requests can share an in-flight query. No route data is written to a database or persistent store.

Memory budgets account for retained route data across active queries, shared results, cache entries, and renderers. They are estimates rather than a hard RSS limit. Slow or disconnected clients, cancellation, shutdown, and cache expiry release their associated work and memory reservations.

ASN and PTR enrichment is optional, asynchronous, and separately bounded. It enriches visible page content only; it does not change route queries or results. Status endpoints read cached values and do not initiate DNS lookups. Enabling enrichment can disclose eligible ASNs or addresses to DNS operators.

## Web and security boundaries

Public pages are generated from validated, canonical GET URLs. Forms and links work without JavaScript; HTMX updates results and browser history when available. Templates use `html/template`, static files are embedded and served by exact asset name, and security headers include a restrictive Content Security Policy.

Browser input is bounded and strictly validated. Unknown or repeated parameters are rejected. Backend URLs come only from validated configuration; the backend client requests fixed endpoints and does not follow redirects. Backend, operator, and DNS text remains untrusted display data. Logs avoid query values, addresses, credentials, backend URLs, headers, and response bodies. Forwarded headers do not establish client identity or influence generated URLs.

The public application listener and optional metrics listener are separate. Health and readiness are served by the application; metrics are not registered on the public handler. Deployments should restrict the unauthenticated `bgplgd` API and metrics listener to trusted networks and place public TLS and client-facing controls at a reverse proxy. See [Security and deployment](docs/SECURITY.md).

## Configuration and deployment

Runtime settings are loaded from strict YAML and validated at startup. [`examples/config.example.yaml`](examples/config.example.yaml) documents the supported settings and defaults. Keep runtime configuration and backend addresses local; do not commit them. Hextet LG and OpenBGPD are separate services, connected over a reachable private network or address.

The provided Compose deployment runs the container as a non-root user with read-only configuration and a read-only root filesystem. The public listener is on port 8080; the metrics listener, when enabled, is separate and should not be published publicly. See the [README](README.md), [OpenBGPD collector guide](docs/OPENBGPD.md), and [release and deployment guide](docs/RELEASING.md) for operational details.

## Further reading

- [README](README.md): features, configuration, URLs, and local development.
- [Testing and integration checks](docs/TESTING.md): deterministic tests and opt-in browser/live checks.
- [Community definitions](docs/COMMUNITIES.md), [session visibility](docs/SESSION_VISIBILITY.md), and [PTR enrichment](docs/PTR_ENRICHMENT.md): feature behavior and limitations.
- [Theming](docs/THEMING.md) and [site branding](docs/BRANDING.md): appearance and deployment identity.
