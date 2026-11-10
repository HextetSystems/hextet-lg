# Hextet LG

Hextet LG is a read-only web looking glass for one or more OpenBGPD `bgplgd` HTTP APIs. It is a Go application with Go HTML templates, HTMX form updates, and embedded frontend assets. It has no database or other persistent service.

## Search options

The optional `features.communities` and `features.large_communities` settings default to `true`; `features.extended_communities` defaults to `false`. Set any to `false` to disable that search type. These flags affect searches only: route community attributes remain visible and Community Explorer remains available. Disabled searches are rejected even through direct URLs.

Advanced Filters start collapsed and collapse after a successful search; validation errors open them. The indicator counts active advanced criteria, and expanding the section retains their values. CIDR searches default to exact prefix; select **Include more specifics** (`all=1`) or **Include covering prefixes** (`or-shorter=1`) for other scopes. For example, `/?type=prefix&q=8.8.8.0%2F24&all=1` and `/?type=prefix&q=2001%3Adb8%3A%3A%2F32&or-shorter=1` are shareable links. Bare IPv4 and IPv6 addresses use longest-prefix match and cannot be combined with these scope flags. Route visibility exclusions apply to every mode and enabled community search before results are cached or counted.

Neighbor addresses in the Neighbor Explorer and route tables link to a fresh route search on the originating backend. The search does not infer an address family from the session address, so it includes both families unless a filter is explicitly selected. Route tables show the full address compactly; native tooltips provide available descriptions and, in the Neighbor Explorer, remote ASNs. Expanded route details retain the description. Descriptions are shown only when the backend supplies them; no additional neighbor lookup is made.

## Route visibility exclusions

Optionally configure `route_visibility.exclude.ipv4` and `ipv6` to hide destination routes across all backends and route outputs. No prefixes are excluded by default. For example:

```yaml
route_visibility:
  exclude:
    ipv4:
      - {prefix: 10.0.0.0/8, match: or-longer}
      - {prefix: 192.0.2.0/24, match: exact}
    ipv6:
      - {prefix: "fd00::/8", match: or-longer}
      - {prefix: "2001:db8:1234::/48", match: exact}
```

`exact` hides only the specified destination prefix; `or-longer` also hides contained more-specific destinations. Rules combine with OR. Startup rejects invalid CIDRs, host bits, wrong families, IPv4-mapped IPv6 prefixes, missing fields and unsupported match modes. Duplicate rules are harmless. With any rules configured, unparseable or IPv4-mapped route destinations are also withheld because their visibility cannot be established safely. Changes require an application restart. Filtering happens before retention, shared queries and cache admission, so counts and pages contain visible routes only; incomplete backend responses remain incomplete.

This is an application-level disclosure control and does not change BGP routing policy or what the backend receives. For highly sensitive routes, prevent them from reaching the public looking-glass backend at all. This feature does not make the underlying OpenBGPD API safe for public exposure. Other backend information, such as neighbor and summary data, is not route-filtered.

## Themes

The interface has a restrained light theme (white, light grey, dark grey) and a dark theme (near-black charcoal, layered grey surfaces), each with one accent color. Both are configured by the optional `theme` section; every value is independently overridable, omitted values keep the built-in defaults, and existing configurations need no change. Colors are strict `#RRGGBB` hex only; names, CSS functions, `url()`, `var()` and anything else fail startup. To change only the accent:

```yaml
theme:
  accent: "#0D9488"
```

`theme.default` is `auto` (follow the browser, the default), `light` or `dark`. The theme button stores the visitor's choice in the browser's `localStorage` key `theme`; that overrides the default in that browser only (no cookies, no server state) and is reset by removing the key or clearing site data. The stylesheet is generated once at startup, so edit the YAML and restart. Poor contrast logs a warning (WCAG AA targets); unreadable body text (below 3:1) refuses to start. RPKI/ASPA validity, partial/failed results and warnings keep fixed status colors that are not the accent and are always labelled in text. See [docs/THEMING.md](docs/THEMING.md) for the palettes, derivations, tokens and a manual verification checklist.

## Views and shareable URLs

The Route Explorer (`/`) runs native, server-side OpenBGPD queries. Neighbor Explorer (`/neighbors`) and BGP Summary (`/summary`) show live BGP session data per backend, and RTR Sessions (`/rtr`) shows the RPKI-to-Router sessions between each backend and its validators; each can be switched off independently (see [Session visibility](#session-visibility)). Community Explorer (`/communities`) is an embedded local reference and remains available if backends are down.

Every view is a bookmarkable GET URL. Shared links are **live queries, not snapshots**: re-opening one runs the query again and the routes may have changed. If the short-lived result cache is enabled (see below), a result may be up to `cache_ttl` old and is labelled as cached. URLs never contain backend addresses; `backend` is a stable configured ID or `all`.

| Page | Parameters |
|---|---|
| `/` | search filters (below), `backend`, `view=table\|text\|detail`, `page`, `output=plain` |
| `/neighbors`, `/summary`, `/rtr` | `backend` |
| `/communities` | `q`, `category` |

Simple searches keep their original form: `/?type=prefix\|asn\|large-community&q=...&backend=...&view=...`. Prefix searches with a scope flag also use `type` and `q`. Other compound searches name each filter, for example `/?as=15169&large-community=395089%3A0%3A6&af=ipv6&best=1&backend=all&view=table&page=2`. The application emits parameters in a canonical order (filters in the order of the table below, then `backend`, `view`, `page`); both forms are accepted as input and mixed. A form submission that carries empty fields is redirected once to the canonical URL, and HTMX updates push the canonical URL into browser history. Back/forward and fresh-session opening work without any stored state, and ordinary navigation works without JavaScript.

Unknown parameters, repeated parameters (there are no OR lists), invalid values and mutually exclusive combinations are rejected with an explanation; filters are never silently dropped. HTML and plaintext responses return HTTP 400; HTMX swaps the error fragment into the page.

## Route Explorer layout and page titles

The Route Explorer is built to put the route table near the top of the page. From top to bottom:

1. **Search panel.** One row with Backend, Search by, Search value (the widest control), View and **Search**. Each control has a real `<label>` (visually hidden to save height) and the form is a labelled `role="search"` landmark. A second line holds the **Advanced filters** disclosure (collapsed unless a search was rejected; the active-filter count stays visible) and, on the right, the query context, **Copy link** and **Clear search**. On narrow screens the controls wrap (the search value first) and grow to touch-sized heights; desktop controls stay compact.
2. **Query context.** For an executed search: a `Loc-RIB` tag (searches always use the Loc-RIB), a chip for every filter that the search box does not already show (advanced filters and prefix modes, so the search value is never shown twice), and an **i** help popover with the live-query explanation (the `/rib?…` query sent to each backend, "not snapshots", cache lifetime). This element lives next to the form, outside `#results`, so HTMX responses replace it out of band.
3. **Backend status.** One line directly above each backend's table: outcome (`COMPLETE`, `PARTIAL`, `FAILED`, always as text), backend name and location, route count, query duration, response size and, if applicable, `cached … ago`; a paged result adds `showing 1–100`. Failures and partial results get a compact notice inside that backend's panel, so a failed backend never hides a successful one and backends are never merged. Long names are truncated visually and stay complete in the tooltip. The line's duration is the *backend query* time; it is not the footer's page-generation time.
4. **Route table.** The dense desktop table with a screen-reader caption naming its backend; optional columns and the phone layout are described in [Mobile layout, route columns and raw JSON](#mobile-layout-route-columns-and-raw-json).
5. **Pager.** Shown (above and below the results) only when there is more than one page; a single-page result shows its count in the status line only. Page semantics are unchanged.
6. **Footer** with the page-generation time.

Copy link confirms in its label ("Copied") and in a polite live region, then restores itself; the navigation bar keeps its own Copy link on pages that have no search panel.

### Browser titles

Every HTML page has a title of the form `<subject> · <site name>`; the site name is the configured `server.title` (default `Hextet Looking Glass`) and is produced by a single function (`Handler.siteName`), so it is the only thing that changes when the application name becomes configurable branding.

| Page | Subject |
|---|---|
| `/` without a search | `Route Explorer` |
| Prefix or address | the normalized value, e.g. `192.0.2.0/24`, `2001:db8::/32` |
| Source ASN | `AS395611` |
| Large community | `395089:5:2` |
| Standard / extended community | `Community 65000:100`, `Extended community rt 65000:1` |
| Neighbor / peer group | `Neighbor 192.0.2.1`, `Peer group transit` |
| Several filters | the first primary filter above plus `+N filters`; prefix modes read `(more specifics)` / `(covering prefixes)`; filters without a primary one read `Route search (N filters)` |
| Rejected request | `Invalid search` (the rejected input is never echoed) |
| `/neighbors`, `/summary`, `/rtr`, `/communities` | `BGP Neighbors`, `BGP Summary`, `RTR Sessions`, `Communities` |

A single selected backend is appended as `@ <backend name>` (neighbor addresses and route views are backend-specific); `all` adds nothing. Page 2 and later add `(page N)`. Values come from the normalized query, not the raw input; control and invisible characters are removed, long values are cut at 64 characters with `…`, backend names at 32, and the text is HTML-escaped. Backend addresses never appear.

HTMX responses carry the title as a leading `<title>` element, which HTMX 2 applies to `document.title` in the same swap (no extra script). Full pages, including the one served for a history-restore request, have exactly one `<title>` in `<head>`. Back and forward restore each entry's title with its results; **Clear search** is a normal link to `/`, so it loads the default title.

## Mobile layout, route columns and raw JSON

The route table is one piece of markup that adapts to the available width through CSS media queries (no JavaScript viewport detection, no second query and no second set of rows):

| Width | Presentation |
|---|---|
| above 1000 px | Desktop: the dense table, unchanged row heights; search controls in one row. |
| 701–1000 px | Tablet: the same table, scrolled horizontally inside its own container instead of squeezing columns (the search controls wrap at 1000 px and 820 px as before). |
| 700 px and below | Phone: each route becomes a compact card. The prefix (with `BEST`), origin ASN, RPKI and ASPA state (labelled text, not colour only), next hop and a two-line AS path are visible; neighbor, local preference, MED and age move into the expanded details together with the full AS path. |
| 520 px and below | Backend and search type share a row, the view selector shares one with **Search**. Below 360 px controls stack and the details grid has one column. |

Each route has one details row holding two independent native `<details>` disclosures on the same line: **Route details** and, only when enabled, **Raw JSON**. Either, both or neither can be open; opening one never opens the other. While both are collapsed the row is exactly as tall as the former single "Route details" row. An expanded disclosure takes the full width and the other one moves below it. The Neighbors and Summary pages use the same pair (**Neighbor details** and **Raw JSON**).

### Display configuration

The optional `display` section is presentation only: it never changes the OpenBGPD request, route parsing, cache keys, search filters or visibility. Omitting it keeps every existing configuration working.

| Setting | Default | Effect |
|---|---|---|
| `display.raw_json` | `false` | Renders the Raw JSON disclosures (routes, BGP neighbors, BGP summary). When `false` no control is rendered and the payload is not embedded in the page. |
| `display.route_columns.neighbor` | `false` | Shows the Neighbor column. When hidden, the neighbor stays in Route details and in neighbor searches. |
| `display.route_columns.neighbor_format` | `name` | `ip`, `name` or `both`; anything else fails startup with the allowed values. |
| `display.route_columns.rpki` | `true` | RPKI (OVS) column. Hidden: the state stays in Route details; the `ovs` filter is unchanged. |
| `display.route_columns.aspa` | `true` | ASPA (AVS) column, same rules. |

**Raw JSON discloses internal routing details.** The raw backend objects include fields the structured views leave out (router and BGP IDs, neighbor and session internals). Enable `raw_json` only for trusted deployments. It controls what is rendered, not how backend responses are parsed; there is no endpoint that serves raw data, the RTR page and the plaintext views never show it, and the response-size limits are unchanged.

**Neighbor names** use the existing next-hop reverse DNS machinery ([PTR enrichment](docs/PTR_ENRICHMENT.md)): the same cache, workers, timeout, TTLs, negative caching and deduplication, and the same cache-only `/ptr-status` endpoint. `name` shows the PTR name and falls back to the address while it is unresolved or when there is none; `both` shows `name (address)` or just the address; `ip` never resolves anything. A name never replaces the identity of a neighbor: the link always targets the backend-supplied address and the source backend, and the address stays in the tooltip. Names are looked up only while the Neighbor column is visible and `ptr_enrichment` is enabled (otherwise the address is shown and a warning is logged at startup). Neighbor lookups share the page's `max_lookups_per_page` budget and are scheduled after next hops, so they cannot add DNS work. The column keeps a fixed width and truncates long names with an ellipsis, so a late answer does not move other columns.

### Browser history

Back and Forward re-render the page from the server using the canonical URL (`historyCacheSize` is 0, so HTMX never restores a DOM snapshot, whose form values are stale). The search box, search type, backend, view, advanced filters, title and results therefore always agree with the URL, exactly as after a reload. Backend queries are subject to the normal result cache. Search controls and the theme button are wired through delegated listeners so they keep working after the page body is replaced.

## Page generation timing

HTML pages end with a muted footer note such as `Generated in 42.7 ms`. It is on by default and is switched off with `features.page_timing: false` (existing configurations need no change). The flag only removes the display: the HTTP duration metric and the access log keep measuring every request.

- **What it measures.** The server-side time from the moment request handling starts (the same instant the access log and `hextet_lg_http_request_duration_seconds` start from) until the footer is rendered. That covers request parsing and validation, work admission, the route cache lookup, waiting for OpenBGPD (or for another request's shared in-flight query), route processing, visibility exclusions, pagination, enrichment scheduling and all template output before the footer.
- **What it does not measure.** Network time, browser rendering, the footer's own few bytes, or the background ASN and PTR lookups, which never delay a page. It is not the duration printed in each backend's status line, which is how long the original backend query took.
- **Caching.** A cache hit shows the work this request did (typically a few milliseconds), a cache miss includes the OpenBGPD wait, and a request that joins a shared in-flight query shows its own wait. A cached result's original query time never appears in the footer.
- **Streaming.** HTML is streamed from the template to the client and is not buffered to obtain the figure, so the value is read just before the footer, when everything above it has been handed to the connection. If the client reads more slowly than the server writes and the page is larger than the socket buffers, that stall is part of the figure. HTML pages are bounded (100 routes per page), so this is a corner case; `output=plain`, text responses, JSON and the status endpoints are never modified and carry no timing.
- **Formats.** One decimal below 100 ms (`8.4 ms`, `42.7 ms`), whole milliseconds below one second (`836 ms`), two decimals below 10 s (`1.24 s`), then `12.3 s` and `134 s`; under 0.05 ms it reads `<0.1 ms`. If no start time is available the note is omitted rather than invented.
- **HTMX.** A search or pager link replaces only `#results`, so the response carries the new figure as a trailing out-of-band element (`<span id="page-timing" hx-swap-oob="true">`) that HTMX swaps into the footer in the same step as the new results. The figure therefore always belongs to the results beside it, even if responses arrive out of order, and the ASN/PTR enrichment updates (which only set `title` attributes) never touch it. Browser history restoration from HTMX's snapshot cache shows the figure of the request that originally generated that page; it is not re-measured. When the snapshot is not available the server renders the full page again and measures anew.
- **Error pages.** The validation-error page and other application pages show the figure with their original status code. Responses produced before a page exists (rate limiting, oversized URL, 404 for a disabled view, busy and shutdown replies) are not application pages and carry none.

## Session visibility

Two independent switches under `features` control the session views: `bgp_neighbors` (`/neighbors` and `/summary`) and `rtr_sessions` (`/rtr`). Both default to on, so existing configurations are unchanged; a disabled view has no navigation link and answers 404. The RTR page reads bgplgd's `/rtr` endpoint (present in OpenBGPD 8.2 to 9.3). It distinguishes sessions, no sessions, an unsupported backend, an unavailable backend and a malformed response, and does not publish validator addresses unless `features.rtr_session_addresses` is enabled. See [docs/SESSION_VISIBILITY.md](docs/SESSION_VISIBILITY.md) for the exact fields, endpoints, multi-backend behavior and privacy notes.

## Search filters (compound queries)

All filters are optional and combinable; they are sent to OpenBGPD in a **single** `GET /rib` request built with `net/url`, and OpenBGPD does the filtering. The application never downloads a RIB to filter it locally. The Advanced Filters section of the form lists them; the plain search box (`type` + `q`) remains the default.

| Parameter | Meaning |
|---|---|
| `prefix` | IPv4/IPv6 address or prefix |
| `as` | **source** ASN (OpenBGPD's source-AS filter; not AS-path membership, no customer cones) |
| `community` | standard (RFC 1997) community, `as:value` or a well-known name such as `NO_EXPORT` |
| `ext-community` | extended community; grammar validated by OpenBGPD |
| `large-community` | RFC 8092 `x:x:x` |
| `neighbor` / `group` | peer address (IPv4 or IPv6) or description / peer group |
| `af` | `ipv4`, `ipv6`, `vpnv4`, `vpnv6` |
| `rib` | routing table name (Loc-RIB endpoint only) |
| `ovs` | `valid`, `invalid`, `not-found` |
| `avs` | `valid`, `invalid`, `unknown` |
| `best`, `error`, `filtered`, `invalid`, `leaked` | route state flags, value `1` |
| `all`, `or-shorter` | prefix scope flags (need `prefix`) |

Rules enforced before any request is made, because bgplgd would otherwise silently honour only one of them:

- `neighbor` and `group` are mutually exclusive.
- At most one of `best`, `error`, `filtered`, `invalid`, `leaked`.
- `all` and `or-shorter` are mutually exclusive and require `prefix`.
- All searches use Loc-RIB (`/rib`). Legacy `endpoint=rib-in|rib-out` and `rib=in|out|local` are rejected; `endpoint=rib` is accepted as a legacy alias and removed from canonical links. Other `rib` values select a Loc-RIB routing table.
- Standard, extended and large communities stay separate filters and separate result columns.
- A flag value of `0` is treated as unset.

OpenBGPD answers a query that bgpctl rejects (for example a malformed `ext-community`, or an unknown `group`) with HTTP 200 and an *empty body*. A genuine "no matches" answer is a small JSON document. The application distinguishes them: an empty body is reported as a failed query, never as "no matching routes".

### IPv4 and IPv6

Both families are supported everywhere and treated identically. Without `af`, no family restriction is applied and OpenBGPD returns IPv4 and IPv6 routes together (on the reference backend, `as=15169` returns 1,068 IPv4 + 146 IPv6 = 1,214 routes; `af=ipv4`/`af=ipv6` select the halves natively). Prefix and neighbor addresses are validated with `net/netip` and canonicalized (IPv6 compressed, lower-case; host bits preserved) so equivalent spellings share one cache entry and one URL; IPv4-mapped IPv6 addresses and zones are rejected in prefix searches. A route's family comes from its **prefix**, never from its next hop or neighbor: an IPv6 route may have an IPv4 next hop (RFC 8950) and either family of neighbor (the reference backend carries IPv6 routes over an IPv4 session). Plaintext columns widen to fit the longest prefix or next hop, so full 43-character IPv6 prefixes are never truncated, while IPv4-only output keeps its traditional layout. `true_nexthop: "???"` (unresolved) falls back to the exit next hop. Unmodelled attributes remain available in each route's raw JSON.

## Large responses, streaming and partial results

A DFZ-sized answer is an expected operating condition. For example, the Large Community search `395089:0:8` on the reference backend returns 112 MiB / 179,243 routes (IPv4 + IPv6) in about 9 seconds.

- **Streaming decode.** The response is decoded incrementally with `encoding/json.Decoder` as it arrives. The first complete route is available after ~0.1 s. The raw body is never buffered whole and only a compacted copy of each route's JSON is retained.
- **Completion is explicit and per backend**: `complete`, `partial` or `failed`, with a reason (`timeout`, `response_limit`, `memory_limit`, `capacity`, `connection_closed`, `invalid_json`, `backend_error`, `canceled`, `busy`).
  - *complete*: the whole JSON document was decoded and validated (closing array/object present, nothing after it). Individually valid routes are not enough.
  - *partial*: interrupted after at least one **complete** route. Those routes are kept; an unfinished route object is discarded. More matching routes may exist, so a partial result never implies that a prefix is absent.
  - *failed*: nothing usable was received. This is not evidence that no routes match.
- **Independent backends.** With `backend=all` each backend has its own status; `A: COMPLETE — 15,203 routes`, `B: PARTIAL — 42,317 routes (timeout)`, `C: FAILED` are shown side by side and never merged. Healthy and partial backends still contribute routes.
- **No retries.** Interrupted large queries are not repeated automatically.

### Resource limits

All are configurable in `looking_glass` (defaults shown):

| Setting | Default | Notes |
|---|---|---|
| `query_timeout` | 30s | per backend query including the streamed body; `backends[].query_timeout` overrides it |
| `max_response_bytes` | 134217728 (128 MiB) | upstream body, enforced while reading |
| `max_retained_bytes` | 268435456 (256 MiB) | estimated memory for decoded routes of one query; reaching it is a deterministic `memory_limit` partial |
| `max_total_retained_bytes` | 768 MiB | aggregate estimated route retention and overview reservations, including results still referenced after cache eviction; exhausting it (other queries hold the memory) is a transient `capacity` partial |
| `max_concurrent_work` / `max_queued_work` | 8 / 16 | active backend-driven requests including rendering, and waiting requests |
| `page_size` | 100 | routes per HTML page |
| `max_concurrent_queries` | 2 | all upstream route queries across the whole instance; a query that cannot start within `queue_wait` or half its timeout is `busy` |
| `max_concurrent_backends` | 4 | backend fan-out within one request |
| `cache_ttl` / `cache_max_bytes` | 20s / 256 MiB | see below; `cache_ttl: 0s` disables the cache |

128 MiB of JSON does not need 128 MiB of RAM: it needs more. Measured on the reference backend, 112 MiB of JSON decoded to about 250 MiB of retained routes, and the container's cgroup peak was 446 MiB because Go's collector needs headroom. `max_retained_bytes` therefore bounds retention separately from the byte limit, and the process sets a soft Go memory limit from the configured budgets (`max_concurrent_queries × max_retained_bytes + cache_max_bytes`, plus slack) unless `GOMEMLIMIT` is set. Plan container memory for roughly that budget; with the defaults, a single large query peaked around 450 MiB. Lower the limits for smaller hosts.

### Public deployment controls

The default request-target limit is 8192 bytes, query-string limit 6144 bytes, parameter limit 64, and header limit 32768 bytes. The server has a 5s header read timeout, 15s request read timeout, 60s idle timeout, and 15s per-write deadline; there is no overall write deadline that cuts short a backend query. These limits permit IPv6 and compound searches. Header and URL limits and rates are configurable in `server` as shown in the example.

Per-peer token buckets allow 10 inexpensive requests per second (burst 60), and 1 live request per second (burst 8). Live includes Route Explorer requests with parameters, neighbors, summary and RTR sessions. Exhaustion returns 429 and `Retry-After`; liveness and readiness are exempt. The limiter holds at most 10,000 buckets and removes inactive entries after ten minutes using a periodic cleanup. It uses the direct TCP peer only and ignores all forwarded headers. Behind Cloudflare Tunnel or another proxy all users therefore share the proxy's allowance; configure rate limiting at the trusted ingress or size the limits for the aggregate. Do not expose an unauthenticated bypass around that ingress.

All route queries, including prefix searches, share `max_concurrent_queries` slots; `max_queued_queries` (16) bounds active and waiting work and `queue_wait` defaults to 2s. Identical queries share a flight and cached responses do not take a slot. A fully busy result returns 503 with `Retry-After`; retry later or increase capacity only after sizing memory. A partial response means one or more backends stopped early; check its reason and do not infer absence of routes. Limits are per process, not shared across replicas.

Security headers restrict scripts and styles to same-origin assets, deny framing, disable MIME sniffing, limit referrers and disable camera, microphone and geolocation. `/healthz` checks process liveness without contacting a backend; `/readyz` reports shutdown readiness. SIGTERM and SIGINT stop admission and wait up to `shutdown_grace` (40s), then close connections and cancel shared work. Compose grants 45s for shutdown (see *Shutdown* below).

**Memory sizing:** Each dataset has one reservation shared by its cache, flight and request owners; eviction drops only the cache reference, and the reservation remains until the last renderer finishes. Retention is reserved incrementally before appending each complete route. `cache_max_bytes` is a cache-eviction sublimit within `max_total_retained_bytes`, not an additional aggregate allowance. Summary and neighbor responses are limited to 1 MiB each, and reserve 128 MiB per selected backend until rendering completes. The work gate includes cache hits and slow writers; saturation returns 503 with `Retry-After`. Health and local documentation do not use this gate.

The aggregate counter is an estimate of retained route objects plus fixed overview allowances; JSON decoder buffers, individual large JSON objects, formatting, Go heap fragmentation, garbage collection, network buffers and runtime memory are not precisely charged. It is neither a heap nor RSS limit. The runtime's derived `GOMEMLIMIT` is soft. Start with at least 2 GiB container memory for the default limits and measure representative traffic before production; this is an initial recommendation, not a measured ceiling. A single large query in the earlier implementation measured about 446 MiB cgroup peak; a local in-process mock smoke run (128 requests, 10,000 routes per result) observed 71.2 MiB peak process RSS and 51.0 MiB peak heap, but was short and is not sustained mixed-load container validation. Run `HEXTET_LOAD_TEST=1 go test ./internal/web -run TestMockLoad -v` to repeat it; this includes the test process and mock backend, not a container memory measurement. Set lower route, response, cache, aggregate and work limits together for smaller hosts, leaving substantial headroom, and enforce ingress concurrency and rate limits.

### Cache admission for partial results

Whether a finished result may be replayed from the cache is decided from its typed completion reason, never from message text:

| Outcome | Cached? | Why |
|---|---|---|
| `complete` | yes | normal case |
| `partial`: `memory_limit` (per-query limit), `response_limit`, `timeout`, `connection_closed`, `invalid_json`, `backend_error` | yes | the same query tends to end the same way, and paging must not re-run an expensive query for every page |
| `partial`: `capacity` (aggregate budget exhausted by other work) | **no** | describes this instant's load; replaying it for `cache_ttl` would keep serving a degraded answer after capacity recovered |
| `partial`: `busy`, `canceled` | no | state of this instance or a departed client |
| `failed` (any reason) | no | nothing usable |

A result that is not cached is still delivered, with its true status, to every request that shared its in-flight query; it is simply not retained. It never replaces or evicts an existing complete entry, and its memory is returned like any other. Skips are counted in `hextet_lg_cache_admission_skips_total{reason}`. The `capacity` message says the shared server budget is exhausted; it is not the per-query `max_retained_bytes` limit.

## Observability

### Logging

Logs are structured (`log/slog`), one record per line on stderr, JSON by default:

```yaml
logging:
  level: info     # debug | info | warn | error
  format: json    # json | text
```

Both settings are optional. Example records:

```json
{"time":"2026-11-09T23:57:06Z","level":"INFO","msg":"backend query","request_id":"093dab1768c3af3c","backend":"local","operation":"rib","status":"complete","backend_duration_ms":412.2,"routes":3,"bytes":1199,"retained_bytes":3630,"first_route_ms":0.3}
{"time":"2026-11-09T23:57:06Z","level":"INFO","msg":"request","request_id":"093dab1768c3af3c","method":"GET","route":"index","status":200,"duration_ms":415.9,"bytes":15762,"result":"complete","backends":1,"cache_hits":0,"cache_misses":1}
{"time":"2026-11-09T23:58:11Z","level":"WARN","msg":"backend query","request_id":"7c1f0e0a3d6b9a42","backend":"local","operation":"rib","status":"partial","backend_duration_ms":30001.4,"routes":42317,"bytes":61203344,"retained_bytes":104857600,"reason":"timeout"}
```

- **One `request` record per HTTP request** (method, normalized `route`, status, `duration_ms`, bytes, and for route searches the overall `result`, backend count and cache hits/misses) and **one `backend query` record per upstream query** (backend ID, `operation` = `rib`, `rib_in`, `rib_out`, `neighbors`, `summary`, `rtr`, `status`, `reason`, backend duration). Routes are never logged individually.
- `request_id` is generated per request (never accepted from the client) and is returned in the `X-Request-Id` response header. A shared in-flight query is logged under the id of the request that started it; requests that joined it have `cache_hits` counted.
- **Levels:** `info` for normal requests and complete queries; `warn` for 5xx responses (including 503 overload), partial or failed backend queries and clients that stopped reading (`client":"write_timeout"`); `error` for 500s, render failures and deliberately aborted responses; `debug` for health checks, static files, 429s and queries that were refused before reaching a backend. Client cancellations and disconnects are `info` with `"client":"canceled"` or `"disconnected"`.
- **Never logged:** query strings, prefixes, ASNs, headers, credentials, client addresses, backend URLs or response bodies. Unrecognized paths are logged as `route":"other"`.
- Shutdown records are described under *Shutdown*.

### Metrics

Prometheus metrics are **disabled by default** and are served by a separate listener, never by the public `server.listen` address:

```yaml
metrics:
  enabled: true
  listen: "127.0.0.1:9090"   # loopback, or an explicit management address
```

The public server has no `/metrics` route (it returns 404 whatever headers the client sends, and `X-Forwarded-For` is never consulted). The management listener serves only `GET /metrics`. A non-loopback `listen` is allowed, and logged as a warning at startup, because then the only access control is your network policy. `listen` may not share a port with `server.listen`.

**Standalone:** scrape `http://127.0.0.1:9090/metrics` from the same host.

**Docker Compose:** loopback inside a container is reachable only from that container, so a collector must reach a non-loopback address inside the container, but nothing should publish it. Set `metrics.listen: "0.0.0.0:9090"` in `configs/config.docker.yaml`, do **not** add a `ports:` entry for 9090, and attach the collector to the same user-defined Docker network (for example by adding the same network to Hextet LG with a Compose override file, and scraping `hextet-lg:9090`). For a quick local check without a network, run `docker exec hextet-lg wget -qO- http://127.0.0.1:9090/metrics` with `listen: "127.0.0.1:9090"`. The shipped `compose.yaml` publishes only 8080. "Unpublished" means not reachable from outside the Docker host; the host itself and every container on the same Docker network can still reach the container's address, so attach only your collector to that network. The listener serves `GET /metrics` and nothing else, and has no authentication of its own.

| Family | Type | Meaning |
|---|---|---|
| `hextet_lg_http_requests_total{method,route,status_class}` | counter | requests; `status_class` is `2xx`…`5xx` or `client_closed` (client left before the response completed; kept out of the error rate). `route` is one of `index`, `neighbors`, `summary`, `rtr`, `communities`, `healthz`, `readyz`, `static`, `other` |
| `hextet_lg_http_request_duration_seconds{method,route}` | histogram | request duration |
| `hextet_lg_http_active_requests` | gauge | requests being served |
| `hextet_lg_build_info{version,revision,go_version}` | gauge | always 1; one series identifying the running build (same values as `hextet-lg version`) |
| `hextet_lg_http_rate_limited_total{bucket}` | counter | per-peer rate-limit refusals (`page`, `live`) |
| `hextet_lg_http_client_aborts_total{route,kind}` | counter | `canceled`, `disconnected`, `write_timeout` |
| `hextet_lg_work_gate_rejections_total{reason}` | counter | admission refusals: `queue_full`, `wait_timeout`, `canceled`, `shutdown` |
| `hextet_lg_work_active` / `_limit` / `hextet_lg_work_admission_waiters` | gauge | admitted work, its limit, requests waiting for a slot |
| `hextet_lg_query_rejections_total{reason}` | counter | upstream queries refused locally: `queue_full`, `slot_timeout`, `shutdown`, `canceled`, `budget` |
| `hextet_lg_query_queue_depth`, `hextet_lg_active_route_decodes` | gauge | queries queued or active / holding an expensive-query slot |
| `hextet_lg_backend_requests_total{backend,operation,status}` | counter | attempted upstream requests by outcome (`complete`, `partial`, `failed`) |
| `hextet_lg_backend_request_duration_seconds{backend,operation}` | histogram | upstream duration including the streamed body |
| `hextet_lg_backend_failures_total{backend,operation,reason}` | counter | partial or failed requests by reason; `reason="timeout"` and `reason="canceled"` are the timeout and cancellation counts |
| `hextet_lg_backend_active_queries{backend}` | gauge | upstream requests in flight |
| `hextet_lg_backend_last_success_timestamp_seconds{backend}` | gauge | last complete request (absent until one happens) |
| `hextet_lg_backend_last_failure_timestamp_seconds{backend}`, `hextet_lg_backend_last_failure_info{backend,reason}` | gauge | last backend-side failure and its reason |
| `hextet_lg_resource_retained_bytes`, `hextet_lg_resource_memory_budget_bytes` | gauge | aggregate accounted retained bytes and its configured budget |
| `hextet_lg_resource_budget_denials_total` | counter | reservations refused because the aggregate budget was full |
| `hextet_lg_cache_hits_total`, `_misses_total`, `_evictions_total`, `_expirations_total`, `_shared_flight_reuse_total` | counter | cache behavior; a request that joins an in-flight query is a miss and a shared-flight reuse |
| `hextet_lg_cache_entries`, `_retained_bytes`, `_max_bytes`, `_active_flights` | gauge | cache size, budget and running shared queries |
| `hextet_lg_cache_admission_skips_total{reason}` | counter | results not stored: `failed`, `capacity`, `transient`, `abandoned`, `too_large`, `disabled` |
| `hextet_lg_shutting_down` | gauge | 1 once shutdown has begun |
| `go_*`, `process_*` | | Go runtime and process metrics |

The resource, work-gate and cache metrics read the same counters the application enforces limits with; they are not a second accounting. Reading them is O(1) (no cache scan). Labels come only from fixed sets and configured backend IDs: no prefix, ASN, neighbor, client address, request ID or query string is ever a label.

**Backend health** is judged from real traffic only; nothing polls a backend. A `complete` result is a success. `timeout`, `connection_closed`, `invalid_json` and `backend_error` (including a non-2xx status) are backend-side failures and set the last-failure time and reason, even when partial routes survived. Local limits (`response_limit`, `memory_limit`, `capacity`) and `canceled` touch neither timestamp: they say nothing about the backend. `/healthz` and `/readyz` are unchanged and do not depend on backends.

Useful PromQL:

```promql
# Result cache utilization (0..1) and hit ratio
hextet_lg_cache_retained_bytes / hextet_lg_cache_max_bytes
rate(hextet_lg_cache_hits_total[5m]) / (rate(hextet_lg_cache_hits_total[5m]) + rate(hextet_lg_cache_misses_total[5m]))

# Aggregate memory budget in use (0..1); sustained values near 1 mean capacity partials
hextet_lg_resource_retained_bytes / hextet_lg_resource_memory_budget_bytes

# Active upstream queries per backend, and decodes against the instance limit
hextet_lg_backend_active_queries
hextet_lg_active_route_decodes

# Backend error ratio and timeouts (excludes client cancellations and local limits)
sum by (backend) (rate(hextet_lg_backend_failures_total{reason=~"timeout|connection_closed|invalid_json|backend_error"}[5m]))
  / sum by (backend) (rate(hextet_lg_backend_requests_total[5m]))
sum by (backend) (rate(hextet_lg_backend_failures_total{reason="timeout"}[5m]))

# Seconds since a backend last answered completely
time() - hextet_lg_backend_last_success_timestamp_seconds

# Overload: admission refusals, budget denials, 503s
sum by (reason) (rate(hextet_lg_work_gate_rejections_total[5m]))
rate(hextet_lg_resource_budget_denials_total[5m])
sum(rate(hextet_lg_http_requests_total{status_class="5xx"}[5m]))
```

### Shutdown

On SIGTERM or SIGINT the process logs `shutdown initiated` (with the grace period), stops admission and reports `/readyz` unavailable (`admission disabled`, with the outstanding `active_http`, `active_work`, `admission_waiters`, `active_decodes`, `active_flights` and `retained_bytes`), drains requests for at most `server.shutdown_grace`, and logs either `shutdown complete` (`graceful: true`) or, if the grace expired, a warning with the remaining counts followed by `shutdown finished after deadline`. Both final records carry `duration_ms`, `settled` and the final counts, read after a bounded (at most 0.5 s) wait for forced-closed handlers to unwind; they should be zero.

**Grace periods must nest.** Docker sends SIGKILL when `stop_grace_period` expires, so the application's grace must be shorter. The defaults are 40 s application grace (`server.shutdown_grace`) inside 45 s Docker grace (`stop_grace_period` in `compose.yaml`); an isolated stop test with a hung query, a stalled reader and a slow reader finished in 40.1 s with exit code 0. Keep a margin of at least a few seconds. Raise both together, application first reviewed against Docker, never `shutdown_grace` alone.

### Troubleshooting

| Symptom | Look at |
|---|---|
| Users see "busy" / 503 | `hextet_lg_work_gate_rejections_total` (`queue_full`, `wait_timeout`), `hextet_lg_query_rejections_total{reason="slot_timeout"}`; compare `hextet_lg_work_active` with `_limit` and `hextet_lg_active_route_decodes` with `max_concurrent_queries` |
| Results marked partial, reason "capacity" | `hextet_lg_resource_retained_bytes` near `..._memory_budget_bytes` and `hextet_lg_resource_budget_denials_total`: several large queries or long renders hold memory. Retry shortly; these are never cached. Lower concurrency or raise the budget only after sizing container memory |
| Partial, reason "memory_limit" | the single query exceeds `max_retained_bytes`; deterministic, narrow the search |
| Backend slow versus down | `hextet_lg_backend_request_duration_seconds` rising with `status="complete"` is slow; `failures_total{reason="timeout"}` is a stall; `connection_closed` / `backend_error` with a recent `last_failure_timestamp` is down or erroring; no failures but a stale `last_success` means no traffic |
| Few cache hits | `hextet_lg_cache_admission_skips_total`, `evictions_total` (budget too small) and `expirations_total` versus `cache_ttl` |
| Slow or stuck responses | `hextet_lg_http_client_aborts_total{kind="write_timeout"}`: clients that stopped reading hold a work slot until the 15 s write deadline |
| Container killed during stop (exit 137) | the logs lack `shutdown complete`; compare `duration_ms` with `stop_grace_period` |

Correlate a user report with `X-Request-Id`, then search the logs for that `request_id`.

## Pagination and the optional cache

OpenBGPD has no server-side pagination, result limit or streaming contract, so HTML pagination does **not** reduce the upstream response, and the application does not fake it. Instead it decodes the answer once and slices it: 100 routes per page, with page number, total decoded count, previous/next links and status on every page (`&page=N`, stable URLs).

To make page navigation practical, validated results (complete and partial, with their completion metadata) are kept in a small in-memory cache keyed by backend ID and the canonical query. It is bounded by a global memory budget, uses a fixed TTL from fetch time (default 20 s) with a timer-driven release, evicts expired then least-recently-used entries, never stores failed outcomes or partial results caused by momentary load (see *Cache admission for partial results*), never copies route data, and shares one upstream query between identical concurrent requests (cancelled only when every waiter has left). Nothing is persisted. A cached page says so and shows its age; cached results are temporary, **not historical snapshots**.

### Plaintext output and HTTP semantics

The HTML table, plaintext summary (`view=text`) and plaintext detail (`view=detail`) all render from the same decoded routes. `output=plain` (or `Accept: text/plain`) returns `text/plain; charset=utf-8` for text views, streamed through a buffered writer and stopped if the client disconnects; it includes **all** decoded routes unless `page=` is given. Incomplete backends get a conspicuous block, for example:

```text
WARNING: PARTIAL RESULTS
Backend: Winnipeg (Home)
Reason: Backend timeout after 30s
Routes decoded: 42317
Elapsed: 30.0s

The following routes are only a subset of the
matching routing information. More matching routes may exist.
```

Completion metadata for machine clients is sent as headers on route responses (header names are case-insensitive):

- `X-Hextet-LG-Completeness`: `complete`, `partial` or `failed` (overall; `partial` whenever any backend is incomplete).
- `X-Hextet-LG-Routes`: total decoded routes.
- `X-Hextet-LG-Backends`: `id=status[;reason=...];routes=N;elapsed=Ns[;cached=1]`, comma separated.

Plain-text status codes: `200` for complete **and** partial results (check the headers or the in-body warning), `400` for invalid requests, `404` for a page beyond the last, `504` if every backend timed out, `503` if busy or cancelled, `502` for other all-backend failures. A partial result is never an empty result.

## Community definitions, route tooltips and Community Explorer

BGP community descriptions are **operator-supplied data**, not built into the application. Enable them with the optional `community_definitions` section (`enabled: false` by default; `directory` is required when enabled) pointing at a directory of `asN.txt` files in the legacy [NLNOG looking glass format](https://github.com/NLNOG/lg.ring.nlnog.net/tree/main/communities) (`64500:100,Learned from customer`; ranges `400-499`, one-digit `x` and any-number `nnn` wildcards, and `$0` substitutions; standard, large and extended communities). Files are validated and compiled **once at startup**; a configuration that cannot be loaded reliably fails startup with file and line, and changes need a restart. There is no download, watcher or runtime reload. Grammar, precedence, limits and differences from the upstream implementation are in [docs/COMMUNITIES.md](docs/COMMUNITIES.md).

With definitions loaded, communities in a route's expanded **Route details** show a native `title` tooltip, escaped as text, while the visible value stays unchanged; unknown communities simply render their raw value. Text views and `output=plain` are never annotated. Definitions are presentation metadata only: searches, canonical URLs, the `features.*` flags, route visibility and caching are unaffected, a community stays searchable by its raw value whether or not it has a description, and descriptions are not searchable.

Community Explorer (`/communities`) lists the loaded definitions of any number of ASNs, with the existing text and category filters (a page shows at most 1,000 matches) and a one-click search link for each exact definition whose search type is enabled. The legacy format cannot express the former *category*, *notes* and *implementation status/evidence* columns, so those come from an optional `metadata.yaml` in the same directory (documented in the guide). **Implemented** there means the operator recorded the community as present in its own configuration (for example a RouterOS community list, routing filter or configured pattern); it is not independent verification that the intended routing behaviour occurs, and neither are the descriptions authoritative.

The former built-in AS395089 reference has been moved out of the code into [`examples/communities/`](examples/communities) (`as395089.txt`, 30 definitions, and `metadata.yaml`). Nothing loads it unless you configure the directory: a clean installation is operator-neutral. In Docker, mount the directory read-only at `/etc/hextet-lg/communities` (see [Docker deployment](#docker-deployment)).

## Isolated container load exercise

On Linux with Docker, `python3 scripts/container-load.py` starts a separate 2 GiB-limited container on loopback port 18080 and a mock backend on loopback port 19099. It refuses an occupied test port, uses an independently generated configuration containing only the mock address, disables the image health check, runs for 305 seconds, samples container memory, and removes the test container on exit. It sends concurrent shared and distinct route searches together with summary and neighbor requests. It does not use either operator configuration. Do not run it if those ports are needed by another service.

`python3 scripts/container-stop.py [image]` is the matching stop-deadline check: it runs one more isolated, Compose-restricted container (loopback ports 18081 and 19098) with a hung shared query, a client that never reads and a very slow reader, sends Docker's normal stop with the 45-second grace, and fails if the container is killed (exit 137) instead of exiting cleanly within the deadline. The resource-recovery, cache-churn and shutdown tests live in `internal/web/closure_*_test.go`; run them with `go test -race ./internal/web`.

A 305-second run with 10,000 mock routes per dataset and 24 client workers completed 85,367 HTTP 200 responses and 245 HTTP 503 responses, all with `Retry-After`; sampled container memory peaked at 359.9 MiB against a 2 GiB limit, and Docker reported no OOM kill. The samples rose initially and then fluctuated rather than growing monotonically. This workload does not represent a full DFZ result, does not measure Go heap or aggregate reservation peaks, and does not exercise slow/disconnected clients or prove the full shutdown matrix. The previous 446 MiB figure was from a single live large query, not this test. Further representative mixed-load and slow-client testing is needed before treating 2 GiB as a verified ceiling.

## Configuration

`configs/config.example.yaml` is the complete, documented example committed to the repository. Runtime configuration belongs in `configs/config.yaml` for standalone development and `configs/config.docker.yaml` for Compose. Both are local, Git-ignored, and excluded from Docker build contexts. If the development file does not already exist, create it with:

```sh
cp configs/config.example.yaml configs/config.yaml
```

Then set each backend's `url` to the OpenBGPD `bgplgd` HTTP API address reachable from the application. Backend URLs are configured exclusively through YAML; requests cannot supply or override them. `default_backend` can be `all` or an enabled backend ID. Startup validates the listen address, backend IDs, URLs and every limit. Every `looking_glass` setting is optional and falls back to the defaults above, so configurations written for earlier versions keep working; note that explicit older values (for example `query_timeout: 10s` and `max_response_bytes: 16777216`) are honoured and will turn large searches into partial results; remove those lines to adopt the new defaults. The optional `community_definitions` section (disabled by default) is described under Community definitions above; the optional `theme` section is described under Themes. Use `--config /path/to/config.yaml`; the default is `/etc/hextet-lg/config.yaml`.

## Docker deployment

For a minimal OpenBGPD collector and `bgplgd` backend deployment, see [docs/OPENBGPD.md](docs/OPENBGPD.md).

Compose runs only Hextet LG on port 8080. It mounts `./configs/config.docker.yaml` read-only at `/etc/hextet-lg/config.yaml`, runs as an unprivileged user, drops Linux capabilities, enables a read-only root filesystem, and provides a health check. Create the Docker file from the completed development configuration only if it does not already exist, then set its `server.listen` port to 8080 while preserving backend and resource settings. Ensure the non-root container can read it. Do not overwrite either existing file.

```sh
docker compose config -q
docker compose build            # local development image hextet-lg:local (version "dev")
docker compose up -d
docker compose ps
curl -fsS http://127.0.0.1:8080/healthz
```

The image comes from `HEXTET_LG_IMAGE` and defaults to `hextet-lg:local`, so the development workflow above is unchanged. To run a published release, select it by version or, for production, by immutable digest:

```sh
HEXTET_LG_IMAGE=ghcr.io/hextetsystems/hextet-lg@sha256:<digest> docker compose up -d --no-build --pull never
```

`hextet-lg:local` is a mutable tag and is **not** a rollback reference. Upgrades and rollbacks, which preserve the exact previously running image, are described in [docs/RELEASING.md](docs/RELEASING.md) and automated by `scripts/deploy.sh` (`status`, `upgrade <image>`, `rollback`, with `--dry-run`).

For the suggested host layout, keep Compose files in `/srv/docker/compose/hextet-lg/` and place the Docker runtime file at `configs/config.docker.yaml` relative to that Compose project, or adjust the Compose bind mount to your chosen local config path.

To use community definitions, add `community_definitions: {enabled: true, directory: /etc/hextet-lg/communities}` to `configs/config.docker.yaml` and mount the directory read-only next to the configuration (the files must be readable by UID 10001; no write access, port or other restriction changes):

```yaml
    volumes:
      - ./configs/config.docker.yaml:/etc/hextet-lg/config.yaml:ro
      - /srv/docker/data/hextet-lg/communities:/etc/hextet-lg/communities:ro
```

`compose.yaml` carries this as a commented example, because a bind mount of a missing host directory would be created empty by Docker. If the directory is configured but absent, the container exits at startup. Restart the container after editing definition files.

Size the container using the measurements and limitations above; set a deployment-specific hard memory limit only with sufficient headroom, and lower `max_retained_bytes`/`max_concurrent_queries` on small hosts. To validate a different configuration without touching `configs/config.yaml`, use a Compose override file that replaces the config bind mount.

OpenBGPD is not bundled. `localhost:9099` inside the Hextet LG container means that container itself. If OpenBGPD runs in a separate Compose project, attach both services to a shared user-defined Docker network and use the backend service name and port, such as `http://openbgpd:9099`, or configure another reachable address. Keep `bgplgd` reachable only from trusted application networks.

## Releases and versions

Releases are built by GoReleaser from Git tags (`vMAJOR.MINOR.PATCH`, prereleases `-rc.N`) in GitHub Actions: Linux amd64/arm64 archives and `checksums.txt` on GitHub Releases, and a multi-architecture image at `ghcr.io/hextetsystems/hextet-lg` tagged `vX.Y.Z`, `sha-<commit>` and, for stable releases only, `latest`. `hextet-lg version` prints the version, commit, build time and Go toolchain. See [docs/RELEASING.md](docs/RELEASING.md) for versioning, CI/CD, artifacts, GitHub settings, and the upgrade and rollback procedure.

## Local development

```sh
export GOCACHE=/tmp/hextet-lg-go-cache
# Create configs/config.yaml from the example only if it does not exist.
# Set server.listen to port 7000 and configure the backends.
go run ./cmd/hextet-lg --config configs/config.yaml
```

Open `http://127.0.0.1:7000`; Compose can run simultaneously on port 8080. Searches are in the URL query string and can be bookmarked.

## Build and validation

```sh
export GOCACHE=/tmp/hextet-lg-go-cache
go mod tidy
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
go mod verify
go build ./cmd/hextet-lg
./hextet-lg version
```

Requires the Go version declared in `go.mod` (1.24 or newer). Release tooling: `make docker` (local image), `goreleaser check` (`make release-check`) and `make snapshot` (local release build into `dist/`, no credentials, nothing published). CI (`.github/workflows/ci.yaml`) runs the checks above plus the image, Compose and GoReleaser snapshot validation on every pull request and push to `main`.

Tests use `httptest` fixtures and do not require a live OpenBGPD process. Dependencies use normal Go module metadata and are not vendored.

## Known limitations

- **OpenBGPD API.** bgplgd offers no pagination, result limit or streaming contract; HTML paging slices a fully decoded answer. Its filters are the ones listed above only; there is no AS-path or customer-cone search, and none is emulated locally.
- A query that exceeds `query_timeout`, `max_response_bytes` or `max_retained_bytes` returns a clearly identified **partial** result; not every DFZ-sized query can complete within the default 30 seconds.
- bgplgd cannot distinguish "bgpd returned nothing" from "no matches" when it emits a valid empty document (for example for a neighbor address that does not exist); such answers are reported as complete with zero routes. Rejected queries that produce an *empty body* are reported as failures.
- Extended-community syntax is validated by OpenBGPD, not by this application.
- Metrics and log records are in-process only: counters reset on restart, and logs are as durable as the container log driver. There is no tracing and no bundled dashboard or alert rules.
- The footer's page generation time is server-side only and is read before the footer is written, so it excludes the footer itself, network time and browser rendering; HTMX history restoration displays the original request's figure.
- Browser Back/Forward re-queries the backends (through the result cache) instead of restoring a snapshot; results are always live queries.
- On phones the Neighbors, Summary, RTR and Community Explorer tables keep their columns and scroll horizontally inside their own container rather than turning into cards; only the Route Explorer table is card-based.
- Cached results can be up to `cache_ttl` old; disable with `cache_ttl: 0s` for strictly live results.
- RTR sessions: OpenBGPD reports no record counts or last-synchronization time over `/rtr`, so none are shown; a bgplgd without `/rtr` is shown as unsupported. The raw backend JSON on the neighbor pages (off unless `display.raw_json` is enabled) contains fields not otherwise displayed (see [docs/SESSION_VISIBILITY.md](docs/SESSION_VISIBILITY.md)).
- OpenBGPD versions may expose different optional fields. Summary session counts are derived only from returned neighbor records; other data is available as raw JSON per route when `display.raw_json` is enabled.
- Community definitions and any "Implemented" status are operator-supplied; they are not live verification. The loader is stricter than the NLNOG implementation (duplicates, unsupported tokens and leading-zero literals are startup errors), so some upstream files need editing; see [docs/COMMUNITIES.md](docs/COMMUNITIES.md). Extended-community definitions cover the numeric `label A[:B]` forms only, and the OpenBGPD well-known standard community names are mapped from knowledge of its output format, not verified against a live backend.
- The embedded HTMX runtime is pinned at version 2.0.4. No external browser assets are required.
- Absent route fields display as unknown or an em dash. RPKI OVS, ASPA AVS, and route validity remain separate values.
- Live route results depend on the separately managed OpenBGPD API.

### Optional ASN registration descriptions

`asn_enrichment` is disabled by default. When enabled, the server looks up
`AS<asn>.asn.cymru.com` TXT records using its configured system DNS resolver.
These are descriptive registration metadata, not proof of route ownership or
routing authorization. The resolver and Team Cymru can observe queried ASNs;
consider this DNS dependency and disclosure before enabling it.

The initial route or neighbor page does not wait for DNS. Only visible table
routes or displayed neighbors are considered, with origins ahead of path
members. The default budget is 64 newly scheduled ASNs per page, four DNS
workers, a two-second lookup timeout, and a separate bounded cache of 10,000
entries (positive TTL seven days, negative TTL 24 hours). Private,
documentation, and reserved ASNs are skipped. JavaScript makes a bounded
series of cache-only status requests and adds native tooltips; without it,
numbers and links remain usable. DNS failures leave the numbers unchanged.

Persistence is optional and off by default. To enable it, set
`asn_enrichment.cache.persist: true` and mount a writable directory for UID
10001, for example in the Compose service:

```yaml
volumes:
  - /srv/docker/data/hextet-lg/asn-cache:/var/lib/hextet-lg
```

The cache file is written atomically with restrictive permissions, periodically
and at shutdown. An unavailable or unwritable cache file does not prevent the
looking glass from serving requests. The root filesystem remains read-only.

OpenBGPD supplies the AS path used here as display text. Enrichment preserves
its ordering and set/confederation delimiters; it does not reconstruct path
segments or assign meaning to malformed text.

To check browser behavior without automated browser tooling, use a local test
backend and mock DNS resolver: load a route page with uncached ASNs, confirm
numbers and links appear immediately, then confirm titles appear after lookup.
Navigate with the HTMX pager and browser back/forward while responses are
delayed, and confirm titles never migrate to the next page. Repeat with
enrichment disabled and with the resolver returning errors. Do not use a
production backend or live DNS for this check.

Optional asynchronous next-hop PTR tooltips are disabled by default. See [PTR enrichment](docs/PTR_ENRICHMENT.md) for configuration, DNS privacy, address eligibility and caching.
