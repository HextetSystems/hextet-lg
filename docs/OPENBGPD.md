# OpenBGPD collector for Hextet LG

This example runs the versioned `openbgpd/openbgpd:9.3` image as a collector and its bundled `bgplgd` API. OpenBGPD 9.3 is the current portable release. The container image entrypoint starts `bgpd`, `bgplgd -d`, and HAProxy together; `bgplgd` is not enabled by publishing port 9099 alone. HAProxy exposes the API on that port, and the image supplies the restricted control socket used by `bgplgd`.

## 1. Create the files

```sh
sudo mkdir -p /srv/docker/compose/openbgpd /srv/docker/data/openbgpd
cd /srv/docker/compose/openbgpd
sudoedit /srv/docker/data/openbgpd/bgpd.conf
```

Save this minimal `bgpd.conf` (replace every example ASN and address with values assigned to your network and peer):

```conf
# EXAMPLE ONLY: replace with your assigned ASN and router ID.
AS 65000
router-id 192.0.2.1

# EXAMPLE ONLY: replace with the intended peer's address and ASN.
neighbor 198.51.100.2 {
	remote-as 65001
	descr "example-peer"
}

# Accept routes received from this peer.
allow from 198.51.100.2
```

The addresses above are documentation examples, not reachable infrastructure. Coordinate the local/remote ASNs, peer address, and any authentication or routing policy with the peer operator. This deliberately has no `allow to` rule, so it does not advertise routes.

Create `/srv/docker/compose/openbgpd/compose.yaml`:

```yaml
services:
  openbgpd:
    image: openbgpd/openbgpd:9.3
    container_name: openbgpd
    restart: unless-stopped
    ports:
      - "179:179/tcp"
      # Local-only by default. Set OPENBGPD_API_BIND to a trusted host
      # interface address only when remote Hextet LG access is required.
      - "${OPENBGPD_API_BIND:-127.0.0.1}:9099:9099/tcp"
    volumes:
      - /srv/docker/data/openbgpd:/etc/bgpd:ro
    tmpfs:
      - /run/bgpd
```

Start it and check the container:

```sh
cd /srv/docker/compose/openbgpd
sudo docker compose up -d
sudo docker compose ps
```

The configuration directory is the persistent host data at `/srv/docker/data/openbgpd`; `/run/bgpd` is temporary runtime state for the control socket. The image starts the API stack automatically when its normal `bgpd` command runs. Do not override the image command or entrypoint for this setup.

## 2. Check the peer and API

Inspect BGP sessions from inside the container. A healthy session should show `Established`:

```sh
sudo docker compose exec openbgpd bgpctl show summary
```

Check the JSON API from the Docker host:

```sh
curl -fsS http://127.0.0.1:9099/neighbors
curl -fsS http://127.0.0.1:9099/rib
```

The `/neighbors` and `/rib` endpoints return JSON through the image's HAProxy-to-`bgplgd` setup. A responding API does not mean routes are available: the collector must establish a BGP session and receive routes before Hextet LG can display them. Inspect received routes with `sudo docker compose exec openbgpd bgpctl show rib in`.

## 3. Point Hextet LG at it

In the Hextet LG runtime YAML, configure the backend using the project's schema:

```yaml
backends:
  - id: collector
    name: "Example collector"
    location: "Example location"
    url: "http://192.0.2.10:9099"
    enabled: true
```

Replace `192.0.2.10` with the private host address reachable from Hextet LG. Set `OPENBGPD_API_BIND` to that host interface address before `docker compose up -d` so 9099 listens there, and allow that port only from trusted looking-glass clients. If Hextet LG is on a shared user-defined Docker network, it can instead use `http://openbgpd:9099`; attach both projects to that network and keep 9099 unpublished on the host. Do not add backend addresses to public request parameters.

## Optional: RTR/RPKI

RTR is not required for BGP collection or the JSON API. To add origin validation later, run an RTR-speaking validator and add an `rtr 192.0.2.100` directive to `bgpd.conf` using the validator's actual address (and any required non-default port). Follow that validator's authentication and network guidance. The image also provides `/var/lib/rpki-client` for validated ROA payload files when using `rpki-client` output instead. Do not enable either until a validator is configured.

## Network access

Restrict TCP 9099 to trusted Hextet LG clients; the API is not an authenticated public service. Restrict TCP 179 to intended BGP peers only, using host and upstream firewalls or security groups. Do not expose either port broadly. Review the selected image's current [Docker Hub documentation](https://hub.docker.com/r/openbgpd/openbgpd) and [container entrypoint](https://github.com/openbgpd/openbgpd-container/blob/master/entrypoint.sh) when upgrading, since the API startup and proxying are image behavior.
