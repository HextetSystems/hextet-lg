# syntax=docker/dockerfile:1

# One runtime image, two ways to obtain the binary:
#
#   BINARY_SOURCE=compiled  (default) build from source in this file. Used by
#                           `docker compose build`, `make docker` and CI.
#   BINARY_SOURCE=prebuilt  copy the binary GoReleaser already built, from
#                           <TARGETPLATFORM>/hextet-lg in the build context.
#                           Used only by .goreleaser.yaml (dockers_v2), so the
#                           published image contains the exact binary that is in
#                           the release archive.
ARG BINARY_SOURCE=compiled

# Always runs on the build host and cross-compiles, so building the arm64 image
# never needs QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS compiled
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath \
    -ldflags="-s -w \
      -X github.com/HextetSystems/hextet-lg/internal/version.Version=${VERSION} \
      -X github.com/HextetSystems/hextet-lg/internal/version.Commit=${REVISION} \
      -X github.com/HextetSystems/hextet-lg/internal/version.Date=${CREATED}" \
    -o /out/hextet-lg ./cmd/hextet-lg

FROM scratch AS prebuilt
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/hextet-lg /out/hextet-lg

FROM ${BINARY_SOURCE} AS binary

FROM alpine:3.24
ARG VERSION=dev
ARG REVISION=unknown
ARG CREATED=unknown
LABEL org.opencontainers.image.title="hextet-lg" \
      org.opencontainers.image.description="Hextet Looking Glass: a web looking glass for OpenBGPD (bgplgd)" \
      org.opencontainers.image.source="https://github.com/HextetSystems/hextet-lg" \
      org.opencontainers.image.url="https://github.com/HextetSystems/hextet-lg" \
      org.opencontainers.image.documentation="https://github.com/HextetSystems/hextet-lg#readme" \
      org.opencontainers.image.licenses="BSD-2-Clause" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}"
COPY --from=binary --chmod=0555 /out/hextet-lg /hextet-lg
# A numeric, non-root identity needs no passwd entry (and no RUN step, which
# would require emulation to build the non-native architecture).
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/hextet-lg"]
CMD ["--config", "/etc/hextet-lg/config.yaml"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
