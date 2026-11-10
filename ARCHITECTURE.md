# Architecture

Hextet LG is a single Go process. `cmd/hextet-lg` loads and validates YAML, derives a soft memory limit from the configured budgets, initializes fixed backend clients from that configuration, and serves the embedded interface. `internal/web` validates public search values, fans out to selected configured backends with bounded concurrency, and renders HTML templates. It does not proxy arbitrary URLs and stores no route data on disk.

## Query path

```text
public GET URL ──► bgp.ParseQuery ──► Query.Normalize (validate, canonicalize, exclusivity)
                                          │
                  per selected backend:   ▼
            cache.Do(backendID|Query.Key) ──hit──► shared read-only *BackendResult
                  │ miss / join in-flight query
                  ▼
        route-query slot (all route searches; max_concurrent_queries, instance-wide)
                  ▼
        Client.Search: one GET /rib[/in|/out]?<filters> (net/url) with a context deadline
                  ▼
        boundedReader (response limit) ─► json.Decoder, one route object at a time
                  ▼
        parseRoute ─► visibility policy ─► retained-memory check ─► []Route  (+ Completion: status, reason, counts, timings)
                  ▼
        paginate / writeText ─► HTML templates, or streamed text/plain with completion headers
```

`internal/bgp/query.go` is the single definition of the supported filters (the same names as bgplgd's `/rib` parameters), their value grammars (standard, extended and large communities stay distinct; IPv4 and IPv6 addresses are validated and canonicalized with `net/netip`) and the rules bgplgd would otherwise resolve silently: exclusive groups, flags that need a prefix, and reserved legacy RIB endpoint choices. It also defines the canonical public URL pairs, so parsing and URL generation round-trip.

## Streaming decode and completion

`internal/bgp/stream.go` decodes the bgpctl document (`{"rib":[...]}`; an empty result has no `rib` key) with `encoding/json.Decoder` while the HTTP body is still arriving. Only complete route objects reach the sink; an unfinished object is discarded. The enclosing document must close and nothing may follow it before the result is `complete`; anything else is `partial` (at least one complete route) or `failed`, with a reason: `timeout`, `response_limit`, `memory_limit` (the per-query retention limit), `capacity` (the instance-wide budget was exhausted by other work), `connection_closed`, `invalid_json`, `backend_error`, `canceled` or `busy`. `Reason.Transient()` (`capacity`, `busy`, `canceled`) marks conditions that describe this instance's momentary state rather than the query. The size limit is enforced by a reader wrapper while reading; retained memory is estimated per route and bounded separately because decoded routes need more RAM than their JSON. Raw route JSON is kept compacted (one copy) for the diagnostic view. Route address family is derived from the prefix only. bgplgd answers rejected queries with an empty 200 body, which is classified as a failure, not as "no routes".

Neighbor navigation uses the address supplied by each backend and the source backend ID to construct canonical route URLs. It adds no address-family filter and runs through the same search and visibility pipeline. Route records retain the supplied description separately for compact table tooltips and expanded details; missing descriptions are not supplemented by backend requests. Links use ordinary navigation, including from HTMX result fragments, so the destination form and browser history reflect the new search.

The startup-compiled, immutable route visibility policy checks the destination before reservation and retention; excluded routes never enter a flight or cache. Counts reflect visible routes. When a complete upstream route was decoded but excluded, an interrupted document is still partial, even with zero visible routes; an entirely empty HTTP body remains a failure. If configured, the policy fails closed for unparseable or IPv4-mapped route destinations. Restart to change rules. This is a presentation disclosure control, not BGP policy or protection for direct access to the backend API. Neighbor and summary information is outside its scope.

Each backend carries its own `Completion`; statuses are summarized but never merged. Failures do not discard other backends' results.

## Concurrency, cache and memory

- Per request, backends are queried in parallel up to `max_concurrent_backends`.
- All route queries take one of `max_concurrent_queries` slots shared by the whole instance; the bounded admission queue waits at most `queue_wait` or half the backend timeout. Nothing is retried.
- `internal/web/cache.go` is a short-lived result cache (default 20 s) with a global memory budget, fixed TTL plus timer-driven release, expired-then-LRU eviction, and single-flight de-duplication cancelled only when every waiter leaves. It stores only validated complete results and partial results with a deterministic reason (never failures, and never `capacity`, `busy` or `canceled` partials: `skipReason` decides from the typed reason, so a transient overload is not replayed for the TTL after capacity recovers), shares results read-only, and is process memory, not persistence. Pagination uses it so that paging does not re-run a 10-second query per page; OpenBGPD offers no server-side pagination.
- A process-wide budget reserves estimated route bytes incrementally before retention. Each dataset has one ref-counted lease: the flight, cache and each consumer own references, and eviction or expiry releases only the cache reference. The last owner releases the aggregate reservation. The cache budget is a sublimit, not a separate pool of route memory.
- A bounded work gate covers route and overview requests through response completion, including cache hits and slow writers. Overview bodies have a 1 MiB cap and reserve 128 MiB per selected backend against the same aggregate budget. The Community Explorer and health endpoints bypass this gate. Waiting is bounded and returns 503 with Retry-After.
- Estimates do not measure decoder temporaries, formatting, actual heap or RSS. The separately derived Go soft memory limit is not a hard ceiling. Container sizing requires headroom and mixed-load measurement.

## Community definitions

`internal/communities` is a small standard-library-only package (plus `yaml.v3`, already a dependency, for the optional metadata file) that parses the legacy NLNOG community text format and answers `Lookup(kind, value)`. It is independent of the web layer and of the BGP client. `web.NewHandler` loads it once when `community_definitions.enabled` is true and stores the immutable `*communities.Set` on the `Handler`; any load problem fails startup. Nothing is loaded when the section is disabled, and there is no fallback data.

- **Compiled form.** Exact definitions go in a map keyed by kind and canonical text. Pattern definitions (ranges, `x`, `nnn`) become a small matcher of literal/range/digit tokens, never a regular expression, and are bucketed by kind, extended label and literal first component, then sorted once by (estimated value-space size, load order). A lookup is a map probe plus a short scan of one or two buckets, with bounded backtracking (at most three `nnn` per definition, components of at most ten digits). Benchmarks show roughly 200 ns and at most one small allocation per lookup with 24,000 definitions.
- **Canonicalisation.** Route values are normalised before matching (leading zeros, extended label case and spacing, OpenBGPD's well-known names such as `NO_EXPORT`), so equivalent spellings share one definition. Standard, large and extended communities are separate namespaces.
- **Enrichment happens at render time.** Templates call `communityTitle kind value` per displayed attribute; descriptions are never stored in `bgp.Route`, results, the cache, flights or the resource budget, so M3 accounting, shared flights and cache admission are untouched. `html/template` escapes titles. Plaintext views and `output=plain` stay raw.
- **Search independence.** `internal/bgp/query.go`, the feature flags, canonical URLs and the visibility policy do not know about definitions.
- **Metadata.** The NLNOG format has no place for the Explorer's category, notes or implementation status/evidence, so an optional `metadata.yaml` in the same directory supplies them per canonical definition key. It is presentational and validated against the loaded definitions.
- **Determinism.** Files load in a fixed order (well-known, then ascending ASN) and ties are broken by that order, never by map iteration. Duplicates are startup errors.

See [docs/COMMUNITIES.md](docs/COMMUNITIES.md) for the format, precedence rules, limits and upstream differences.

## Presentation

The web layer embeds HTML templates and CSS. Search forms work as ordinary GET forms (a simple box plus Advanced Filters, generated from the field table in `form.go`) and use HTMX to replace the results region when the browser can load its runtime; HTMX updates push the canonical URL, and history-restore requests receive full pages. HTML shows 100 routes per page (`paginate` slices without copying). Plain text is rendered from the same decoded routes by a streaming writer whose columns widen to the longest prefix or next hop, with conspicuous warnings for partial or failed backends and `X-Hextet-LG-*` completion headers. Community status is derived from a `routeros:` evidence block in the embedded YAML.

Compose builds a static Go executable in a multistage build (the same Dockerfile GoReleaser uses for released images) and runs it as UID 10001 with a read-only root filesystem. Configuration is a read-only bind mount. OpenBGPD remains an independently managed service reachable through a shared network or explicit address.

### Theming

`internal/theme` compiles the optional `theme` configuration at startup (`config.Validate` for errors, `NewHandler` for the served copy) into one immutable stylesheet of semantic CSS custom properties (`--color-*`, `--status-*`). Operator input is limited to strict `#RRGGBB` values; hover, subtle, readable-link, button-foreground, control-border and status tokens are derived deterministically and checked against WCAG AA, and the CSS is assembled from validated values only. `GET /static/theme.css` serves it from memory with a strong content-hash `ETag` and `no-cache`; templates link every static asset with a `?v=` fingerprint. `static/app.css` is layout only and contains no color literals (a test enforces it). `static/theme-init.js` runs synchronously in `<head>`, after the stylesheets, applying the visitor's `localStorage` choice before first paint; HTMX swaps never touch `<html>`. Status colors (route validity, completeness, warnings) are separate from the accent and not configurable. See [docs/THEMING.md](docs/THEMING.md).

## Current scope

M2 includes route, neighbor, summary and community views, compound server-side route queries, dual-stack IPv4/IPv6 parity, and the large-response, partial-result and pagination remediation. Public GET URLs capture the requested view and are live queries rather than snapshots. Session counts are derived per backend from the returned summary neighbor records; raw JSON preserves version-specific fields. M3.1 adds HTTP, rate, admission, and lifecycle controls. M3.2 adds structured logging, Prometheus metrics, backend health visibility, the cache admission fix and clean disconnect handling (below); M3.3 adds release engineering (below); M4.1 adds configurable route visibility exclusions. M4.2 adds search-only community feature flags, Loc-RIB-only queries and collapsible advanced filters with explicit prefix scopes. Flags never remove route attributes or Community Explorer. Bare addresses retain longest-prefix-match behavior. Visibility applies before retention in every search mode. M4.4 moves community descriptions out of the code into operator-supplied NLNOG-format files (below); the application contains no operator-specific community data.

## Page generation timing (M4.8)

`protect()` records one monotonic `start` per request in `reqInfo`; the access log, the HTTP duration histogram and the footer all measure from it, so there is no second clock. The footer calls the `pageTiming` template function when the `timing` template is executed. HTML responses are written straight from `html/template` to the response writer (no buffering), and the footer is the last thing executed, so that single read covers everything the handler and the template did before it, including OpenBGPD, cache and shared-flight waits, and it costs one `time.Since`. Asynchronous ASN/PTR work is never awaited, so it is excluded by construction. Response-write stalls for pages larger than the socket buffers are the one non-generation cost that can be included; the page size is bounded by pagination, and buffering a page only to subtract that would change the existing streaming and failure semantics (`renderFailed`), so it is documented rather than removed.

The footer lives outside the HTMX `#results` target. For HTMX fragments (`HX-Request` without `HX-History-Restore-Request`) `Page.Fragment` is set and `results.html` ends with the same `timing` template rendered with `hx-swap-oob="true"`, which HTMX applies to the footer element with the same id in the same swap as the results. A timing that cannot be computed renders as a hidden, empty element in a fragment (clearing a stale value) and as nothing in a full page. `features.page_timing` gates the template; non-HTML responses never involve it. Formatting is `FormatGenerated` in `internal/web/timing.go`.

## Session views (M4.7)

`/neighbors`, `/summary` and `/rtr` share one handler (`Handler.overview`): the same admission, per-request memory lease, bounded backend fan-out, metrics and logging, but separate templates and decoders. They are registered only when `features.bgp_neighbors` (neighbors, summary) or `features.rtr_sessions` (rtr) is on, and the navigation template reads the same flags, so a disabled view has neither a link nor a route. The flags are independent.

`bgp.Client.RTR` queries the fixed `/rtr` path and returns an `RTRResult` whose state is one of sessions, none, unsupported (HTTP 404), unavailable (transport/HTTP error) or malformed (empty body, bad JSON/structure, oversize); it has no error return, so callers cannot lose the distinction. The body is bounded at 1 MiB and 256 sessions, text is cleaned and bounded, and every field is optional. There is no capability cache: the request is the probe. `bgp.Neighbor` gained the optional `/neighbors` fields (group, BGP ID, update/message breakdown, last errors, negotiated address families); the `session` block is decoded best-effort so an unexpected shape cannot break the page. `internal/web/sessions.go` turns both into presentation rows containing only reported values. Validator addresses are omitted from RTR rows unless `features.rtr_session_addresses` is set, and no raw RTR JSON is rendered. See [docs/SESSION_VISIBILITY.md](docs/SESSION_VISIBILITY.md).

## Public request lifecycle

A bounded per-peer token bucket precedes handlers; forwarded identity headers are ignored. Request sizes and parameter counts are checked before routing. Response security headers apply globally and embedded scripts are external assets. `/healthz` is independent of backends; `/readyz` becomes unavailable on shutdown. Shutdown (`Handler.Shutdown`, used by `main`) stops admission, drains HTTP requests for a bounded grace period, then closes the remaining connections, which cancels their request contexts and with them every shared backend query nobody still waits for, and drops cache references. Each response write also carries a 15-second deadline, so a client that stops reading releases its renderer slot and result lease on its own. A single periodic worker expires idle limiter buckets and stops on shutdown.

## Deployment separation

Standalone development uses the Git-ignored `configs/config.yaml` on port 7000. Compose mounts the separate Git-ignored `configs/config.docker.yaml` read-only and listens on port 8080. Both files contain private operator configuration and are excluded from the build context; the Docker file must be readable by the non-root container. The default 40-second application shutdown grace fits within the Compose 45-second stop grace; a longer configured grace requires a corresponding Compose adjustment.


## Observability (M3.2)

```text
request ─► protect (request id, active count, respWriter) ─► guard (limits, rate limit, headers) ─► mux ─► handlers
                 │                                                                                      │
                 └─ finishRequest: status class, client outcome, HTTP metrics, one access-log record      └─ query(): backend metrics + health + one record
```

- **Logging** (`internal/obs`, `internal/web/observe.go`): `log/slog`, JSON by default, level and format from `logging`. `protect` assigns a random request id (never client supplied), carries a `reqInfo` scratch record in the context and writes one `request` record when the request ends; `Handler.query` writes one `backend query` record. Levels are chosen centrally in `finishRequest`; routine traffic is `info`, health/static/429 `debug`, overload and partial backend results `warn`, client departures `info`. Only fixed route names, IDs, counts and durations are logged.
- **Metrics** (`internal/web/metrics.go`): a private `prometheus.Registry` per `Handler`. HTTP and backend instruments are recorded where events happen. Resource, work-gate and cache metrics are produced by one `resourceCollector` that reads the authoritative state at scrape time (`resultBudget.usage/denied`, `workGate.rejected`, `resultCache.snapshot` and its atomic counters), so there is no second accounting and a scrape is O(1). `Handler.MetricsHandler()` is not part of `Routes()`: `cmd/hextet-lg` serves it from a separate, optional listener (`metrics.enabled`, default off, validated not to share a port with the public listener). Labels come only from fixed sets and configured backend IDs.
- **Backend health**: `recordBackend` updates counters and a per-backend `backendHealth` (last success, last backend-side failure and its reason) from real traffic only; nothing polls. Local limits and cancellations are deliberately not backend failures.
- **Response handling**: every response passes through `respWriter`, which applies the per-write deadline, remembers whether the header is committed and the first write error, and swallows a second `WriteHeader`. `renderFailed` answers 500 only before the first byte; after it, a write failure means the client is gone (classified `disconnected`/`canceled`, quiet, status class `client_closed`), and a template failure aborts the connection with `http.ErrAbortHandler` so a truncated page cannot look complete. Leases and work slots are released by the handlers' deferred calls on every path.
- **Shutdown**: `Handler.Shutdown` logs initiation, admission closing with the outstanding-work counters, a grace-expiry warning, and the final counts with duration. The application grace (40 s) must stay below the Compose `stop_grace_period` (45 s).

## Release engineering (M3.3)

```text
git tag vX.Y.Z ─► release.yaml: validate-tag ─► ci.yaml (go, docker, goreleaser jobs) ─► GoReleaser
                                                                                          │
   internal/version ◄── -ldflags (Version, ShortCommit, Date) ── build linux/{amd64,arm64} ┤ static, CGO off
                                                                                          ├─► tar.gz archives + checksums.txt ─► GitHub Release
                                                                                          └─► dockers_v2 (buildx, Dockerfile BINARY_SOURCE=prebuilt) ─► GHCR manifest list
```

- **Single source of version**: the Git tag. `internal/version` holds link-time variables (`Version`, `Commit`, `Date`) and falls back to the module/VCS information the Go toolchain embeds, then to `dev`/`unknown`. `hextet-lg version` (handled in `main` before flag parsing, no CLI framework), the `listening` log record, the OCI labels and `hextet_lg_build_info{version,revision,go_version}` all read the same values. The build-info series has constant labels fixed per build, so cardinality is one.
- **One runtime image design**: the `Dockerfile` defines the runtime stage once; `BINARY_SOURCE` chooses between compiling from source (`compiled`, cross-compiling on the build host so no emulation is needed) and the binary GoReleaser built (`prebuilt`), so a released image contains the archive's executable.
- **GoReleaser owns the release** (builds, archives, checksums, GitHub Release, image build, tags and manifest push via `dockers_v2`); the workflow only supplies Go, Buildx and the GHCR login, and records the pushed digest. `latest` is templated to exist only for non-prerelease, non-snapshot builds.
- **Deployment**: `compose.yaml` takes `HEXTET_LG_IMAGE`; `scripts/deploy.sh` wraps native Compose commands to preserve the running image under an immutable local tag, verify health, readiness, metrics isolation and log format, and roll back. It never reads or changes runtime configuration.

ASN enrichment is independent of BGP route retention and shared flights.
When enabled, page rendering schedules eligible displayed ASNs into a bounded
worker queue. Team Cymru TXT lookups use the system resolver with a timeout;
results live in a separate bounded cache. A rate-limited cache-only status
endpoint supports a finite sequence of browser updates to native titles, and
cannot initiate DNS lookups. Optional cache persistence uses atomic replacement
in an explicitly writable directory; it is disabled by default.

## Next-hop reverse DNS

Optional PTR enrichment uses independent bounded workers and an in-memory cache. Only addresses in rendered, visibility-filtered route table pages are scheduled, and the cache-only status endpoint shares the ASN frontend update lifecycle. DNS never blocks rendering or changes route results. See [PTR enrichment](docs/PTR_ENRICHMENT.md).
