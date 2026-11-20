# Community definitions

Community definition files add operator-supplied descriptions to route details and populate the Community Explorer. They are presentation data: they do not change route searches, backend results, or which routes are visible. Hextet LG does not download or synchronize definitions.

## Enable definitions

```yaml
community_definitions:
  enabled: true
  directory: /etc/hextet-lg/communities
```

Definitions are read at startup. Restart Hextet LG after changing the files. When this feature is disabled or no definitions are configured, community values display without descriptions.

## Files and format

Place `as<N>.txt` files in the configured directory, where `<N>` is the ASN that supplies the definitions. Use `well-known.txt` for communities that do not belong to a particular ASN. For example, `as64500.txt` contains definitions from AS64500. Files load in a consistent order: `well-known.txt`, then ascending ASN.

The file name records the definition's source; it does not restrict which community values it matches.

Write one definition per line as `community,description`. Blank lines and lines starting with `#` are ignored. The first comma separates the community from its description, so descriptions may contain commas.

```text
# Synthetic examples
64500:100,Learned from customer
64500:400-499,Regional policy
64500:nnn,Policy value $0
64500:1:100,Example large community
soo 64500:0,Example site of origin
```

Standard communities use `A:B`; large communities use `A:B:C`; extended communities use `label A[:B]`, as printed by OpenBGPD. Numeric components can be exact numbers, inclusive ranges such as `400-499`, or patterns using `x` for one digit and `nnn` for one or more digits. Patterns can use `$0`, `$1`, and so on in descriptions to show the digits matched by their wildcards.

For repeated ASN-specific definitions, `<ASN>` in an `as<N>.txt` file is replaced with that file's ASN. For example, `<ASN>:1912:0,Metro` in `as64500.txt` describes `64500:1912:0` as `Metro`.

Avoid duplicate definitions in your files. A repeated definition is an error and prevents startup. If different patterns overlap, an exact definition takes precedence; otherwise the more specific pattern is used.

The accepted text format is based on the [NLNOG looking glass community files](https://github.com/NLNOG/lg.ring.nlnog.net/tree/main/communities). Hextet LG supports standard, large, and extended communities, ranges, and digit patterns; it does not load the upstream JSON or URL-based formats.

## Where descriptions appear

Matching descriptions appear as tooltips in expanded route details and as entries in the Community Explorer. Unknown communities remain visible as their raw values. Plain-text output is unchanged, so it remains suitable for scripts. Descriptions are not searchable; route searches continue to use the community value.

The Explorer can filter definitions by text and source. Exact definitions link to a route search when that search type is enabled. Patterns are listed but do not create search links.

## Docker

Mount the directory read-only and make the files readable by UID 10001:

```yaml
services:
  hextet-lg:
    volumes:
      - ./communities:/etc/hextet-lg/communities:ro
```

Set `community_definitions.directory` to `/etc/hextet-lg/communities` in the runtime configuration. The directory must exist when definitions are enabled. Invalid files, including duplicate definitions, stop startup with the file and line reported.

[`examples/communities/`](../examples/communities) has sample definition files. Copy only the ones you want to use; they are not loaded by default.
