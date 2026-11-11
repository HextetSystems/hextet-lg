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

`internal/web/m52_test.go`, `internal/bgp/client_test.go`, `internal/site/site_test.go` and `internal/config/m52_test.go` add the M5.2 security and deployment set:

- **HTTP surface.** Security headers and content types on every kind of response (pages, errors, 404/405, static, branding); no debug, metrics or raw-data route; `/static/` by exact name only, with validators and fingerprint-only immutability.
- **Input.** A table of malformed, oversized, repeated, non-ASCII, control-character and markup inputs is rejected with HTTP 400 in full pages, HTMX fragments and history restores, is never reflected unescaped and never reaches the mock backend; valid free-text values stay one encoded upstream parameter.
- **Backend client.** Redirects (301 to 308) are never followed for `/rib`, `/neighbors`, `/summary` or `/rtr`; the target is never contacted and nothing from it is rendered. Plaintext output neutralizes control characters in backend values.
- **Privacy defaults.** With `display.raw_json` off no page, fragment, error view, header or parameter yields raw backend JSON; with it on the payload is escaped text in its own disclosure and the response limit still applies. RTR validator addresses, ports and local addresses are absent from every response, attribute, enrichment request and log line unless `features.rtr_session_addresses` is on, and are never resolved in either setting.
- **Route exclusions.** IPv4 and IPv6, exact and or-longer: excluded routes (their prefixes, next hops, origin ASNs and raw objects) are absent from tables, text views, plaintext, pagination, counts, cached repeats, Raw JSON and both enrichment services.
- **Enrichment.** Hostile DNS answers are inert; a client cycling through many result pages while the resolver is stuck starts at most `max_concurrent` lookups per service and queues a fixed number; the status endpoints start none.
- **Branding and configuration.** Truncated and corrupt PNG/JPEG/GIF/ICO files and SVG with external references fail startup; no branding URL can name another file; a minimal configuration has every disclosure setting off, and unsafe or misspelled settings fail startup.
- **Deployment.** The application behind an in-process reverse proxy (path-only redirects and `HX-Push-Url`, ignored `X-Forwarded-*`, validators through the proxy, nothing about the client logged), the shared rate-limit bucket behind a proxy, and a create/use/close cycle that leaves no goroutine behind.

## 2. Browser regression (opt-in, headless Chromium)

Playwright is not a dependency of this repository, is never installed into it and has no `node_modules` here. Keep it in a dedicated directory outside the working tree; the documented location is `~/.cache/hextet-lg-browser`:

```sh
mkdir -p ~/.cache/hextet-lg-browser
npm install --prefix ~/.cache/hextet-lg-browser playwright
# Only if that installation has no Chromium yet (it reuses an existing one):
npx --prefix ~/.cache/hextet-lg-browser playwright install chromium
```

Run both browser suites (the pattern matches `TestBrowserRegression` and `TestBrowserSecurity`):

```sh
NODE_PATH="$HOME/.cache/hextet-lg-browser/node_modules" \
HEXTET_LG_BROWSER=1 \
go test -v ./internal/web -run 'Browser'
```

`NODE_PATH` is the only link between the tests and the installation: it is where `require('playwright')` is resolved, so any other directory that contains Playwright works by changing that one variable, and nothing in the repository names a user-specific path. Do not point it at another project's `node_modules`. Without `HEXTET_LG_BROWSER=1`, node or a resolvable Playwright the tests are **skipped**, which is not a pass; plain `go test ./...` never starts a browser. `HEXTET_LG_BROWSER_SCENARIOS="history layout"` limits a run to the named scenarios (regression: `history`, `pagination`, `controls`, `optIn`, `layout`; security: `defaults`, `csp`, `optIn`, `assets`, `reverseProxy`, `themes`).

The test starts the application in-process on a loopback port with mock backends (nothing is exposed, nothing contacts a real router) and drives `internal/web/testdata/browser/regression.js`: the HTTP 400 history sequence (valid, rejected, Back, Forward, Reload, valid again), HTMX pagination with Back/Forward, advanced filters, themes, disclosures, opt-in Raw JSON and Neighbor column, hostile branding text, and every page at 1440/768/390/320 px (no horizontal page scroll, one title, footer timing, card layout at or below 700 px, no console or CSP errors, no request leaving the origin). Baseline: 189 checks.

`TestBrowserSecurity` (M5.2) starts three more in-process instances behind a mock backend whose every text field is markup: one with default settings, one with every opt-in disclosure and hostile operator text, and the default one behind a reverse proxy on a different origin. `internal/web/testdata/browser/security.js` (114 checks) verifies that no dialog ever opens and nothing executable is injected on any page; that the Content-Security-Policy is present and actually enforced (an injected inline script, string evaluation, inline handler, `<style>`, off-origin image and framing are all refused and reported, while the application itself causes no violation); that with default settings no response body or DOM carries raw backend JSON or an RTR validator address; that the opt-in disclosures show hostile content as text; that the logo, favicon, stylesheets and scripts load from fingerprinted same-origin URLs; that navigation, HTMX history (including a rejected search) and Copy link work through the proxy with path-only references; and that both themes and the phone widths have no horizontal page scroll with hostile content and every disclosure open.

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

## Deployment validation (manual, isolated)

The container, proxy and lifecycle checks of M5.2 are not automated in CI beyond the image build; they were run by hand against an isolated Compose project and can be repeated the same way. The rules: a unique project name and image tag, loopback-only host ports other than 7000 and 8080, a mock backend, temporary configuration outside `configs/`, and removal of only what the run created (`docker compose -p <name> down` and `docker image rm <tag>`; never a prune).

```sh
docker build -t hextet-lg-m52:test .
docker compose -p hextet-lg-m52 -f /path/to/temporary/compose.yaml up -d   # app on 127.0.0.1:18080, mock backend, optional TLS proxy
curl -fsS http://127.0.0.1:18080/healthz
docker compose -p hextet-lg-m52 -f /path/to/temporary/compose.yaml down
docker image rm hextet-lg-m52:test
```

What to observe: `healthy` status; pages and custom branding served; UID 10001, no capabilities, read-only root with only `/tmp` and `/dev/shm` writable and `/tmp` left empty; read-only configuration and branding mounts; JSON logs without addresses or queries; a clear one-line error and non-zero exit for an invalid configuration; a failed backend recovering without a restart; `docker compose stop` during a slow query letting that request finish and exiting 0; and the same behaviour through a TLS-terminating proxy reached over `127.0.0.1` and `[::1]`.

## Operational safety

Integration checks never change configuration, sessions, routes or policy, never restart anything and never write `configs/`. Use mock backends for failure injection and load; use the live check sparingly.
