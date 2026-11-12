# Developing Hextet LG

Use the Go version declared in [`go.mod`](go.mod). Go tests use local mock backends by default; a live OpenBGPD service is not required.

## Run locally

Create a local config if needed, set its backend URL, and change `server.listen` to `127.0.0.1:7000`:

```sh
mkdir -p configs
test -e configs/config.yaml || cp examples/config.example.yaml configs/config.yaml
go run ./cmd/hextet-lg --config configs/config.yaml
```

Open <http://localhost:7000>. Runtime configuration under `configs/` is local and ignored by Git.

## Validate changes

From the repository root:

```sh
go fmt ./...
go mod verify
go vet ./...
go test ./...
go test -race ./...
node --test internal/web/testdata/*.test.js
```

Browser regression tests and live OpenBGPD compatibility checks are opt-in. Their setup and behavior are documented in [docs/TESTING.md](docs/TESTING.md); skipped checks should be reported as skipped, not as passing.

## Contribution guidelines

- Keep changes focused and straightforward; prefer Go standard-library solutions where practical.
- Organize tests by package and feature, not by development milestone.
- Preserve IPv4 and IPv6 support, security boundaries, and bounded resource usage.
- Update relevant tests and documentation when behavior changes.
- Avoid new frameworks, dependencies, or tooling without a demonstrated need.
- Do not include production configuration, backend responses, or private network details.
- Do not stage, commit, tag, push, deploy, or publish unless explicitly instructed.

For the project structure and request flow, see [ARCHITECTURE.md](ARCHITECTURE.md). Configuration options are documented in [`examples/config.example.yaml`](examples/config.example.yaml).
