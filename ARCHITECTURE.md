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

`internal/bgp/query.go` is the single definition of the supported filters (the same names as bgplgd's `/rib` parameters), their value grammars (standard, extended and large communities stay distinct; IPv4 and IPv6 addresses are validated and canonicalized with `net/netip`) and the rules bgplgd would otherwise resolve silently: exclusive groups, flags that need a prefix, and the Loc-RIB-only `rib` parameter. It also defines the canonical public URL pairs, so parsing and URL generation round-trip.

## Streaming decode and completion

`internal/bgp/stream.go` decodes the bgpctl document (`{"rib":[...]}`; an empty result has no `rib` key) with `encoding/json.Decoder` while the HTTP body is still arriving. Only complete route objects reach the sink; an unfinished object is discarded. The enclosing document must close and nothing may follow it before the result is `complete`; anything else is `partial` (at least one complete route) or `failed`, with a reason: `timeout`, `response_limit`, `memory_limit` (the per-query retention limit), `capacity` (the instance-wide budget was exhausted by other work), `connection_closed`, `invalid_json`, `backend_error`, `canceled` or `busy`. `Reason.Transient()` (`capacity`, `busy`, `canceled`) marks conditions that describe this instance's momentary state rather than the query. The size limit is enforced by a reader wrapper while reading; retained memory is estimated per route and bounded separately because decoded routes need more RAM than their JSON. Raw route JSON is kept compacted (one copy) for the diagnostic view. Route address family is derived from the prefix only. bgplgd answers rejected queries with an empty 200 body, which is classified as a failure, not as "no routes".

The startup-compiled, immutable route visibility policy checks the destination before reservation and retention; excluded routes never enter a flight or cache. Counts reflect visible routes. When a complete upstream route was decoded but excluded, an interrupted document is still partial, even with zero visible routes; an entirely empty HTTP body remains a failure. If configured, the policy fails closed for unparseable or IPv4-mapped route destinations. Restart to change rules. This is a presentation disclosure control, not BGP policy or protection for direct access to the backend API. Neighbor and summary information is outside its scope.

Each backend carries its own `Completion`; statuses are summarized but never merged. Failures do not discard other backends' results.

## Concurrency, cache and memory

- Per request, backends are queried in parallel up to `max_concurrent_backends`.
- All route queries take one of `max_concurrent_queries` slots shared by the whole instance; the bounded admission queue waits at most `queue_wait` or half the backend timeout. Nothing is retried.
- `internal/web/cache.go` is a short-lived result cache (default 20 s) with a global memory budget, fixed TTL plus timer-driven release, expired-then-LRU eviction, and single-flight de-duplication cancelled only when every waiter leaves. It stores only validated complete results and partial results with a deterministic reason (never failures, and never `capacity`, `busy` or `canceled` partials: `skipReason` decides from the typed reason, so a transient overload is not replayed for the TTL after capacity recovers), shares results read-only, and is process memory, not persistence. Pagination uses it so that paging does not re-run a 10-second query per page; OpenBGPD offers no server-side pagination.
- A process-wide budget reserves estimated route bytes incrementally before retention. Each dataset has one ref-counted lease: the flight, cache and each consumer own references, and eviction or expiry releases only the cache reference. The last owner releases the aggregate reservation. The cache budget is a sublimit, not a separate pool of route memory.
- A bounded work gate covers route and overview requests through response completion, including cache hits and slow writers. Overview bodies have a 1 MiB cap and reserve 128 MiB per selected backend against the same aggregate budget. Locally embedded communities and health endpoints bypass this gate. Waiting is bounded and returns 503 with Retry-After.
- Estimates do not measure decoder temporaries, formatting, actual heap or RSS. The separately derived Go soft memory limit is not a hard ceiling. Container sizing requires headroom and mixed-load measurement.

## Presentation

The web layer embeds HTML templates and CSS. Search forms work as ordinary GET forms (a simple box plus Advanced Filters, generated from the field table in `form.go`) and use HTMX to replace the results region when the browser can load its runtime; HTMX updates push the canonical URL, and history-restore requests receive full pages. HTML shows 100 routes per page (`paginate` slices without copying). Plain text is rendered from the same decoded routes by a streaming writer whose columns widen to the longest prefix or next hop, with conspicuous warnings for partial or failed backends and `X-Hextet-LG-*` completion headers. Community status is derived from a `routeros:` evidence block in the embedded YAML.

Compose builds a static Go executable in a multistage build (the same Dockerfile GoReleaser uses for released images) and runs it as UID 10001 with a read-only root filesystem. Configuration is a read-only bind mount. OpenBGPD remains an independently managed service reachable through a shared network or explicit address.

## Current scope

M2 includes route, neighbor, summary and locally embedded community views, compound server-side route queries, dual-stack IPv4/IPv6 parity, and the large-response, partial-result and pagination remediation. Public GET URLs capture the requested view and are live queries rather than snapshots. Session counts are derived per backend from the returned summary neighbor records; raw JSON preserves version-specific fields. M3.1 adds HTTP, rate, admission, and lifecycle controls. M3.2 adds structured logging, Prometheus metrics, backend health visibility, the cache admission fix and clean disconnect handling (below); M3.3 adds release engineering (below); M4.1 adds configurable route visibility exclusions.

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
