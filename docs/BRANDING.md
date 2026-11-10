# Site identity and branding

Hextet LG is software; a looking glass is a deployment. This guide separates the two:

| Identity | What it is | Where it lives |
|---|---|---|
| Software | **Hextet LG**: name, repository, Go module, binary, container image, `hextet_lg_*` metrics, `X-Hextet-LG-*` headers, the default "Powered by Hextet LG" footer note | Fixed. Never configurable away from the code. |
| Deployment | The network that runs the looking glass: its name, operator, AS number, website, contact, logo, favicon | The optional `site` section of the YAML configuration. |

An installation with no `site` section is operator-neutral: it names no operator and no AS number, and needs no files. Nothing in the `site` section influences a backend query, a filter, a route result, backend selection, visibility, caching or enrichment. The operator AS number is display metadata only and is unrelated to backend, neighbor, origin, search or session ASNs.

## Configuration

```yaml
site:
  name: "Example Networks LG"          # header and browser titles
  subtitle: "Network Routing Information"
  operator: "Example Networks"         # footer
  asn: 64500                           # 64500 or "AS64500"; footer
  website: "https://example.net"       # absolute http(s) URL
  contact: "noc@example.net"           # plain email address
  branding:
    directory: "/etc/hextet-lg/branding"
    logo: "logo.svg"
    favicon: "favicon.png"
    footer: "Powered by Hextet LG"     # plain text
```

Every field is optional.

| Field | Default | Notes |
|---|---|---|
| `site.name` | `server.title` if set, else `BGP Looking Glass` | Up to 64 characters. Header text and the title suffix of every page (`<subject> · <name>`). |
| `site.subtitle` | `Network Routing Information` | Up to 96 characters. `""` removes it. Visually reduced (still in the accessible name) on phones. |
| `site.operator` | none | Up to 64 characters. Footer; linked to `website` when both are set. |
| `site.asn` | unset | `64500` or `"AS64500"`. Shown as `AS64500` in the footer. Reserved values (23456, 65535, 4294967295) are rejected. |
| `site.website` | none | Absolute `http`/`https` URL, ASCII (use punycode hosts), no credentials. `javascript:`, `data:`, relative and other schemes fail startup. Rendered with `rel="noopener noreferrer"`. Without `operator`, the host name is shown as the link text. |
| `site.contact` | none | One plain email address (no display name, quoting or lists). Rendered as a `mailto:` link. |
| `site.branding.directory` | none | Where `logo` and `favicon` are read from. Required when either is set. |
| `site.branding.logo` | none | File name inside `directory`. |
| `site.branding.favicon` | built-in icon | File name inside `directory`. |
| `site.branding.footer` | `Powered by Hextet LG` | Up to 160 characters. `""` removes it. |

All text is plain text: it is validated (valid UTF-8, no control or non-printing characters, bounded length), HTML-escaped on output, and never interpreted as HTML, Markdown or template syntax. Invalid values fail startup with the field name.

The footer shows only what is configured: `Example Networks · AS64500 · noc@example.net · Powered by Hextet LG · Generated in 3.1 ms`. Absent fields leave no empty labels or separators, and the timing separator appears only when something precedes it. The existing `Powered by OpenBGPD · Read-only queries` attribution stays on the left.

## `server.title` compatibility

`server.title` was the site name before this section existed. It still works and is no longer required:

1. `site.name`, when set.
2. `server.title`, when set (control characters are stripped and it is truncated to 64 characters rather than rejected, so old configurations always load).
3. `BGP Looking Glass`.

If both are set and differ, `site.name` wins and a warning is logged at startup. There is one title-generation path (`internal/web/title.go`); it uses the resolved name.

## Logo and favicon

Both are optional, local, and read **once at startup**; replacing a file requires a restart (the asset URL carries a content fingerprint, so browsers never keep an older copy). Nothing is fetched from an external host, and there is no upload endpoint.

| | Types | Size limit |
|---|---|---|
| Logo | `.png`, `.jpg`/`.jpeg`, `.gif`, `.svg` | 512 KiB; raster images up to 4096 px per side |
| Favicon | the above and `.ico` | 128 KiB |

The logo appears in the header in place of the letter mark. It is 30 px high (110 px wide at most on phones, 180 px otherwise), never distorted (`object-fit: contain`), and the site name stays as text beside it, so the logo is marked decorative (`alt=""`). Width and height attributes come from the file, so the header does not shift while loading. Logos are not recolored: use a file that reads on both the light and dark header, or one with a transparent background. The header and footer colors come from the [theme](THEMING.md); there is no separate branding palette.

Without a favicon, a built-in generic icon is used, drawn in the configured `theme.accent`.

### Serving

| URL | Content |
|---|---|
| `/branding/logo` | the logo (404 when none is configured) |
| `/branding/favicon` | the favicon or the built-in icon |
| `/favicon.ico` | the same as `/branding/favicon` (for clients that ask for it by default) |

Pages link `/branding/logo?v=<fingerprint>`; a request with the current fingerprint is `Cache-Control: public, max-age=31536000, immutable`, any other URL (including `/favicon.ico`) is `no-cache` with a strong `ETag`. There are no path parameters, so no request can name a different file. The icon link is in every full page's `<head>` and is not part of HTMX fragments, so it works without JavaScript and is unaffected by HTMX navigation.

### Security decisions

- `logo` and `favicon` are **plain file names**: separators, `..`, absolute paths and NUL bytes fail startup.
- The directory is opened as a Go `os.Root`, so neither `..` nor a symbolic link can reach outside it; the file must be a regular file.
- Content is checked against the file extension (PNG/JPEG/GIF headers must decode, ICO must have a valid header, dimensions are bounded); a mismatch fails startup.
- SVG is validated, never inlined into a page, and only referenced from `<img>` (where browsers disable scripts and external loads). Validation requires a well-formed `<svg>` in the SVG namespace and rejects `<script>`, `<foreignObject>`, animation elements, `on*` attributes, entity declarations, `@import`, and any `href` or `url()` that is not a same-document `#fragment`. SVG is also served with `Content-Security-Policy: default-src 'none'; style-src 'unsafe-inline'; sandbox`, so opening the URL directly is inert.
- The page CSP is unchanged (`img-src 'self' data:`): assets are same-origin.
- Raster files are validated by header only; a truncated image that passes is shown broken by the browser, not served as something else.

## Docker

Mount the directory read-only and point `site.branding.directory` at the mount. Files must be readable by UID 10001. The image needs no rebuild to change the identity.

Add the mount in a local, Git-ignored `compose.override.yaml` next to `compose.yaml` (or add the line to your own Compose file):

```yaml
services:
  hextet-lg:
    volumes:
      - ./branding:/etc/hextet-lg/branding:ro
```

and in `configs/config.docker.yaml`:

```yaml
site:
  branding:
    directory: /etc/hextet-lg/branding
    logo: logo.svg
    favicon: favicon.png
```

This works with the read-only root filesystem, a non-root user and dropped capabilities. Behind a reverse proxy nothing changes: the asset URLs are relative to the site root and are cacheable by the proxy.

## Standalone

Use any readable directory, absolute or relative to the working directory:

```yaml
site:
  branding:
    directory: ./branding
    logo: logo.png
```

## Migrating from the Hextet defaults

Earlier versions hard-coded the header ("HEXTET / NETWORK OPERATIONS") and footer ("Hextet Systems · AS395089"). They are gone. A deployment that wants an identity must now say so, for example `site.operator`, `site.asn` and `site.name`; a configuration with only `server.title` keeps its title, and gets the generic subtitle and footer.
