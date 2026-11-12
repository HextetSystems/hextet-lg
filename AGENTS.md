# Contributor guide

Hextet LG is a read-only Go web application that presents routes and session information from configured OpenBGPD `bgplgd` HTTP APIs. The interface uses Go `html/template`, embedded CSS and JavaScript, and HTMX to enhance ordinary forms and links. It has no database or persistent route store. `ARCHITECTURE.md`, `README.md`, and the focused documents under `docs/` describe behavior in more detail; keep this guide general and current.

## Code organization

- `cmd/hextet-lg`: command-line entry point, configuration loading, and process lifecycle.
- `internal/config`: YAML decoding, defaults, and startup validation.
- `internal/bgp`: backend HTTP client, API types, route decoding, and query validation/canonicalization.
- `internal/web`: HTTP handlers, presentation, templates, embedded assets, request controls, and enrichment integration.
- `internal/{asn,ptr}`: optional bounded DNS enrichment services.
- `internal/{communities,site,theme,visibility,obs,version}`: focused packages for community definitions, deployment identity and assets, theme generation, route visibility, observability, and build identity.
- `internal/web/templates` and `internal/web/static`: page templates and frontend assets, embedded through `internal/web/embed.go`.
- `internal/*/*_test.go` and `internal/web/testdata`: Go tests, browser test scripts, and sanitized backend fixtures.
- `examples/`: sample runtime configuration and community data. `docs/`: user, operator, security, architecture, and testing guidance.

Keep functionality in the package that owns it. Keep backend transport models separate from presentation models, and avoid adding dependencies, layers, or generic abstractions without a clear need.

## Go and web conventions

- Use the module `github.com/HextetSystems/hextet-lg`. Prefer the Go standard library for HTTP, URL handling, JSON, templates, validation, and concurrency where practical. Keep dependencies small; do not add vendoring or `go.work`.
- Format Go changes with `gofmt`. Follow existing error handling, context propagation, and package boundaries.
- Validate addresses with `net/netip` and preserve IPv4 and IPv6 behavior throughout parsing, queries, display, and tests. Do not infer a route's family from its next hop.
- Use the configured bgplgd client and its fixed supported endpoints. Backend URLs come only from validated configuration. Keep requests bounded by contexts/timeouts, response and retained-memory limits, and existing concurrency controls. Do not fetch a whole RIB for local filtering or add generic AS-path/customer-cone features.
- Treat route results as read-only, per-backend data. Preserve completion and failure states; partial or failed responses must not look like complete empty results. Keep route validity, RPKI, and ASPA states distinct.
- Render HTML with `html/template`; keep templates and assets under `internal/web`. Prefer progressive enhancement: ordinary navigation and forms should work without JavaScript, while HTMX may update results and history. Do not concatenate untrusted text into markup or weaken the Content Security Policy to support a feature.
- Keep public URLs canonical and bookmarkable. Reject unknown or repeated parameters rather than silently selecting one. Serve static assets only through the existing exact-name embedded asset handler.

## Security and resource boundaries

Browser input, backend responses, and DNS answers are untrusted. Validate and bound public input; escape backend, operator, and DNS text; do not expose arbitrary proxy, debug, raw-data, or backend-address controls. Never follow backend redirects. Avoid logging queries, addresses, credentials, headers, backend URLs, or response bodies; follow the existing `log/slog` conventions.

Preserve bounded request parsing, rate limiting, response sizes, retained memory, concurrency, cache accounting, and graceful shutdown. Optional ASN and PTR enrichment is descriptive only: keep work asynchronous and bounded, use the existing services and caches, and keep status endpoints cache-only. Enabling DNS enrichment can disclose eligible ASNs or addresses to DNS operators; the feature remains optional and documented.

Configuration is strict and validated at startup. Keep old configurations working when adding optional settings, document defaults in `examples/config.example.yaml`, and update the relevant operator documentation. Keep `configs/` local and ignored; never commit runtime backend addresses or real backend responses. Sanitize fixtures to documentation addresses and private ASNs.

## Tests and documentation

Organize tests by package and feature/functionality, not by development milestone. Extend the closest existing tests and fixtures when behavior changes; add focused regression coverage for relevant success, invalid-input, security, IPv4/IPv6, and resource-limit cases. Update the relevant README or `docs/` page when user-visible behavior, configuration, security, or operations change. Do not add a test framework or other tooling without a demonstrated need.

The deterministic validation commands are documented in `docs/TESTING.md` and run in CI:

```sh
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
node --test internal/web/testdata/*.test.js
```

Browser and live OpenBGPD checks are opt-in; follow `docs/TESTING.md` for prerequisites and invocation. A skipped opt-in check is not a passing check. For a documentation-only change, use the validations appropriate to the change and report what was run.

## Change discipline

Make focused changes that solve the requested problem with the smallest clear implementation. Preserve existing behavior unless the change calls for a behavior change; avoid unrelated cleanup, new frameworks, and speculative features. Review the diff for scope, formatting, security implications, and documentation accuracy. Do not stage, commit, tag, push, deploy, or publish unless explicitly instructed.
