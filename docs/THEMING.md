# Theming

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

## Contrast and status colors

Operator colors are not silently changed. Contrast below WCAG AA (4.5:1 for text and 3:1 for the accent against page and panel surfaces) produces a startup warning. Primary text below 3:1 against the background or surface fails startup.

Route validity, RPKI OVS, ASPA AVS, result completeness, and warnings use fixed semantic status colors; they are not configurable and are kept distinct from the accent. Statuses always include text labels, so meaning does not depend on color alone.

## Visitor theme choice

`default: auto` follows the browser or system light/dark preference. `light` and `dark` select a fixed default. The theme button can switch the visitor's theme and saves the choice in that browser's `localStorage` under `theme`; it overrides the configured default for that browser only. There are no cookies or server-side preferences.

Without JavaScript, the configured default still applies, but the toggle is unavailable. With JavaScript, the saved theme is applied before first paint and persists through searches, pagination, history navigation, and reloads. Clearing the site's data or removing the `theme` localStorage key restores the configured default.

## Branding

The optional logo and favicon (`site.branding`, see [Site identity and branding](BRANDING.md)) do not use a separate palette. Logos are not recolored; the built-in favicon uses `theme.accent`. Choose a logo that works on both light and dark headers, or use transparency.
