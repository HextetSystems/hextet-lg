# Changelog

## v1.1.0 — Unreleased

### Added

- Configurable route visibility exclusions, external community definitions and a community explorer based on operator data.
- RTR session visibility, optional ASN and next-hop PTR enrichment, configurable themes and generic operator branding.
- Configurable route columns, opt-in raw JSON, page timing and browser and OpenBGPD integration regressions.

### Changed

- Compact Route Explorer controls, responsive route cards, titles and server-rendered browser history restoration.
- Community definitions are supplied by operators rather than embedded as an operator-specific default. Searches use the Loc-RIB.

### Security and fixes

- Reject backend redirects and repeated control parameters; disable static directory listings and use fingerprinted static asset validators.
- Sanitize control characters in plain text and validate branding images more strictly. Raw JSON and RTR validator addresses are hidden by default.
- Restore rejected HTTP 400 searches correctly during browser Back and Forward navigation.
