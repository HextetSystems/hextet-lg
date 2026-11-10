# Testing and integration checks

Three layers, from always-on to opt-in. Only the first runs in CI; nothing here needs a live network.

## 1. Deterministic tests (always on)

```sh
go fmt ./... && go vet ./... && go test ./... && go test -race ./...
node --test internal/web/testdata/*.test.js
```

Backends are `httptest` mocks. `internal/web/m51_test.go` adds the M5.1 regression set:

- **OpenBGPD 9.3 shapes.** `internal/web/testdata/openbgpd93/*.json` have the exact field structure a live OpenBGPD 9.3 bgplgd returned for `/summary`, `/neighbors`, `/rtr` and `/rib`, with every address replaced by a documentation address (RFC 5737/3849), ASNs by private ASNs and descriptions by placeholders. Never commit a real response; sanitize first.
- **Multi-backend matrix.** Eight mock backends (healthy, connection dropped, HTTP 500, malformed JSON, empty, partial, slow, IPv6) in one request: sections stay separate and attributed, failures never read as "no matching routes", cache keys are backend-aware and failures are never cached, pagination and per-backend RTR states (sessions, none, unsupported, unavailable, malformed) stay distinct.
- **Validation states.** `valid`/`invalid`/`not-found`/`unknown`/absent RPKI and ASPA values are not conflated.
- **Privacy and safety.** RTR validator addresses never reach HTML, other pages, the PTR resolver or status endpoints; Raw JSON is absent by default; hostile strings are escaped (including titles and branding); the backend only ever receives GET `/rib`, `/neighbors`, `/summary`, `/rtr`.
- **History.** A rejected search stays HTTP 400 on a history-restore request and is a full page (see *Browser history* in the README).

## 2. Browser regression (opt-in, headless Chromium)

Playwright is not a dependency of this repository. Provide it yourself:

```sh
npm install --prefix ~/.cache/hextet-lg-browser playwright   # once; any prefix works
npx --prefix ~/.cache/hextet-lg-browser playwright install chromium
HEXTET_LG_BROWSER=1 NODE_PATH=~/.cache/hextet-lg-browser/node_modules \
  go test ./internal/web -run TestBrowserRegression -v -count=1
```

`NODE_PATH` is only where `require('playwright')` is resolved. Without Playwright, node or the env var the test is **skipped**, which is not a pass. `HEXTET_LG_BROWSER_SCENARIOS="history layout"` limits the run (`history`, `pagination`, `controls`, `optIn`, `layout`).

The test starts the application in-process on a loopback port with mock backends (nothing is exposed, nothing contacts a real router) and drives `internal/web/testdata/browser/regression.js`: the HTTP 400 history sequence (valid, rejected, Back, Forward, Reload, valid again), HTMX pagination with Back/Forward, advanced filters, themes, disclosures, opt-in Raw JSON and Neighbor column, hostile branding text, and every page at 1440/768/390/320 px (no horizontal page scroll, one title, footer timing, card layout at or below 700 px, no console or CSP errors, no request leaving the origin).

## 3. Live read-only compatibility check (opt-in)

```sh
HEXTET_LG_LIVE=1 HEXTET_LG_LIVE_CONFIG=configs/config.yaml \
  [HEXTET_LG_LIVE_EXPECT_VERSION=9.3] \
  [HEXTET_LG_LIVE_PREFIXES=192.0.2.0/24,2001:db8::1] [HEXTET_LG_LIVE_ASNS=64500] \
  go test ./internal/web -run TestLiveOpenBGPD -v -count=1
```

It reuses the backends of an existing configuration file, so no address or credential is committed. Read-only and bounded: the application runs in-process behind `httptest` (no listener), DNS enrichment, metrics and community definitions are off in memory only, the config file is never written, every request is a GET of `/summary`, `/neighbors`, `/rtr`, `/metrics` (version) or, only for probes you name, `/rib`; reads are size-capped and requests spaced 300 ms apart; at most 8 prefix and 8 ASN probes. Choose probes you know are small: a `/rib` answer is fully read. Results are `PASS`, `FAIL`, `LIM` (limitation: the API does not expose the item) or `SKIP` (no data to exercise it). Addresses, bodies and validator addresses are never logged.

Expected of the backend: `/summary`, `/neighbors` and `/rtr` answer 200 `application/json`; `/rtr` may be 404 (reported as *unsupported*); the version is read from the `bgpd_info{release="..."}` series of a bgpd exporter on `/metrics` when the deployment exposes one, otherwise it is operator-provided and the check says so.

## RTR compatibility and unsupported fields

Parsed from OpenBGPD 9.3 `/rtr`: `descr`, `remote_addr`, `remote_port`, `state`, `version`, `minimal_version`, `session_id`, `serial`, `refresh`, `retry`, `expire`, `timers`, and the `last_*` error fields when present. OpenBGPD does not report the last successful synchronization time or VRP/ASPA record counts, so none are shown. An established session without session ID and serial is not presented as healthy.

## Operational safety

Integration checks never change configuration, sessions, routes or policy, never restart anything and never write `configs/`. Use mock backends for failure injection and load; use the live check sparingly.
