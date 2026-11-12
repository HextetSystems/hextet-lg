# Testing

The default test suite uses mock backends and does not require a running OpenBGPD service or external network access. CI runs the formatting, build, Go, race-detector, and JavaScript checks below.

## Standard checks

From the repository root:

```sh
go fmt ./...
go mod verify
go vet ./...
go build ./cmd/hextet-lg
go test ./...
go test -race ./...
node --test internal/web/testdata/*.test.js
```

Tests are organized by package and feature. HTTP tests use local mock backends; fixtures that represent backend responses are sanitized to documentation addresses, private ASNs, and placeholder descriptions. Do not commit real backend responses or production data.

## Optional browser checks

The browser regression and security tests use Playwright with Chromium. Playwright is not a project dependency; install it outside the repository:

```sh
export PLAYWRIGHT_DIR=/path/outside/the/repository
npm install --prefix "$PLAYWRIGHT_DIR" playwright
npx --prefix "$PLAYWRIGHT_DIR" playwright install chromium
NODE_PATH="$PLAYWRIGHT_DIR/node_modules" HEXTET_LG_BROWSER=1 \
  go test -v ./internal/web -run Browser
```

These tests start the application in-process with mock backends. They cover browser history, search controls, responsive layouts, themes, disclosures, and security behavior without contacting a live router. Without `HEXTET_LG_BROWSER=1` or a usable Playwright installation, the browser tests are skipped; report skips as skips, not passes.

## Optional live compatibility check

The live check can compare the application with a configured OpenBGPD backend. Run it only against a backend you are authorized to query. It sends read-only `GET` requests; route probes are sent only when you explicitly provide them, so choose a small number of known, bounded queries.

```sh
HEXTET_LG_LIVE=1 \
HEXTET_LG_LIVE_CONFIG=/path/to/config.yaml \
go test ./internal/web -run TestLiveOpenBGPD -count=1 -v
```

Optionally set `HEXTET_LG_LIVE_PREFIXES` or `HEXTET_LG_LIVE_ASNS` to provide route probes, and `HEXTET_LG_LIVE_EXPECT_VERSION` to select the expected OpenBGPD release. The test loads the named configuration without writing it and disables optional enrichment and metrics in memory. It reports unavailable or unsupported backend behavior as a limitation, and does not log backend addresses or response bodies.
