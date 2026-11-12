# Releasing and deploying Hextet LG

Releases are produced by **GoReleaser Community Edition** (`.goreleaser.yaml`) and run by **GitHub Actions**. Binaries and checksums go to **GitHub Releases**; container images go to **GitHub Container Registry** as `ghcr.io/hextetsystems/hextet-lg`. Deployment is plain **Docker Compose**.

## Versioning

Versions are [Semantic Versions](https://semver.org) and **Git tags are the only source of the build version**: `vMAJOR.MINOR.PATCH` for a stable release, `vMAJOR.MINOR.PATCH-rc.1` (any `-prerelease` suffix) for a prerelease. GoReleaser generates release notes from Git commits since the previous tag using the sorting and filters in `.goreleaser.yaml`. The manually maintained `CHANGELOG.md` is informational and is not a release gate.

| Build | How | `hextet-lg version` | Image |
|---|---|---|---|
| Development | `go build`, `go run`, `make build` | `dev`, with the commit and time Go embeds when built inside a Git checkout (`-dirty` if modified), else `unknown` | – |
| Local image | `docker compose build`, `make docker` | `dev` with the commit passed by `make docker` | `hextet-lg:local` (mutable, never a release) |
| Snapshot | `make snapshot` / `goreleaser release --snapshot --clean` | snapshot version derived from the current Git state | local only; no image is pushed |
| Prerelease | tag `vX.Y.Z-rc.1` | `vX.Y.Z-rc.1` | matching version and `:sha-<commit>`; **never** `:latest`; GitHub Release marked *pre-release* |
| Stable | tag `vX.Y.Z` | `vX.Y.Z` | matching version, `:sha-<commit>`, `:latest` |

`hextet-lg version` prints:

```text
hextet-lg vX.Y.Z
commit: abc1234
built: 2026-11-10T00:00:00Z
go: go1.27.3
```

The same values appear in the `listening` log record, the `org.opencontainers.image.*` labels (`version`, `revision` = full commit, `created`) and the `hextet_lg_build_info{version,revision,go_version} 1` metric (one series). The `go:` line is the toolchain that built the binary.

## Building

Requirements: Go 1.26 or newer (the version in `go.mod`). Docker with Buildx is needed for image builds; GoReleaser v2 (CI pins `v2.20.0`) for release builds and snapshots.

```sh
go build ./cmd/hextet-lg && ./hextet-lg version   # development binary
make docker                                       # local image hextet-lg:local (development version)
goreleaser check                                  # validate .goreleaser.yaml
make snapshot                                     # local release build into dist/; publishes nothing, needs no credentials
```

A snapshot produces, in `dist/`:

```text
hextet-lg_<version>_linux_amd64.tar.gz
hextet-lg_<version>_linux_arm64.tar.gz
checksums.txt                        # SHA-256, verify with: cd dist && sha256sum -c checksums.txt
```

Snapshot mode also asks Buildx to assemble the multi-architecture image locally; it does not push it. Each release executable embeds the example YAML, printable with `hextet-lg --print-config`; archives also include `examples/config.example.yaml` beside the binary, plus `README.md`, `ARCHITECTURE.md`, `docs/RELEASING.md`, the listed operator guides, community examples and any `LICENSE*` file. Runtime configuration is never packaged: the archive file list is an explicit allow-list. The archive version omits the leading `v` (for example, `vX.Y.Z` becomes `hextet-lg_X.Y.Z_linux_amd64.tar.gz`). `dist/` is ignored by Git.

Binaries are static (`CGO_ENABLED=0`, `-trimpath`) for linux/amd64 and linux/arm64. Version, commit and build time are injected by GoReleaser with linker flags (`internal/version`).
The build date is an input, so builds with different dates are not expected to be byte-identical. Compare checksums from the same published release rather than assuming independently timed builds match.

## Container image

One `Dockerfile` defines the runtime image (Alpine, UID/GID 10001, no capabilities needed, read-only root filesystem compatible, `HEALTHCHECK`, port 8080, `--config /etc/hextet-lg/config.yaml`). It has two ways of obtaining the binary, selected by `BINARY_SOURCE`:

* `compiled` (default): built from source, cross-compiled on the build host (no QEMU), used by Compose, `make docker` and CI.
* `prebuilt`: copies the binary GoReleaser already built, so the published image contains the **same executable** that is in the release archive. Only `.goreleaser.yaml` selects it.

GoReleaser's `dockers_v2` drives Buildx to build and push **one multi-architecture manifest list per tag**, so a version tag pulls the right architecture on amd64 and arm64 hosts. No separate manifest step exists; the workflow only provides Buildx, the registry login and, afterwards, records the digest.

| Tag | Meaning | Mutable? |
|---|---|---|
| `ghcr.io/hextetsystems/hextet-lg:vX.Y.Z` | release | treat as stable, but a registry tag can be re-pushed |
| `…:sha-<7 hex>` | the exact commit | built once per commit |
| `…:latest` | newest **stable** release | moves; never use for production or rollback |
| `…@sha256:<digest>` | one exact manifest | **immutable; use this in production** |

Labels: `org.opencontainers.image.{title,description,source,url,documentation,licenses,version,revision,created}`. The `licenses` label is `BSD-2-Clause`.

## CI/CD

**`.github/workflows/ci.yaml`** runs on pull requests, pushes to `main` and (as a reusable workflow) from the release workflow. It needs no secrets and no access to OpenBGPD, Prometheus, Cloudflare or any private network. Jobs:

* *Go checks*: run on pull requests, pushes to `main`, and release-tag validation. They include formatting, `go mod verify`, `go vet`, `go build`, `go test ./...`, `go test -race ./...`, and the JavaScript tests, using the Go version declared in `go.mod` (latest patch). The tests use `httptest` backends; the five-minute container load exercise is **not** part of CI.
* *Docker image and Compose*: run on pull requests, pushes to `main`, and release-tag validation. They build linux/amd64 and linux/arm64 from the Dockerfile, smoke-test the amd64 image (version, `/healthz`, `/readyz`, `/metrics` is 404, UID 10001, read-only root, all capabilities dropped, clean stop) with the committed example configuration, and run `docker compose config -q` for the default and a digest-pinned image.
* *GoReleaser*: runs on pull requests and release-tag validation, but is skipped on pushes to `main` because the pull request already checked packaging and the release-tag run checks it again. It runs `goreleaser check`, then `release --snapshot --clean`, verifies checksums and archive contents, and checks `hextet-lg version` from an archived binary.

**`.github/workflows/release.yaml`** runs only for pushed tags matching `v*`:

1. `validate-tag`: the tag must be `vMAJOR.MINOR.PATCH[-prerelease]` and its commit must be reachable from `main`.
2. `ci`: the full CI workflow runs on the tagged commit.
3. `release` (needs both): checkout at the tag, Go, Buildx, `docker/login-action` to `ghcr.io` with the built-in `GITHUB_TOKEN`, `goreleaser release --clean` (including generated release notes), then the digest of the pushed manifest list is read back with `docker buildx imagetools inspect`, written to the job summary and attached to the GitHub Release as `image-digests.txt`.

Permissions are `contents: read` for the workflow; only the `release` job gets `contents: write` and `packages: write`. No personal access tokens are used. Pull requests cannot reach the release workflow, and because `release` needs `ci`, a failing test cannot produce a release. Third-party actions are pinned to commit SHAs (version in the trailing comment); update them deliberately (Dependabot is configured for this).

### Cutting a release (maintainer)

```sh
git switch main && git pull --ff-only
git tag -s vX.Y.Z -m "Hextet LG vX.Y.Z"     # annotated and signed
git push origin vX.Y.Z                       # starts the release workflow
```

Use `-rc.N` for candidates; GoReleaser generates their release notes from commits since the preceding tag, just like stable releases. To retract a bad release: delete the GitHub Release and the tag, publish a new patch version. Do not re-push an existing version tag; consumers pinned by digest are unaffected, those on the tag are not.

### GitHub-side settings (one-time)

* **Actions → General → Workflow permissions**: the workflows declare their own permissions; leaving the default read-only is fine.
* **Packages**: the first push creates the `hextet-lg` package. It is linked to the repository through the `org.opencontainers.image.source` label. A new package may be **private**; set its visibility to *Public* (Package settings → Danger Zone) so hosts can pull without credentials, and check that the repository has *Write* access under *Manage Actions access*.
* **Rulesets**: protect `main` (pull requests, required checks *Go checks*, *Docker image and Compose*, *GoReleaser configuration and snapshot*) and restrict who can create `v*` tags.

## Deployment

### Standalone

Download and verify the archive, install the binary, create the configuration from the example, run it.

```sh
sha256sum -c --ignore-missing checksums.txt
tar xzf hextet-lg_<version>_linux_amd64.tar.gz
cp examples/config.example.yaml /etc/hextet-lg/config.yaml   # then set backend URLs
./hextet-lg version
./hextet-lg --config /etc/hextet-lg/config.yaml
```

The process logs JSON to stderr and shuts down gracefully on SIGTERM/SIGINT (`server.shutdown_grace`, default 40 s).

### Docker Compose

`compose.yaml` selects the image with `HEXTET_LG_IMAGE` (default `hextet-lg:local`, the development workflow). Private configuration stays in the ignored `configs/config.docker.yaml`, mounted read-only; nothing here changes it, the listener port (8080), the unpublished metrics listener, the security restrictions (non-root, read-only root, `cap_drop: ALL`, `no-new-privileges`, `pids_limit`), the health check or `stop_grace_period: 45s`. Put the selection in an untracked `.env` (see `.env.example`) or the shell:

```sh
HEXTET_LG_IMAGE=ghcr.io/hextetsystems/hextet-lg@sha256:<digest> docker compose up -d --no-build --pull never
```

Find the digest in the release's `image-digests.txt`, the workflow summary, or with `docker buildx imagetools inspect ghcr.io/hextetsystems/hextet-lg:<version-tag>`.

### Upgrade and rollback

The **local tag `hextet-lg:local` is mutable and is not a rollback reference**: it is moved by every `docker compose build`, and the image a running container was created from can already be gone from the local store. The rollback target is therefore always the *image ID of the running container*, preserved under a fresh, immutable local tag before replacement.

`scripts/deploy.sh` automates this with native Compose commands:

```sh
scripts/deploy.sh status                                    # running image id, labels, digest, ports, history
scripts/deploy.sh upgrade ghcr.io/hextetsystems/hextet-lg@sha256:<digest> --dry-run
scripts/deploy.sh upgrade ghcr.io/hextetsystems/hextet-lg@sha256:<digest>     # asks for confirmation (or --yes)
scripts/deploy.sh rollback                                  # back to the image the last upgrade replaced
scripts/deploy.sh rollback hextet-lg:rollback-20261110T051903Z   # or to an explicit image
```

`upgrade` refuses to run without an explicit image, validates the Compose configuration and that the target exists (pulling it, or checking the registry in a dry run), prints old and new image id, version and digest, tags the running image as `hextet-lg:rollback-<UTC time>`, recreates the one service with `docker compose up -d --no-build --no-deps --pull never`, then waits for the Docker health check and `/readyz`, checks that only 8080 is published and that the public listener answers `/metrics` with 404, and reports whether logs are structured. If any required check fails it rolls back automatically (use `--no-auto-rollback` to keep the failed container for inspection) and exits non-zero. On success it records the selection in `.env` (`HEXTET_LG_IMAGE=…`) so that a later plain `docker compose up` does not silently revert, and appends to `.deploy-state/history.tsv`. It never touches `configs/`, builds, prunes or deletes images. If the running container's image is no longer in the local store it stops and explains; `--preserve-current` then snapshots the running container with `docker commit` as the rollback image. Expect a short outage while the container is stopped and recreated (up to the 45 s grace period, typically a few seconds when idle).

The same steps by hand:

```sh
# 1. record and preserve what is running
docker inspect hextet-lg --format 'image={{.Image}} configured={{.Config.Image}}'
docker tag <image id from above> hextet-lg:rollback-$(date -u +%Y%m%dT%H%M%SZ)
# 2. select and check the target
export HEXTET_LG_IMAGE=ghcr.io/hextetsystems/hextet-lg@sha256:<digest>
docker pull "$HEXTET_LG_IMAGE" && docker compose config -q
# 3. replace
docker compose up -d --no-build --no-deps --pull never hextet-lg
# 4. verify
docker compose ps                                   # (healthy)
docker exec hextet-lg wget -qO- http://127.0.0.1:8080/readyz
docker port hextet-lg                               # only 8080/tcp
docker logs --tail 5 hextet-lg                      # JSON lines
# 5. roll back if needed
HEXTET_LG_IMAGE=hextet-lg:rollback-<time> docker compose up -d --no-build --no-deps --pull never hextet-lg
```

Rollback images are never deleted automatically; remove old `hextet-lg:rollback-*` and `hextet-lg:replaced-*` tags yourself when they are no longer needed.

## Operations summary

* **Configuration**: runtime YAML is private and ignored by Git; `examples/config.example.yaml` is the documented example. Backend URLs come only from YAML.
* **Health**: `/healthz` (liveness, independent of backends) and `/readyz` (503 once shutdown begins); the image and Compose both define the health check.
* **Metrics**: optional (`metrics.enabled`), separate listener, never published by Compose; scrape from a collector on a shared Docker network.
* **Logging**: JSON on stderr (`logging.level`, `logging.format`), never including queries, prefixes, ASNs, headers, client addresses or backend URLs.
* **Shutdown**: SIGTERM, `server.shutdown_grace` (40 s) below the 45 s `stop_grace_period`.
