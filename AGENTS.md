# Repository guidance

## Go conventions

- Use the standard module `github.com/HextetSystems/hextet-lg`; keep dependencies in `go.mod`/`go.sum` and do not add vendoring or `go.work`.
- Keep dependencies small. Prefer Go standard library packages for HTTP, URL handling, JSON, templates, and validation.
- Run `gofmt` on Go files and validate with `go vet ./...` and `go test ./...`.

## Project layout

- `cmd/hextet-lg`: CLI and process lifecycle.
- `internal/config`: strict YAML decoding and startup validation.
- `internal/bgp`: OpenBGPD HTTP transport types, native query construction, and client.
- `internal/web`: handlers, embedded templates, and static assets.
- `internal/version`: build identity (link-time `Version`/`Commit`/`Date` with toolchain fallbacks).
- `.goreleaser.yaml`, `.github/workflows`: release automation and CI. `scripts/deploy.sh`: upgrade/rollback wrapper. `docs/RELEASING.md`: release and deployment guide.
- `configs`: example YAML configuration.

## Frontend embedding

- Keep templates and static assets beneath `internal/web`, beside `embed.go`.
- Use package relative embed patterns such as `//go:embed templates/* static/*`.
- Do not reference parent directories in `go:embed` patterns. Render with `html/template`; HTMX may enhance standard forms.

## Backend API conventions

- Use `GET /rib` (and `/rib/in`, `/rib/out`) with bgplgd's native filters, combined in one request built with `net/url`: `prefix`, `as` (source-AS lookup), `community`, `ext-community`, `large-community`, `neighbor`, `group`, `af`, `rib`, `ovs`, `avs`, `best`, `error`, `filtered`, `invalid`, `leaked`, `all`, `or-shorter`. `internal/bgp/query.go` is the single place that defines, validates and canonicalizes them.
- Each filter takes one value; never send repeated parameters or treat them as OR. Reject (never silently drop) invalid values, unknown parameters and combinations bgplgd would silently resolve: `neighbor`/`group`, the route-state flags, and `all`/`or-shorter` are exclusive; `all`/`or-shorter` need `prefix`; `rib` is Loc-RIB only. Keep standard, extended and large communities distinct.
- Do not implement generic AS-path matching, customer cones, or download a whole RIB for local filtering.
- Support IPv4 and IPv6 identically: validate with `net/netip`, never restrict a query to IPv4 implicitly (send `af` only when requested), derive a route's family from its prefix (never from next hop or neighbor), and avoid fixed-width or IPv4-only assumptions.
- Respect request contexts, configured timeouts, concurrency bounds, and response byte limits.
- Keep API transport types separate from presentation models and preserve multiple routes per prefix.
- Decode `/rib` responses as a stream (`encoding/json.Decoder`); never read the whole body into memory. Only a fully decoded route object may be kept, and a response is `complete` only after the whole document (including its closing delimiters and the absence of trailing data) validated. Interrupted queries keep their complete routes as `partial` with a reason; with no routes they are `failed`. Never present partial or failed results as complete or as "no matching routes", and keep each backend's status separate.
- bgplgd answers a query bgpctl rejected with HTTP 200 and an empty body; treat that as a failure, not as an empty result.
- Enforce the response-size limit while reading, bound retained decoded memory separately, and bound expensive (non-prefix) upstream queries across the whole instance. Never retry large interrupted queries.
- The only cache is the short-lived, memory-bounded in-process result cache (validated complete/partial results only, never failures); do not add Redis, a database, a queue or other persistent storage, and never describe cached results as snapshots.
- Keep route `valid`, RPKI `ovs`, and ASPA `avs` distinct.
- Backend URLs come only from validated YAML configuration, never public request parameters.

## Configuration files

- Commit `configs/config.example.yaml` as the complete, documented example configuration.
- Keep `configs/config.yaml` and `configs/config.docker.yaml` local and ignored by Git; do not commit runtime backend addresses.
- Operators create runtime configuration with `cp configs/config.example.yaml configs/config.yaml` and then set backend URLs.
- Backend URLs are configured exclusively through validated YAML, never public request parameters.
- Every `looking_glass` setting is optional with a documented default (30s timeout, 128 MiB response, 256 MiB retained, 100 per page, 2 concurrent large queries, 20s cache); keep existing configurations working and document new settings in the example file. Backends may override `query_timeout`.
- Standalone development uses `configs/config.yaml` on port 7000. Docker Compose uses the separate local `./configs/config.docker.yaml` on port 8080 and mounts it read-only at `/etc/hextet-lg/config.yaml`; the application loads that path in the container.

## Docker

- Docker Compose is the primary deployment method; do not bundle OpenBGPD in this stack.
- Keep runtime non-root, configuration read-only, root filesystem read-only where practical, and expose port 8080.
- A different Compose project requires a shared Docker network or another reachable backend address; container localhost is not the host or peer service.

## Milestones

- M3.1 and M3.2 are accepted and M3.3 (release engineering) is in scope by explicit request; do not begin M4 without an explicit request.

## Public URLs

- Preserve bookmarkable GET URLs: `/` uses `type`+`q` (single prefix, source-AS or Large Community search) or the named filters above, plus `endpoint`, `backend`, `view` and `page`; `/neighbors` and `/summary` use `backend`; `/communities` uses `q` and `category`. Backend values are stable IDs or `all`, never addresses. Existing simple `type`/`q` URLs must keep working.
- Emit canonical URLs: single-filter simple searches in the `type`/`q` form, compound searches with filters in the fixed order of `bgp.StringFilterKeys` then `bgp.FlagFilterKeys`, then `backend`, `view`, `page` (omitted for page 1). Redirect form noise (empty fields) once; for HTMX set `HX-Push-Url`; serve a full page for `HX-History-Restore-Request`.
- Route views are `table`, `text`, and `detail`; `output=plain` requests text/plain for text views and returns all routes unless `page` is given. Plain responses carry `X-Hextet-LG-Completeness`, `X-Hextet-LG-Routes` and `X-Hextet-LG-Backends`, and use HTTP 400/404/502/503/504 only for invalid, out-of-range or fully failed requests (partial = 200 plus headers and an in-body warning). Links are live queries, not snapshots. Keep ordinary navigation functional without JavaScript and preserve HTMX history.

## Communities

- `internal/web/communities.yaml` stays the human-editable reference. Status is derived from its `routeros:` evidence block (Implemented = in a community list, routing filter or configured pattern; Documented/planned = README only; Unresolved = declared insufficient evidence). Do not invent meanings, and do not claim live verification.

## Public deployment controls

- Preserve bounded request parsing, direct-peer rate limiting, security headers, bounded route-query admission, liveness/readiness, and graceful shutdown. Never trust forwarded identity headers without explicitly configured trusted proxies.
- Memory budgets are estimates, not RSS limits; each dataset must remain accounted for while any cache, flight, or renderer owns it. Document changes to operational defaults and preserve old configurations.

## Observability

- Log with `log/slog` (JSON default, `logging.level`/`logging.format`). Never log query strings, prefixes, ASNs, headers, credentials, client addresses, backend URLs or response bodies; log fixed route names and configured backend IDs only. Request IDs are generated, never accepted from clients.
- Prometheus metrics are optional (`metrics.enabled`, default false) and served only by the separate `metrics.listen` listener (`Handler.MetricsHandler`, never in `Routes()`). Do not publish the metrics port in Compose, and do not rely on forwarded headers to protect it. Metric labels come only from fixed sets and configured backend IDs (plus the constant per-build labels of `hextet_lg_build_info`).
- Resource, work-gate and cache metrics read the authoritative counters at scrape time (O(1)); do not add a second accounting or scan the cache per scrape.
- Cache admission is decided from the typed `bgp.Reason` (`skipReason` in `internal/web/cache.go`), never from message text: partial results caused by `capacity`, `busy` or `canceled` are never cached. The per-query `memory_limit` and the aggregate `capacity` stay distinct reasons.
- After the response header is committed, never call `http.Error`/`WriteHeader`; use `Handler.renderFailed`. Keep `server.shutdown_grace` below the Compose `stop_grace_period`.

## Release engineering

- GoReleaser Community Edition (`.goreleaser.yaml`, `version: 2`, `dockers_v2`) is the only release tool; do not add a parallel release script or a second image pipeline. Versions come from Git tags only; never store a version number in a file. Do not create tags, push, publish releases or images, or replace the running production container without explicit authorization.
- One Dockerfile defines the runtime image; `BINARY_SOURCE` selects `compiled` (source) or `prebuilt` (GoReleaser). Keep it non-root (UID 10001), read-only-root compatible, with the `HEALTHCHECK` and no RUN step that would need emulation to build arm64.
- `latest` exists only for stable (non-prerelease, non-snapshot) releases. Production and rollback use image digests or preserved immutable local tags, never `hextet-lg:local`. `scripts/deploy.sh` must keep validating before replacing, never touch `configs/`, never prune or delete images, and never act without an explicit subcommand and image.
- Archives use an explicit file allow-list; runtime configuration (`configs/config.yaml`, `configs/config.docker.yaml`) is never packaged, committed or baked into an image. Pin third-party GitHub Actions to commit SHAs, keep workflow permissions least-privilege, and let only `release.yaml` publish (after `ci.yaml` passes).
