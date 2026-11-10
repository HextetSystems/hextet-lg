FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/hextet-lg ./cmd/hextet-lg

FROM alpine:3.21
RUN addgroup -S -g 10001 app && adduser -S -D -H -u 10001 -G app app
COPY --from=build /out/hextet-lg /hextet-lg
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/hextet-lg"]
CMD ["--config", "/etc/hextet-lg/config.yaml"]
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
