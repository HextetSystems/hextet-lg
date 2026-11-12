# OpenBGPD collector for Hextet LG

This example uses the `openbgpd/openbgpd:latest` image as a collector and its bundled `bgplgd` API. At the time of this update, `latest` resolves to OpenBGPD 9.3, and our compatibility coverage extends through 9.3. The tag can move as the upstream image is updated, so confirm compatibility when pulling a newer image. The container image entrypoint starts `bgpd`, `bgplgd -d`, and HAProxy together; `bgplgd` is not enabled by publishing port 9099 alone. HAProxy exposes the API on that port, and the image supplies the restricted control socket used by `bgplgd`.

For complete syntax and version-specific behavior, consult the official OpenBSD [`bgpd.conf(5)` man page](https://man.openbsd.org/bgpd.conf).

## 1. Create the files

```sh
mkdir -p ~/docker-compose/openbgpd/config
cd ~/docker-compose/openbgpd
${EDITOR:-vi} config/bgpd.conf
```

Save this collector-oriented `bgpd.conf`. Replace the example ASNs and addresses with values assigned to your network and peer, and remove optional sections you do not use:

```conf
# EXAMPLE ONLY: use your assigned ASN and a unique router ID.
AS 65000
router-id 192.0.2.1

# This host is a looking glass, not a forwarding router. Keep BGP-learned
# routes in bgpd's RIB for the API without installing them in the kernel FIB.
fib-update no

# If this collector does not run an IGP or have static routes to the BGP
# next hops it receives, allow nexthop validation through the system default
# route. The host must have a suitable default route.
nexthop qualify via default

# Accept routes received from configured BGP neighbors. "any" is a filter
# match; it does not create neighbors or open a session by itself.
allow from any

# Do not advertise routes to any neighbor. Keep this explicit for a read-only
# looking glass; do not add a later, more specific "allow to" rule.
deny to any

# EXAMPLE ONLY: replace with the intended peer's address and remote ASN.
neighbor 198.51.100.2 {
	remote-as 65001
	descr "example-peer"

	# Enable the address families this peer should send to the collector.
	# Replace or remove these to match the peer and your deployment.
	announce IPv4 unicast
	announce IPv6 unicast

	# OPTIONAL: receive multiple paths for a prefix when the peer also
	# supports and sends Add-Path. The default is no.
	announce add-path recv yes
}

# OPTIONAL: connect to RTR validators for RPKI data. This sample requests RTR
# v2 for ASPA synchronization; remove min-version if you only need RTR sessions
# or ROA origin validation. Use the configured validator port (default 323).
# Descriptions appear on Hextet LG's /rtr page, so choose safe-to-publish text.
rtr 192.0.2.100 {
	descr "validator-a"
	port 3323
	min-version 2
}

# OPTIONAL: a second validator can provide another RTR data source.
rtr 192.0.2.101 {
	descr "validator-b"
	port 3323
	min-version 2
}

# The OpenBGPD container's bgplgd process uses this read-only control socket.
# Keep the path in sync with the container image and retain "restricted".
socket "/run/bgpd/bgpd.rsock" restricted
```

The addresses above are documentation examples, not reachable infrastructure. Coordinate the local and remote ASNs, peer address, address families, and any authentication or routing policy with the peer operator. `allow from any` accepts routes from configured neighbors; it does not configure a neighbor or permit outbound announcements. `deny to any` explicitly rejects outbound updates. OpenBGPD filters default to deny, and the last matching rule decides the result, so review the full filter order if you add other rules.

`fib-update no` prevents learned routes from being installed in the host's forwarding table; they remain in bgpd's RIB for `bgplgd` to query. `nexthop qualify via default` is useful when the collector has no IGP and should validate next hops through its system default route. `announce add-path recv yes` is optional: it can retain multiple received paths per prefix only when the peer also negotiates and sends Add-Path.

Create `~/docker-compose/openbgpd/compose.yaml`:

```yaml
services:
  openbgpd:
    image: openbgpd/openbgpd:latest
    container_name: openbgpd
    restart: unless-stopped
    ports:
      - "179:179/tcp"
      # Local-only by default. Set OPENBGPD_API_BIND to a trusted host
      # interface address only when remote Hextet LG access is required.
      - "${OPENBGPD_API_BIND:-127.0.0.1}:9099:9099/tcp"
    volumes:
      - ./config:/etc/bgpd:ro
    tmpfs:
      - /run/bgpd
```

Start it and check the container:

```sh
cd ~/docker-compose/openbgpd
sudo docker compose up -d
sudo docker compose ps
```

The `config/` directory beside `compose.yaml` is mounted into the container as `/etc/bgpd`; `/run/bgpd` is temporary container runtime state for the control socket. The image starts the API stack automatically when its normal `bgpd` command runs. Do not override the image command or entrypoint for this setup.

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

RTR is not required for BGP collection or the JSON API. Configure one or more RTR-speaking validators if you want RPKI validation data. RTR v2 is not required for an RTR session or ROA origin validation; set `min-version 2` only when you need ASPA records synchronized, and ensure the validator supports v2. Set `port` to the validator's configured port (the RTR default is 323), and follow its authentication and network guidance. The image also provides `/var/lib/rpki-client` for validated ROA payload files when using `rpki-client` output instead.

When `rtr` is configured, Hextet LG's RTR Sessions page (`/rtr`) shows the session state reported by the same `bgplgd` API. A `descr` in the `rtr` block is shown by default; validator addresses are not published unless `features.rtr_session_addresses` is enabled. Choose descriptions that are safe to publish. See [SESSION_VISIBILITY.md](SESSION_VISIBILITY.md).

## Network access

Restrict TCP 9099 to trusted Hextet LG clients; the API is not an authenticated public service. Restrict TCP 179 to intended BGP peers only, using host and upstream firewalls or security groups. Do not expose either port broadly. Review the selected image's current [Docker Hub documentation](https://hub.docker.com/r/openbgpd/openbgpd) and [container entrypoint](https://github.com/openbgpd/openbgpd-container/blob/master/entrypoint.sh) when upgrading, since the API startup and proxying are image behavior.
