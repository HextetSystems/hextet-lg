# BGP neighbor and RPKI RTR session visibility

This guide covers the two session views of Hextet LG, which are independent of each other:

| View | URL | Switch | Default | Backend endpoint(s) |
|---|---|---|---|---|
| BGP neighbors and summary | `/neighbors`, `/summary` | `features.bgp_neighbors` | on | bgplgd `/neighbors`, `/summary` |
| RPKI-to-Router (RTR) sessions | `/rtr` | `features.rtr_sessions` | on | bgplgd `/rtr` |

RTR sessions are the connections between OpenBGPD and an RPKI validator (Routinator, rpki-client, StayRTR, and similar). They are not BGP neighbors and are never mixed into the neighbor tables.

Everything here is read from OpenBGPD's own `bgplgd` HTTP API through the backend URLs in your YAML. There is no SSH, shell, control-socket access or scraping of CLI text.

## Feature flags

```yaml
features:
  bgp_neighbors: true          # /neighbors and /summary
  rtr_sessions: true           # /rtr
  rtr_session_addresses: false # publish validator addresses on /rtr
```

- Omitted flags take the defaults shown. Existing configurations keep working unchanged.
- `bgp_neighbors` and `rtr_sessions` do not control each other. Disabling one never disables the other.
- A disabled view has no navigation link and is not registered: its URL answers `404` and the backend is not contacted.
- `rtr_sessions: true` never causes a startup failure. A backend whose bgplgd lacks `/rtr` is reported as *unsupported* on the page.
- Disabling `bgp_neighbors` does not affect the neighbor links on route results. Those go to the route search (`/?neighbor=…&backend=…`), not to `/neighbors`.
- These two flags switch whole views. The community flags (`communities`, `large_communities`, `extended_communities`) still only control search availability.

## What OpenBGPD actually exposes

Verified against the OpenBGPD 9.3 release source (the version of the image in [docs/OPENBGPD.md](OPENBGPD.md)): `bgplgd/bgplgd.c` (endpoint table), `bgpctl/output_json.c` (JSON), `bgpd/control.c` (restricted socket) and `bgpd/rtr_proto.c`. The `/rtr` endpoint and its JSON were also checked in the 8.2, 8.5, 8.8, 9.0 and 9.2 releases. This is source review, not a test against a live backend.

bgplgd maps each endpoint to a fixed `bgpctl -j` command:

| Endpoint | bgpctl command | Used here |
|---|---|---|
| `/summary` | `show` | yes (basic session list) |
| `/neighbors` | `show neighbor` | yes |
| `/rtr` | `show rtr` | yes |
| `/rib`, `/rib/in`, `/rib/out` | `show rib …` | route search (`/rib`) |
| `/metrics` | `show metrics` | no |
| `/sets`, `/interfaces`, `/memory`, `/nexthops` | | no |

The restricted control socket that bgplgd uses permits `show rtr`, and the OpenBGPD container's HAProxy front end forwards every path, so `/rtr` is reachable on the same port as `/neighbors`.

### Capability matrix

| Information | Available? | Actual source |
|---|---|---|
| BGP neighbor address | Yes | `/summary`, `/neighbors`: `remote_addr` |
| BGP session state | Yes | `/summary`, `/neighbors`: `state` (None, Idle, Connect, Active, OpenSent, OpenConfirm, Established) |
| Neighbor ASN | Yes | `remote_as` |
| Session uptime | Yes | `last_updown` (text) and `last_updown_sec`: time since the last state change, which is the uptime while Established |
| Prefix counts | Yes, `/neighbors` only | `stats.prefixes.received`, `stats.prefixes.sent`. There is no separate "accepted" count |
| Message counters | Yes, `/neighbors` only | `stats.message.{sent,received}` (open, notifications, updates, keepalives, route_refresh, total) and `stats.update.{sent,received}` (updates, withdraws, eor) |
| Address family | Yes, when past OpenSent | `session.capabilities.multiprotocol` (the negotiated families) |
| Last error | Yes, `/neighbors` only | `last_error_sent`, `last_error_received`; peer's shutdown message in `last_shutdown_reason` |
| RTR validator address | Yes | `/rtr`: `remote_addr`, `remote_port`, `local_addr`, optional `descr` |
| RTR connection state | Yes | `/rtr`: `state` (closed, error, established, exchange, negotiation); absent in 8.2 |
| RTR protocol version | Yes | `/rtr`: `version` (8.5+), `minimal_version` (8.8+) |
| RTR serial / session ID | Yes, once data was exchanged | `/rtr`: `serial`, `session_id` (both omitted until the session has data) |
| RTR refresh / retry timers | Yes | `/rtr`: `refresh`, `retry`, `expire` (intervals) and `timers[]` (`RTR RefreshTimer` etc., seconds until due) |
| RTR received records | **No** | Not in `/rtr`. `/sets` has only global ROA and ASPA set totals that are not tied to a session; it is deliberately not used as a substitute |
| RTR last successful synchronization | **No** | Not reported by `/rtr` |
| RTR / validator metrics | **No** | `/metrics` has BGP peer and RDE metrics only, nothing for RTR |

`/summary` (bgpctl `show`) returns only the basic fields in JSON: no counters, no errors, no session block. The summary page therefore has no counter columns. Text output of the same command does contain counters, but the JSON API does not.

## BGP neighbor pages

The compact table keeps one row per neighbor: address (linked to a route search on that backend), description, ASN, state, last change, and, only when the backend reports stats, prefixes and message totals received / sent. State text is exactly what OpenBGPD reports, shown with a restrained color (Established green, Connect/Active/OpenSent/OpenConfirm amber, Idle/None neutral). No health score is computed.

"Neighbor details" expands to a grid of only the values the backend actually returned: group, BGP ID, negotiated address families, hold time and keepalive, last read / write, message and update counters in each direction, last errors and the peer's shutdown message. Missing values are left out or shown as a dash, never as zero. The raw neighbor JSON remains available beneath it, as before.

Neighbor route links use the backend-supplied address and the ID of the backend that returned it. The same address on two backends gives two separate rows in two separate panels with different `backend=` values. Address family is never derived from the neighbor's transport address.

## RTR session page

For each backend the page shows one of five distinct outcomes. They are never conflated:

| Outcome | Meaning | How it is detected |
|---|---|---|
| **N sessions** | Supported, sessions reported | valid document with a non-empty `rtrs` array |
| **no sessions** | Supported, zero sessions | valid document with no `rtrs` (bgpctl prints an empty object when no `rtr` directive exists) |
| **unsupported** | bgplgd has no `/rtr` | HTTP 404 |
| **unavailable** | Could not ask | connection error, timeout, cancellation, any other HTTP error |
| **malformed response** | Answered, but unusable | empty body (how bgplgd reports a rejected command), invalid JSON, wrong structure, `status: FAILED`, invalid address/port, negative numbers, more than 256 sessions, or over 1 MiB |

"No sessions" is shown only for the second row. For the others the page says that nothing is claimed about the backend.

Per session the table shows: validator (see Privacy), state, protocol version (and minimum), session ID, serial, refresh / retry / expire, and a Last error column that appears only if some session on that backend has an error. Session details add the local address (only if addresses are enabled) and the running timers. Every field is optional; absent fields show a dash.

State coloring is deliberately conservative. `established` is green only when the backend also reported a session ID and serial; established without them is amber with an explanation. `exchange` and `negotiation` are amber, `closed` and `error` red, and a missing or unrecognized state is neutral and labelled. The text label is always shown.

There is no capability probe and no capability cache: the `/rtr` request itself is the probe, the response is a few hundred bytes, and the normal rate limiting and work gate bound how often it is made. An older backend therefore costs one cheap 404 per page view.

## Multi-backend behavior

- Each backend is queried independently and shown in its own panel with its own status, duration and (for RTR) capability badge.
- Sessions of different backends are never merged or de-duplicated.
- One backend failing, being unsupported or returning garbage never hides the others.
- Backend selection is by stable ID (`backend=`), never by address. The selection is kept in the form and in the URL.

## Privacy and security

- **Validator addresses are not published by default.** `/rtr` identifies a validator by its `descr` from `bgpd.conf` (`rtr 192.0.2.100 { descr "validator-a" }`) or, if there is none, by `Validator N`. The remote address, port and local address appear only with `features.rtr_session_addresses: true`. Choose descriptions that are safe to publish: the description is shown as written. Validators are usually internal infrastructure; enable addresses only if you are comfortable publishing them.
- The RTR page never shows raw backend JSON.
- `show rtr` contains no authentication material (TCP MD5 secrets are not part of it), and the BGP details grid does not decode local addresses, ports or authentication methods.
- **Raw JSON is opt-in.** The neighbor pages can offer the raw `/neighbors` and `/summary` JSON ("Raw JSON" per neighbor and "Raw backend response"), which includes fields this application does not otherwise display (for example the local session address and configuration flags). Since M4.8.2 it is rendered only when `display.raw_json: true` (default `false`); when off, no control is rendered and the payload is not embedded in the page. Enable it only for trusted deployments. `features.bgp_neighbors: false` still removes the pages entirely.
- All backend strings (descriptions, error text, shutdown messages, families) are escaped by `html/template`. RTR text is also stripped of control characters and truncated to 256 bytes before it is kept.
- Backend URLs come only from the YAML. The only path requested for RTR is the fixed `/rtr`, without a query string.
- Backend identities, query strings and addresses are never logged. Logs carry the configured backend ID, a fixed operation name (`rtr`, `neighbors`, `summary`) and, for RTR, a fixed `rtr_state` value.

## Metrics and logs

`rtr` is a fixed value of the `operation` label on `hextet_lg_backend_*` and a fixed `route` label on `hextet_lg_http_*`. An unsupported backend is recorded as a failed request **without** a failure reason: it is a capability, not a fault, and it does not mark the backend unhealthy. Unavailable and malformed answers carry the normal bounded reasons (`timeout`, `canceled`, `backend_error`, `connection_closed`, `invalid_json`, `response_limit`). Requests to `/rtr` use the same live rate-limit bucket as `/neighbors` and `/summary`.

## Operational examples

Check what a backend offers, from a host that can reach it:

```sh
curl -fsS http://127.0.0.1:9099/rtr     # 200 with {} when no rtr directive is configured; 404 on a bgplgd without /rtr
curl -fsS http://127.0.0.1:9099/neighbors
```

Show only BGP sessions:

```yaml
features:
  bgp_neighbors: true
  rtr_sessions: false
```

Show RTR sessions with a safe label and no addresses (the default), given this `bgpd.conf`:

```conf
rtr 192.0.2.100 {
	descr "validator-a"
}
```

## Limitations

- The data comes from the backend at request time. Pages are live queries, not snapshots, and are not cached.
- Field availability depends on the OpenBGPD release (see the matrix). Optional fields are never inferred.
- No RTR record counts, last-synchronization time, or validator health beyond the state OpenBGPD reports.
- Verified from source and with mock servers, not against a live validator. Exercise it on your own backends before relying on it.
