# Repository guidance

## Go conventions

- Use the standard module `github.com/HextetSystems/hextet-lg`; keep dependencies in `go.mod`/`go.sum` and do not add vendoring or `go.work`.
- Keep dependencies small. Prefer Go standard library packages for HTTP, URL handling, JSON, templates, and validation.
- Run `gofmt` on Go files and validate with `go vet ./...` and `go test ./...`.

## Project layout

- `cmd/hextet-lg`: CLI and process lifecycle.
- `internal/config`: strict YAML decoding and startup validation.
- `internal/bgp`: OpenBGPD HTTP transport types, native query construction, and client.
- `internal/communities`: operator-supplied community definitions (legacy NLNOG text format): parser, matcher, loader. No web or BGP dependencies.
- `internal/web`: handlers, embedded templates, and static assets. `sessions.go` builds the BGP neighbor and RTR presentation rows.
- `internal/theme`: strict `#RRGGBB` theme configuration, deterministic token derivation, contrast audit and the generated stylesheet. No web or BGP dependencies.
- `internal/version`: build identity (link-time `Version`/`Commit`/`Date` with toolchain fallbacks).
- `.goreleaser.yaml`, `.github/workflows`: release automation and CI. `scripts/deploy.sh`: upgrade/rollback wrapper. `docs/RELEASING.md`: release and deployment guide.
- `configs`: example YAML configuration. `examples/communities`: example community definitions (data, not defaults). `docs/COMMUNITIES.md`: definitions guide. `docs/SESSION_VISIBILITY.md`: BGP neighbor and RTR session guide.

## Frontend embedding

- Keep templates and static assets beneath `internal/web`, beside `embed.go`.
- Use package relative embed patterns such as `//go:embed templates/* static/*`.
- Do not reference parent directories in `go:embed` patterns. Render with `html/template`; HTMX may enhance standard forms.

## Backend API conventions

- Use `GET /rib` with bgplgd's native filters, combined in one request built with `net/url`: `prefix`, `as` (source-AS lookup), `community`, `ext-community`, `large-community`, `neighbor`, `group`, `af`, `rib`, `ovs`, `avs`, `best`, `error`, `filtered`, `invalid`, `leaked`, `all`, `or-shorter`. `internal/bgp/query.go` is the single place that defines, validates and canonicalizes them.
- Each filter takes one value; never send repeated parameters or treat them as OR. Reject (never silently drop) invalid values, unknown parameters and combinations bgplgd would silently resolve: `neighbor`/`group`, the route-state flags, and `all`/`or-shorter` are exclusive; `all`/`or-shorter` need `prefix`; reserved legacy `rib=in|out|local` values are rejected. Keep standard, extended and large communities distinct.
- Do not implement generic AS-path matching, customer cones, or download a whole RIB for local filtering.
- Support IPv4 and IPv6 identically: validate with `net/netip`, never restrict a query to IPv4 implicitly (send `af` only when requested), derive a route's family from its prefix (never from next hop or neighbor), and avoid fixed-width or IPv4-only assumptions.
- Respect request contexts, configured timeouts, concurrency bounds, and response byte limits.
- Keep API transport types separate from presentation models and preserve multiple routes per prefix.
- Decode `/rib` responses as a stream (`encoding/json.Decoder`); never read the whole body into memory. Only a fully decoded route object may be kept, and a response is `complete` only after the whole document (including its closing delimiters and the absence of trailing data) validated. Interrupted queries keep their complete routes as `partial` with a reason; with no routes they are `failed`. Never present partial or failed results as complete or as "no matching routes", and keep each backend's status separate.
- bgplgd answers a query bgpctl rejected with HTTP 200 and an empty body; treat that as a failure, not as an empty result.
- Enforce the response-size limit while reading, bound retained decoded memory separately, and bound expensive (non-prefix) upstream queries across the whole instance. Never retry large interrupted queries.
- The route result cache is the short-lived, memory-bounded in-process result cache (validated complete/partial results only, never failures); do not add Redis, a database, or persistent route storage, and never describe cached results as snapshots.
- Apply the immutable startup-compiled route visibility policy before retaining or caching decoded routes; counts describe visible routes, while interruptions preserve their underlying completion status.
- Keep route `valid`, RPKI `ovs`, and ASPA `avs` distinct.
- Backend URLs come only from validated YAML configuration, never public request parameters.

## Configuration files

- Commit `configs/config.example.yaml` as the complete, documented example configuration.
- Keep `configs/config.yaml` and `configs/config.docker.yaml` local and ignored by Git; do not commit runtime backend addresses.
- Operators create runtime configuration with `cp configs/config.example.yaml configs/config.yaml` and then set backend URLs.
- Backend URLs are configured exclusively through validated YAML, never public request parameters.
- `community_definitions` is optional and disabled by default (`enabled`, `directory`); keep it generic and document it in the example file. Docker mounts the directory read-only at `/etc/hextet-lg/communities`; no write access.
- Every `looking_glass` setting is optional with a documented default (30s timeout, 128 MiB response, 256 MiB retained, 100 per page, 2 concurrent large queries, 20s cache); keep existing configurations working and document new settings in the example file. Backends may override `query_timeout`.
- Standalone development uses `configs/config.yaml` on port 7000. Docker Compose uses the separate local `./configs/config.docker.yaml` on port 8080 and mounts it read-only at `/etc/hextet-lg/config.yaml`; the application loads that path in the container.

## Docker

- Docker Compose is the primary deployment method; do not bundle OpenBGPD in this stack.
- Keep runtime non-root, configuration read-only, root filesystem read-only where practical, and expose port 8080.
- A different Compose project requires a shared Docker network or another reachable backend address; container localhost is not the host or peer service.

## Milestones

- M3.1 and M3.2 are accepted and M3.3 (release engineering) and M4.1 (route visibility exclusions) and M4.2 (search configuration and UX) and M4.3 (neighbor navigation and compact route tables) and M4.4 (external BGP community definitions) and M4.5 (ASN name enrichment) and M4.5.1 (configurable themes) are in scope by explicit request; M4.6 (next-hop PTR enrichment) and M4.7 (BGP neighbor and RPKI RTR session visibility) and M4.8 (page generation timing) and M4.8.1 (compact Route Explorer layout and dynamic page titles) and M4.8.2 (mobile usability, route presentation and configurable columns) are in scope by explicit request; do not begin M4.9 or later without an explicit request.

## Public URLs

- Preserve bookmarkable GET URLs: `/` uses `type`+`q` (single prefix, source-AS or Large Community search) or the named filters above, plus `backend`, `view` and `page`; `/neighbors`, `/summary` and `/rtr` use `backend`; `/communities` uses `q` and `category`. Backend values are stable IDs or `all`, never addresses. Existing simple `type`/`q` URLs must keep working.
- Emit canonical URLs: single-filter simple searches in the `type`/`q` form, compound searches with filters in the fixed order of `bgp.StringFilterKeys` then `bgp.FlagFilterKeys`, then `backend`, `view`, `page` (omitted for page 1). Redirect form noise (empty fields) once; for HTMX set `HX-Push-Url`; serve a full page for `HX-History-Restore-Request`.
- Neighbor links use the backend-supplied address and source backend ID, without an inferred `af` or unrelated filters. Keep descriptions escaped and supplementary to the visible address.
- Route views are `table`, `text`, and `detail`; `output=plain` requests text/plain for text views and returns all routes unless `page` is given. Plain responses carry `X-Hextet-LG-Completeness`, `X-Hextet-LG-Routes` and `X-Hextet-LG-Backends`, and use HTTP 400/404/502/503/504 only for invalid, out-of-range or fully failed requests (partial = 200 plus headers and an in-body warning). Links are live queries, not snapshots. Keep ordinary navigation functional without JavaScript and preserve HTMX history.

## Communities

- Community descriptions are operator data in the legacy NLNOG text format (`asN.txt`, `well-known.txt`, optional `metadata.yaml`), loaded once at startup from `community_definitions.directory` when `community_definitions.enabled` (default false). Never embed, default or fall back to any operator's definitions (the AS395089 set lives only in `examples/communities`), never download or refresh definitions, and never log definition contents.
- The format is NLNOG-compatible: first-comma split, `#` comments, exact/range/`x` (one digit)/`nnn` (one or more digits) with `$N` substitutions for standard, large and extended communities. Never compile definition text as a regular expression; keep the bounded matcher. Precedence is exact, then smallest value space, then load order (well-known, ascending ASN, line); never depend on map iteration. Malformed, unsupported or duplicate definitions and any unreadable file fail startup with file and line; do not silently drop lines. See `docs/COMMUNITIES.md` before changing the grammar or its documented differences from upstream.
- Definitions are presentation only. They must not change query construction, search, canonical URLs, the `features.*` flags, route visibility or caching, and must not be copied into retained `bgp.Route`, cache or flight data: look them up at render time from the immutable set. Tooltips are escaped native `title` attributes in the route-details HTML only; plaintext views and `output=plain` stay raw. Do not add description search.
- Standard, extended and large communities stay separate kinds. The Community Explorer is generated from the loaded definitions plus the optional `metadata.yaml` (category, notes, status `implemented|documented|unresolved`, evidence), which never affects lookup. "Implemented" means present in the operator's configuration; do not invent meanings and do not claim live verification.

## Session visibility

- `features.bgp_neighbors` (`/neighbors`, `/summary`) and `features.rtr_sessions` (`/rtr`) default to true and are independent; a disabled view is not registered (404) and has no navigation link. The community flags still control search only. `features.rtr_session_addresses` defaults to false: never show RTR validator addresses, ports or local addresses, or raw RTR JSON, unless it is on.
- Use only fields bgpctl really emits (`docs/SESSION_VISIBILITY.md` lists them with their source). RTR comes only from bgplgd `/rtr`; never add SSH, shell, control-socket access, CLI-text scraping or a substitute such as `/sets` or ROV results. `/summary` JSON has no counters; do not invent them.
- Keep RTR outcomes distinct (`bgp.RTRState`: sessions, none, unsupported, unavailable, malformed). Never show "no sessions" unless the backend answered with a valid empty document. Absent values are dashes, never zero, and an established session without session ID and serial is not "good".
- `rtr` is a fixed route and backend-operation label; an unsupported backend is a failed request with no failure reason. Do not add capability caches, per-session goroutines or polling. Sessions of different backends are never merged.

## Page generation timing

- `features.page_timing` (default true) only controls the footer display; HTTP metrics and the access log are never conditional on it. The start is `reqInfo.start` set by `protect`; the single read happens in the `pageTiming` template function at footer execution. Do not add a second clock, buffer HTML responses to obtain the figure, or put timing into JSON, CSV, `output=plain`, metrics or headers.
- HTMX fragments carry the figure as a trailing `hx-swap-oob` element so it is replaced together with the results; keep it top-level, singular and absent from full pages. Omit the indicator (full page) or hide it (fragment) instead of showing an unmeasured value. History restoration may show the original request's figure.

## Route Explorer layout and titles

- Order: compact search panel (one control row, a secondary row with Advanced filters, query context, Copy link, Clear search), one status line per backend directly above its table, the unchanged table, the footer. Do not reintroduce a query-summary panel, per-backend heading cards, repeated counts or long explanatory paragraphs; explanations belong in the help popover or Advanced filters.
- Controls keep real labels (visually hidden is fine), the `role="search"` form, collapsed Advanced filters (opened only by validation errors) and the active-filter count. The query context (`context` template) is outside `#results`: fragments replace it with `hx-swap-oob`, singular and top-level, like `#page-timing`; it never repeats the search box value. Backend outcomes are text, never colour only; backends are never merged and a failed one never hides another. The pager exists only for more than one page.
- Every HTML page has exactly one `<title>`, `<subject> · <site>`, built only in `internal/web/title.go` (`siteName` is the single suffix hook for M4.9). Subjects come from the normalized query, never raw input; rejected requests are "Invalid search"; no query strings beyond the bounded canonical primary filter, no backend addresses, advanced filters only counted, user-derived text stripped and bounded. A fragment starts with `<title>` (HTMX applies it); a full page (including history restore) has it only in `<head>`. Add no client-side title code.

## Mobile layout, display settings and history

- The route table is one DOM: phones (up to 700 px) restyle the same `<tbody class="route">` rows as cards with CSS media queries. Do not add a second (mobile) markup variant, a viewport-detecting script or a second set of enrichment targets, IDs or Raw JSON payloads. Desktop row height with both disclosures collapsed must not grow.
- Route details and Raw JSON are two independent native `<details>` in one `.disclosures` row (also on the neighbor pages). `display.raw_json` (default false) gates every Raw JSON presentation; when off, render nothing and embed no payload. Never add an endpoint that serves raw backend data. `bgp.Route.Raw` retention and accounting do not depend on the setting.
- `display.route_columns` (neighbor false, neighbor_format name|ip|both, rpki true, aspa true) is presentation only: it must not change the backend request, route parsing, cache keys, filters or visibility. A hidden column's value stays in Route details; header, cells and the details colspan must stay aligned for every combination.
- Neighbor names reuse the M4.6 PTR service (cache, workers, TTLs, dedup, `/ptr-status`); never add a resolver or cache. They share the per-page budget, are scheduled after next hops and only while the Neighbor column is visible with `name`/`both`. The hostname is display text only: links, identity and keys use the backend-supplied address and backend ID.
- Browser history restores from the server (`historyCacheSize:0`), never from HTMX DOM snapshots, so the form always matches the canonical URL. Search-form and theme handlers must stay delegated or be rebound on `htmx:historyRestore`.

## Public deployment controls

- Preserve bounded request parsing, direct-peer rate limiting, security headers, bounded route-query admission, liveness/readiness, and graceful shutdown. Never trust forwarded identity headers without explicitly configured trusted proxies.
- Memory budgets are estimates, not RSS limits; each dataset must remain accounted for while any cache, flight, or renderer owns it. Document changes to operational defaults and preserve old configurations.

## Observability

- Log with `log/slog` (JSON default, `logging.level`/`logging.format`). Never log query strings, prefixes, ASNs, headers, credentials, client addresses, backend URLs or response bodies; log fixed route names and configured backend IDs only. Request IDs are generated, never accepted from clients.
- Prometheus metrics are optional (`metrics.enabled`, default false) and served only by the separate `metrics.listen` listener (`Handler.MetricsHandler`, never in `Routes()`). Do not publish the metrics port in Compose, and do not rely on forwarded headers to protect it. Metric labels come only from fixed sets and configured backend IDs (plus the constant per-build labels of `hextet_lg_build_info`).
- Resource, work-gate and cache metrics read the authoritative counters at scrape time (O(1)); do not add a second accounting or scan the cache per scrape.
- Cache admission is decided from the typed `bgp.Reason` (`skipReason` in `internal/web/cache.go`), never from message text: partial results caused by `capacity`, `busy` or `canceled` are never cached. The per-query `memory_limit` and the aggregate `capacity` stay distinct reasons.
- After the response header is committed, never call `http.Error`/`WriteHeader`; use `Handler.renderFailed`. Keep `server.shutdown_grace` below the Compose `stop_grace_period`.

## Themes

- Every UI color is a semantic CSS custom property defined by the generated `/static/theme.css`; `static/app.css` must contain no color literals, gradients or shadows, templates no inline styles (CSP `style-src 'self'`). Add a token to `internal/theme` rather than a literal.
- Theme configuration accepts only strict `#RRGGBB`, never CSS text; generate CSS only from validated values, once at startup. Derived tokens are deterministic. Contrast shortfalls warn (below 3:1 for primary text fails startup); never silently replace operator colors. See `docs/THEMING.md`.
- Status colors (route `valid`, RPKI `ovs`, ASPA `avs`, completeness, warnings) are fixed, distinct from the single accent, and always paired with a text label. Do not collapse them into the accent.
- The visitor's choice lives only in `localStorage` key `theme` (`light`/`dark`); no cookies or server-side preference. `theme-init.js` stays a synchronous script in `<head>` after the stylesheets. Do not begin M4.9 without explicit authorization.

## Release engineering

- GoReleaser Community Edition (`.goreleaser.yaml`, `version: 2`, `dockers_v2`) is the only release tool; do not add a parallel release script or a second image pipeline. Versions come from Git tags only; never store a version number in a file. Do not create tags, push, publish releases or images, or replace the running production container without explicit authorization.
- One Dockerfile defines the runtime image; `BINARY_SOURCE` selects `compiled` (source) or `prebuilt` (GoReleaser). Keep it non-root (UID 10001), read-only-root compatible, with the `HEALTHCHECK` and no RUN step that would need emulation to build arm64.
- `latest` exists only for stable (non-prerelease, non-snapshot) releases. Production and rollback use image digests or preserved immutable local tags, never `hextet-lg:local`. `scripts/deploy.sh` must keep validating before replacing, never touch `configs/`, never prune or delete images, and never act without an explicit subcommand and image.
- Archives use an explicit file allow-list; runtime configuration (`configs/config.yaml`, `configs/config.docker.yaml`) is never packaged, committed or baked into an image. Pin third-party GitHub Actions to commit SHAs, keep workflow permissions least-privilege, and let only `release.yaml` publish (after `ci.yaml` passes).

- Community feature flags control search only; never strip route attributes or hide the Community Explorer. Searches always use Loc-RIB; reject removed endpoint choices.

## ASN enrichment

- Optional Team Cymru DNS enrichment is descriptive only and separate from route results and shared flights. DNS work is asynchronous, bounded, and limited to visible ASNs. The cache-only status endpoint must never schedule lookups. Persistence is opt-in and requires a writable cache directory. Do not begin M4.9 without explicit authorization.

## PTR enrichment

- PTR lookups are optional, asynchronous and limited to next hops on visible route table pages. Keep the cache memory-only and separate from route and ASN data. The public status endpoint is cache-only. Preserve the shared ASN/PTR frontend lifecycle and do not perform forward confirmation. See `docs/PTR_ENRICHMENT.md`.
