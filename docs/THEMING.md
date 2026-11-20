# Site appearance

Hextet LG includes light and dark palettes. The optional `theme` section configures their colors and the visitor's default theme. Settings take effect after restart; the stylesheet is generated at startup. Omitted values use built-in defaults, so partial configurations are supported.

## Configuration

```yaml
theme:
  default: auto        # auto | light | dark
  accent: "#2563EB"    # shared by both palettes
  light:
    background: "#F7F8FA"
    surface: "#FFFFFF"
    surface_alt: "#F0F2F5"
    surface_hover: "#E8ECF1"
    border: "#D8DDE3"
    text: "#252B32"
    text_muted: "#626A76"
  dark:
    background: "#111316"
    surface: "#1B1E23"
    surface_alt: "#23272D"
    surface_hover: "#292D34"
    border: "#30353C"
    text: "#E8EBEF"
    text_muted: "#A0A7B2"
```

Every color is optional and must be a strict `#RRGGBB` value. Color names, shorthand or alpha hex, CSS functions, and other CSS text are rejected at startup, as are unknown keys. Invalid values are never inserted into the generated stylesheet.

| Setting | Used for |
|---|---|
| `background` | Page background |
| `surface` | Panels, top bar, and inputs |
| `surface_alt` | Table headers, route details, notices, and chips |
| `surface_hover` | Route-row hover and neutral pills |
| `border` | Panel, table, and separator lines |
| `text`, `text_muted` | Primary and secondary text |
| `accent` | Links, primary button, active navigation, and focus ring |

## Site identity

The optional `site` settings identify the network or organization operating this looking glass. Without them, Hextet LG uses a generic title and footer. These settings affect presentation only; they do not change backend queries, route results, visibility, or caching.

```yaml
site:
  name: "Example Networks LG"
  subtitle: "Network Routing Information"
  operator: "Example Networks"
  asn: 64500
  website: "https://example.net"
  contact: "noc@example.net"
  branding:
    directory: "/etc/hextet-lg/branding"
    logo: "logo.svg"
    favicon: "favicon.png"
    footer: "Powered by Hextet LG"
```

All fields are optional.

| Field | Default | Notes |
|---|---|---|
| `site.name` | `server.title`, then `BGP Looking Glass` | Up to 64 characters. Shown in the header and page titles. |
| `site.subtitle` | `Network Routing Information` | Up to 96 characters. Set to `""` to hide it. |
| `site.operator` | none | Up to 64 characters. Shown in the footer and linked to `website` when both are set. |
| `site.asn` | none | An ASN, written as a number or `AS` followed by a number. Shown as `AS<N>` in the footer; reserved values are rejected. |
| `site.website` | none | Absolute `http` or `https` URL. |
| `site.contact` | none | One email address, shown as a contact link. |
| `site.branding.directory` | none | Directory containing the configured logo and favicon. Required when either file is set. |
| `site.branding.logo` | none | Logo file name inside `directory`. |
| `site.branding.favicon` | built-in icon | Favicon file name inside `directory`. |
| `site.branding.footer` | `Powered by Hextet LG` | Up to 160 characters. Set to `""` to remove it. |

Text fields are plain text, not HTML. Invalid values fail configuration validation at startup. `site.name` takes precedence over the older `server.title` setting. If neither is configured, the name is `BGP Looking Glass`.

### Logo and favicon

Branding files are local and read at startup; restart Hextet LG after changing them. No files are downloaded and the application has no upload feature.

| File | Accepted formats | Maximum size |
|---|---|---|
| Logo | PNG, JPEG, GIF, SVG | 512 KiB |
| Favicon | PNG, JPEG, GIF, SVG, ICO | 128 KiB |

Raster images may be up to 4096 pixels on each side. SVGs must be self-contained and cannot include active or external content. Invalid or unsupported images fail startup. Use an image that looks clear in both light and dark themes; logos are not recolored.

The logo is shown beside the site name in the header. If no favicon is configured, Hextet LG uses a generic icon colored with the theme accent. The logo is served at `/branding/logo`; the favicon is served at `/branding/favicon` and `/favicon.ico`.

In Docker, mount the branding directory read-only and point `site.branding.directory` to the mount. Files must be readable by UID 10001:

```yaml
services:
  hextet-lg:
    volumes:
      - ./branding:/etc/hextet-lg/branding:ro
```

For a standalone binary, set `directory` to any readable path, such as `./branding`.

## Contrast and status colors

Operator colors are not silently changed. Contrast below WCAG AA (4.5:1 for text and 3:1 for the accent against page and panel surfaces) produces a startup warning. Primary text below 3:1 against the background or surface fails startup.

Route validity, RPKI OVS, ASPA AVS, result completeness, and warnings use fixed semantic status colors; they are not configurable and are kept distinct from the accent. Statuses always include text labels, so meaning does not depend on color alone.

## Visitor theme choice

`default: auto` follows the browser or system light/dark preference. `light` and `dark` select a fixed default. The theme button can switch the visitor's theme and saves the choice in that browser's `localStorage` under `theme`; it overrides the configured default for that browser only. There are no cookies or server-side preferences.

Without JavaScript, the configured default still applies, but the toggle is unavailable. With JavaScript, the saved theme is applied before first paint and persists through searches, pagination, history navigation, and reloads. Clearing the site's data or removing the `theme` localStorage key restores the configured default.
