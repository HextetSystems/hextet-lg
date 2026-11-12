# BGP neighbor and RTR session pages

Hextet LG has two independent session views. Both read configured backends through the bgplgd HTTP API; there is no SSH, shell, control-socket access, or CLI text scraping.

| Page | Feature flag | Default | Backend API |
|---|---|---|---|
| Neighbors (`/neighbors`) | `features.bgp_neighbors` | `true` | `/neighbors` and `/summary` |
| RTR sessions (`/rtr`) | `features.rtr_sessions` | `true` | `/rtr` |

For example, disable the RTR page while keeping Neighbors available:

```yaml
features:
  bgp_neighbors: true
  rtr_sessions: false
```

A disabled page has no navigation link, returns `404`, and makes no backend request. The Neighbors page combines neighbor rows from `/neighbors` with per-backend session counts from `/summary`; `/summary` is not a public page. RTR sessions are OpenBGPD's connections to RPKI validators, not BGP neighbors.

## Results and compatibility

Each backend has its own section and status. Results are never merged, and a failure or unsupported endpoint from one backend does not hide other backends. Neighbor fields and counters appear only when supplied by the API; missing values are not inferred or shown as zero.

The RTR page distinguishes:

- **Sessions**: a valid response contains one or more sessions.
- **No sessions**: bgplgd returned a valid empty response. This does not imply an error.
- **Unsupported**: bgplgd returned `404` for `/rtr`.
- **Unavailable**: the request failed or returned an HTTP error.
- **Malformed**: the response was empty, invalid, oversized, or had an unexpected structure.

The page shows the state and optional protocol/session details reported by OpenBGPD. It does not show validator record counts or the last successful synchronization time; `/rtr` does not provide those values. The application does not probe or cache endpoint capability: an older backend may return `404` for each page request.

Compatibility has been reviewed against OpenBGPD 9.3 source. The `/rtr` endpoint and response shapes were also checked in 8.2, 8.5, 8.8, 9.0, and 9.2. The committed API fixtures are sanitized 9.3 responses; this coverage does not replace checking a live backend when upgrading. See the [OpenBGPD collector guide](OPENBGPD.md).

## Privacy

Validator addresses, ports, and local addresses are hidden by default. To publish them on `/rtr`, set `features.rtr_session_addresses: true`. When an RTR session has no description, the page uses a generic label. Descriptions from `bgpd.conf` are displayed as written, so choose labels safe for visitors.

Raw `/neighbors` and `/summary` JSON on the Neighbors page is controlled by `display.raw_json`, which defaults to `false`. When disabled, no Raw JSON control or payload is rendered. RTR raw JSON is never displayed. Backend text is escaped, and backend addresses and query values are not logged.

For configuring the OpenBGPD collector, including RTR version 2 for ASPA data, see [OPENBGPD.md](OPENBGPD.md). For user-facing page behavior, see the [README](../README.md#session-visibility).
