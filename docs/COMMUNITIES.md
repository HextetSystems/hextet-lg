# Community definitions

Hextet LG can annotate the BGP communities shown in route results, and list them in the Community Explorer, using **operator-supplied definition files** in the legacy text format of the [NLNOG looking glass](https://github.com/NLNOG/lg.ring.nlnog.net) (see its README, "community description files", and the files in its [`communities`](https://github.com/NLNOG/lg.ring.nlnog.net/tree/main/communities) directory).

Definitions are **presentation data only**. They never change what is searched, what OpenBGPD returns, which routes are visible, or what is cached. They are operator-supplied descriptions, not authoritative verification of anyone's routing policy. The application never downloads definitions; there is no remote URL, synchronisation or periodic refresh.

## Enabling

```yaml
community_definitions:
  enabled: false                          # default: nothing is read
  directory: /etc/hextet-lg/communities   # required when enabled
```

The section is optional. When it is omitted or `enabled: false`, no file is read, no definitions exist (there is no built-in set for any operator), unknown communities display their raw values, and the Community Explorer says that no definitions are configured.

Definitions are compiled **once at startup**. Changing a file requires a restart; there is no file watching or runtime reload. Nothing is read per request.

## Files

| File | Contents |
|---|---|
| `as<N>.txt` | Definitions, `N` a canonical ASN (`as64500.txt`; 1–4294967295, no leading zeros, lower case) |
| `well-known.txt` | Definitions that belong to no operator (for example `65535:666`) |
| `metadata.yaml` | Optional Community Explorer metadata (below) |

The ASN in a file name is organisational. A definition matches by the **community value it describes**, not by the file it is in: `65535:nnn` in `as10965.txt` matches `65535:7`. Different files can describe different ASNs, and a file may describe communities of any ASN. Other `.txt` files (for example the upstream `src_decix.txt` template) are ignored and reported in a startup warning; a file that starts with `as`, ends in `.txt` and is not a valid `asN.txt` name is a startup error, because it is almost certainly a typo. Non-`.txt` files and subdirectories are ignored. Symbolic links are followed (in Docker the target must also be inside the mount).

Loading order is fixed: `well-known.txt`, then ascending ASN (numerically), each file top to bottom. This order is the Explorer's order and the final precedence tie-break.

## Format

One definition per line: `community,description`. The line is split at the **first comma**, so descriptions may contain commas. Blank lines and lines starting with `#` are ignored. A UTF-8 byte order mark and CRLF line endings are accepted. Surrounding whitespace is trimmed.

```text
# synthetic examples, not Hextet policy
64500:100,Learned from customer
64500:200,Learned from peer
64500:300,Learned from transit
64500:400-499,Regional policy
64500:nnn,Policy value $0
64500:1:100,Example large community
soo 64500:0,Example site of origin
```

### Community kinds

| Kind | Notation | Notes |
|---|---|---|
| Standard | `A:B` | Both 0–65535. OpenBGPD's well-known names (`NO_EXPORT`, `NO_ADVERTISE`, `NO_EXPORT_SUBCONFED`, `NO_PEER`, `BLACKHOLE`, `GRACEFUL_SHUTDOWN`) are recognised in routes and mapped to `65535:…` before lookup. |
| Large | `A:B:C` | Each 0–4294967295. |
| Extended | `label A[:B]` | As printed by OpenBGPD (`rt 64500:1`, `soo 64500:1`, …); numeric components only, each 0–4294967295. Labels are matched case-insensitively. Hexadecimal, IPv4-address and non-numeric forms are not supported. |

The three kinds are never conflated: a standard definition never matches a large or extended community.

### Matching constructs

Every numeric component is one of:

| Construct | Example | Meaning |
|---|---|---|
| literal | `100` | exactly that number (no leading zeros) |
| range | `0-100` | any number from 0 to 100 inclusive |
| `x` | `x0`, `2xxx` | exactly one decimal digit per `x`. This is **not** a regular-expression wildcard |
| `nnn` | `nnn`, `1nnn` | one or more decimal digits (any length, **not** a fixed three) |

`x` and `nnn` may be mixed with each other and with literal digits (`65535:x:nnn`, `203038:752113nnn:nnn`). A component is matched as a whole decimal string without leading zeros, so `x0` matches `10`…`90` but not `0` or `100`. A definition with no range or wildcard is **exact**.

### Substitutions

`x` and `nnn` capture the digits they matched, numbered `$0`, `$1`, … from left to right across the whole community. Ranges do not capture.

```text
64500:0:nnn,Do not announce to AS$0
64500:x:nnn,Prepend $0 times to AS$1
12389:2xxx,Set local-pref = $0$1$2
```

Replacement text is only the matched digits; nothing in a description is evaluated. A reference to a capture the definition does not have (`$1` with one wildcard, or any `$N` on a range-only definition) is a startup error. In an **exact** definition `$` is ordinary text.

### Precedence and duplicates

Several definitions can match one community. The result is deterministic and never depends on map order:

1. an **exact** definition;
2. otherwise the pattern definition that can match the **fewest values**. This is estimated per component: a literal is 1, a range is `hi-lo+1`, each `x` multiplies by 10, and a pattern with `nnn` is the component maximum divided by 10 for each fixed digit. So `64500:405` beats `40x`, `400-409` and `4xx`, which beat `nnn`;
3. when still equal, the definition loaded first (see the loading order above).

A definition that is spelled identically (after canonicalisation: lower-case `x`/`nnn`/labels, `<ASN>` expanded) twice, in one file or across files, is a **startup error**. Overlapping but different definitions are legal and ordered as above.

### `<ASN>` templates

As in the NLNOG repository (where `as6695.txt` is a symlink to `src_decix.txt`), `<ASN>` in an `asN.txt` file is replaced by that file's ASN before parsing, so one template can serve many ASNs:

```text
<ASN>:1912:0,Metro: FRA
<ASN>:1913:nnn,Country code $0
```

## Compatibility with NLNOG

The reference is the NLNOG README and its `read_communities` / `get_community_descr_from_list` functions (`utils.py`). The accepted syntax is the README's. The deliberate differences:

| Upstream behaviour | Here |
|---|---|
| Pattern regexes are anchored only at the start (`65535:x0` also matches `65535:100`) | Whole-component match |
| Ranges are documented, but the code cannot evaluate them | Ranges work in any component |
| `x` is ignored when a definition also contains `nnn` (so `65535:x:nnn`, the README's own example, does not work) | Mixed `x`/`nnn` work |
| A community is looked up only in the file named after its first component | Matches by community value across all files |
| Lines it cannot understand are silently ignored | Startup error with file and line |
| Exact definitions: a later duplicate silently replaces an earlier one | Startup error |
| Overlap: exact, then ranges, then regular expressions in file order | Exact, then smallest value space, then file order |
| `<ASN>` expanded only in symbolic links | In every `asN.txt` |
| Also supports `draft-ietf-grow-yang-bgp-communities` JSON URLs | Not supported (no downloads, text format only) |

Not supported: `community_urls.yml`, JSON/YANG definitions and regular expressions. (`X` and `NNN` are accepted as `x` and `nnn`, as upstream lower-cases definitions.)

### Using upstream files

Running the NLNOG `communities/` directory (146 files, about 14,500 definitions) through this loader flags 30 lines, each reported with file and line so you can decide what to do:

- **24 duplicates**, mostly private-use ASNs such as `65001:nnn` or `0:nnn` that different operators define with different meanings. A looking glass cannot know which operator set the community, so you must choose one meaning (delete the other lines);
- **5 literals with leading zeros** (`8315:31:020`, `37271:0000`), which can never equal a decimal community value (upstream cannot match them either);
- **1 unsupported token** (`46733:2:ccc`).

Copy only the files for ASNs you care about, and remove or rename template files you do not want loaded.

## Limits

Fixed in code and generous for real data (the largest upstream file is 23 KB): 1 MiB per file, 4,096 definition files, 100,000 definitions, 1,024 bytes per line, 512 bytes per description, 16 characters per component, at most 3 `nnn` and 16 wildcards per definition. No regular expression is ever compiled; matching a community costs a map lookup plus a short scan of that first component's patterns.

## Startup policy

A configuration that cannot be loaded reliably **fails startup**; the application never runs with a silently incomplete set. Errors name the file and line and are collected (up to 20 are listed):

| Condition | Result |
|---|---|
| `enabled: true` without `directory` | configuration error |
| Directory missing, not a directory or unreadable | startup error |
| Unreadable file, dangling symlink, not a regular file, over the size limit | startup error |
| Invalid file name, malformed or unsupported definition, bad range or number, duplicate, bad substitution, invalid UTF-8, control characters, over a limit | startup error |
| Invalid `metadata.yaml` | startup error |
| Empty directory | valid: no definitions, a warning is logged |
| Other `.txt` files | ignored, listed in a warning |

Startup logs the number of files, definitions (exact and pattern; standard, large and extended) and metadata entries.

## Route tooltips

In the route table's expanded **Route details**, each standard, extended and large community whose value matches a definition carries a native `title` tooltip with the description (and a dotted underline). The visible value is unchanged, large communities remain search links, and unknown communities render plainly with no tooltip. Descriptions are HTML-escaped. There is no JavaScript. Enrichment is looked up at render time from the immutable definition set; nothing is copied into retained or cached results.

The `text` and `detail` views and `output=plain` are **not annotated**: their communities remain the raw OpenBGPD values so the plaintext format stays machine-friendly.

## Search is independent

Definitions do not influence searching. A community is searchable by its raw value whether or not a description exists, search URLs and canonical forms are unchanged, the M4.2 `features.*` flags still govern which search types are accepted, route visibility exclusions still apply, and descriptions cannot be searched (the Community Explorer's own text filter is local to that page).

## Community Explorer and optional metadata

The Explorer lists every loaded definition in loading order: its community or pattern, description, category and file, and (when supplied) status and evidence. Exact definitions link to a one-click search when that search type is enabled; wildcard and range definitions show as patterns without a link. Without metadata the category is the source (`AS64500`, `Well-known`). Search text and category filters work as before. One page shows at most 1,000 matching definitions and says so when it truncates; the filters reach all of them.

The NLNOG format carries only community and description. To keep the Explorer's *category*, *notes* and *implementation status/evidence* without extending that format, an operator may add `metadata.yaml` to the same directory:

```yaml
source: "Free text shown in the Explorer: where the evidence comes from"
communities:
  "64500:100":            # the canonical definition text, as listed in the Explorer
    category: Route classification
    status: implemented    # implemented | documented | unresolved
    evidence: community list
    evidence_note: "optional detail"
    notes: "optional note"
```

Keys must be the canonical text of an existing definition (a key with no definition is a startup error); every field is optional; unknown fields are rejected. `implemented` means the community is present in the operator's own configuration; it never claims the routing behaviour was independently verified. Metadata is purely presentational.

## Docker

Mount the directory read-only; the container needs no write access, and the mount does not change the UID 10001, read-only root filesystem or any other restriction:

```yaml
services:
  hextet-lg:
    volumes:
      - ./configs/config.docker.yaml:/etc/hextet-lg/config.yaml:ro
      - /srv/docker/data/hextet-lg/communities:/etc/hextet-lg/communities:ro
```

with `community_definitions.enabled: true` and `directory: /etc/hextet-lg/communities` in `configs/config.docker.yaml`. The files must be readable by UID 10001 (mode 0644, directory 0755). If the directory is configured but absent, the container exits at startup (see the policy above).

## Example set

[`examples/communities/`](../examples/communities) holds the AS395089 definitions migrated from the former built-in reference: `as395089.txt` (30 definitions) and `metadata.yaml` (category, status and evidence from the operator's RouterOS configuration). Copy them to your communities directory if you operate AS395089, or use them as a model. They are plain operator data, not defaults: nothing is loaded unless you configure the directory.
