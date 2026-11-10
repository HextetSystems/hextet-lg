# Theming

Hextet LG has two complete palettes, light and dark, built from one set of semantic CSS custom properties. Every color is configurable through the optional `theme` section of the YAML configuration; nothing else about the layout changes. Changes require a restart: the stylesheet is generated once at startup.

## Configuration

Every key is optional. Omitted values inherit the built-in defaults, so a partial section works and configurations written before `theme` existed are unchanged.

```yaml
theme:
  default: auto        # auto | light | dark
  accent: "#2563EB"    # one accent, shared by both palettes
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

To change only the accent:

```yaml
theme:
  accent: "#0D9488"
```

| Key | Used for |
|---|---|
| `background` | page background |
| `surface` | panels, top bar, inputs |
| `surface_alt` | table headers, route details, notices, chips |
| `surface_hover` | route-row hover, neutral pills |
| `border` | panel, table and separator lines |
| `text`, `text_muted` | primary and secondary text |
| `accent` | links, primary button, active navigation, focus ring |

The built-in light `text_muted` is `#626A76` (slightly darker than a plain mid grey) so muted text reaches 4.5:1 even on hovered table rows.

### Validation

Colors must be strict `#RRGGBB` (either hex case; normalized to upper case). Color names, `#RGB`, `#RRGGBBAA`, `rgb()`/`hsl()`, `url()`, `var()`, `expression()`, semicolons, braces and any other text are rejected at startup with the offending key. Unknown keys are rejected too. The stylesheet is assembled only from validated values, so configuration cannot inject CSS. `default` must be `auto`, `light` or `dark`.

### Contrast policy

Operator colors are never replaced silently.

- Below WCAG AA (4.5:1 for text, 3:1 for the accent against the page and panels) the application starts and logs a `WARN` naming the palette, the pair and the ratio.
- Primary `text` below 3:1 on `background` or `surface` is unreadable and fails startup.

The built-in themes have no warnings; a test enforces it.

## Light and dark selection

- `auto` (default) follows the browser or system (`prefers-color-scheme`).
- `light` or `dark` start in that theme regardless of the system.
- The theme button (◐) switches theme and stores the choice in the browser's `localStorage` under the key `theme` (`light` or `dark`). A stored choice overrides `theme.default` in that browser only; there are no cookies and no server-side state.
- To reset, remove the key (`localStorage.removeItem('theme')` in the browser console) or clear the site's data. The configured default applies again.
- A tiny synchronous script in `<head>` (`/static/theme-init.js`, after the stylesheets) applies the stored choice before first paint, so there is no flash of the wrong theme. HTMX only swaps the results region and never touches `<html>`, so the choice survives searches, pagination, browser history and reloads.
- Without JavaScript the stylesheet alone applies the configured default (`auto` still follows the system); the button does nothing.

## How it is built

`internal/theme` compiles the section once into `/static/theme.css`: a plain `:root` rule for the default palette, plus the other palette under `:root[data-theme="…"]` (and, in `auto`, inside `@media (prefers-color-scheme: dark)` unless light was chosen). `static/app.css` contains layout only and no color literals; every color is a `var(--…)` token. A test fails if a color literal, gradient or shadow reappears or a token used by `app.css` is undefined in any palette.

The stylesheet is served from memory with a strong content-hash `ETag` and `Cache-Control: no-cache`; page templates link it, `app.css` and the scripts with a `?v=` content fingerprint, so a restart with new colors can never be paired with a cached old file. Security headers (including `style-src 'self'`) are unchanged: there are no inline styles or scripts and nothing is loaded from a CDN.

### Tokens

Configurable: `--color-background`, `--color-surface`, `--color-surface-alt`, `--color-surface-hover`, `--color-border`, `--color-text`, `--color-text-muted`, `--color-accent`.

Derived deterministically (integer sRGB mixing, so output is byte-identical across platforms):

| Token | Derivation |
|---|---|
| `--color-accent-subtle` | accent mixed 12% into `surface` (count badge background) |
| `--color-accent-text` | accent moved toward the palette's pole (black on light palettes, white on dark ones) in 2% steps until it reaches 4.5:1 on all four surfaces and on `accent-subtle`; unchanged if already sufficient. Links, active navigation, prefix links |
| `--color-focus` | equal to `--color-accent-text` (focus ring, 2 px) |
| `--color-accent-foreground` | white or black, whichever is more readable on the accent (button text) |
| `--color-accent-hover` | accent moved 14% away from the foreground color; hover never lowers text contrast |
| `--color-border-control` | `border` moved toward `text` in 5% steps until 3:1 on `background`, `surface` and `surface_alt` (inputs, selects, secondary buttons); decorative borders stay light |

The accent is used for ordinary UI emphasis only, and sparingly: links, the one primary button, active navigation, focus and the count badge. Secondary buttons are neutral.

### Semantic status colors

Route validity (RPKI `ovs`, ASPA `avs`, route `valid`), result completeness and notices carry operational meaning and are **not** the accent and **not** configurable. Each status has fixed hues for light and dark; `--status-{good,bad,warn}-bg` is the hue mixed 16% into the surface and `--status-{good,bad,warn}-fg` is the hue moved toward the pole until it reaches 4.5:1 on that background and on every surface of the palette. Unknown, not-found and other neutral states use `surface_hover` and `text_muted`. Every status always has a text label (`valid`, `invalid`, `unknown`, `COMPLETE`, `PARTIAL`, `FAILED`, "Query failed", "Partial results"), so meaning never depends on color alone; notices also get a heavier left border.

### Remaining non-configurable colors

Only the status hues in `internal/theme/theme.go` (good, bad, warn × light, dark), the black and white button-text candidates and the black/white poles used by the derivations. Browser-native control internals (checkbox glyph, select popups) follow `color-scheme` and `accent-color`.

## Manual verification checklist

No browser framework is part of the test suite. After changing the theme:

1. `go test ./...` and `node --test internal/web/testdata/*.test.js`.
2. Start an isolated instance with a mock backend and open `/`, a prefix search with several routes (OVS valid/invalid/unknown, AVS), `/neighbors`, `/summary`, `/communities`.
3. In each theme confirm: readable table text and headers, subtle row hover, visible keyboard focus on links, inputs, buttons and `<summary>`, readable placeholders, a distinct disabled state (Advanced filters `neighbor`/`group` exclusivity), and status pills still readable.
4. Toggle the theme, search via HTMX, page, go back, reload: the chosen theme persists with no flash.
5. With `localStorage` cleared, check the configured default (and, for `auto`, the system preference).
6. Reload after editing the YAML and restarting: the new colors appear without a hard refresh.

Headless Chromium screenshots of both themes were used for the M4.5.1 review; real-browser interaction tests (toggle clicks, history) are covered only by the Node unit tests of `theme-init.js` and the toggle in `app.js` and by the Go tests of the served HTML.

## Branding

The optional logo and favicon (`site.branding`, see [BRANDING.md](BRANDING.md)) use no separate palette. Logos are never recolored; the built-in favicon is drawn in `theme.accent`. Choose a logo that reads on both the light and the dark header, or use transparency.
