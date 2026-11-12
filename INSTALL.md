# Install Hextet LG

Hextet LG supplies a read-only web interface for the `openbgpd/openbgpd` Docker image, replacing the CGI looking-glass frontend that the image does not include. It connects to the image's `bgplgd` HTTP API; it does not install or manage OpenBGPD. The complete, documented application configuration is [`examples/config.example.yaml`](examples/config.example.yaml).

## Docker Compose

You need Docker Engine with the Compose plugin and a `bgplgd` API reachable from the Hextet LG container.

```sh
mkdir -p configs
test -e configs/config.docker.yaml || cp examples/config.example.yaml configs/config.docker.yaml
```

Edit `configs/config.docker.yaml` and set each backend URL to an address reachable from the container. Then start Hextet LG:

```sh
docker compose config -q
docker compose up -d --build
```

The application listens on port 8080. Open <http://localhost:8080>. Compose mounts the runtime configuration read-only and runs the application as a non-root user.

For production, use a released image pinned by digest and follow the [upgrade and rollback guide](https://github.com/HextetSystems/hextet-lg/blob/main/docs/RELEASING.md).

## Linux binary

Download the Linux amd64 or arm64 archive and `checksums.txt` from [GitHub Releases](https://github.com/HextetSystems/hextet-lg/releases). Verify and unpack the archive, then create a runtime configuration:

```sh
sha256sum -c --ignore-missing checksums.txt
tar xzf hextet-lg_<version>_linux_amd64.tar.gz
test -e config.yaml || cp examples/config.example.yaml config.yaml
```

Edit `config.yaml` with the backend URL and listen address for your host, then run:

```sh
./hextet-lg --config config.yaml
```

Use the arm64 archive name on ARM64 systems. See the [release and deployment guide](https://github.com/HextetSystems/hextet-lg/blob/main/docs/RELEASING.md) for artifact details.

## Before public access

Hextet LG is a read-only interface, but its configured `bgplgd` API is an unauthenticated management interface. Keep the API on a trusted network. Put the web application behind a TLS-terminating reverse proxy and do not expose its application port directly alongside the proxy. The optional metrics listener is separate; keep it on a trusted management network.

Read [docs/SECURITY.md](docs/SECURITY.md) before exposing an instance publicly. For an example OpenBGPD collector setup, see [docs/OPENBGPD.md](docs/OPENBGPD.md).
