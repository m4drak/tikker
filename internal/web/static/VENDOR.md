# Vendored front-end assets

Files in this directory are embedded in the binary (`go:embed`) and served from
`/static/`; the panel never loads anything from a CDN. Licence texts are in `LICENSES/`.

| File | Project | Version | Licence | Source |
|------|---------|---------|---------|--------|
| `htmx.min.js` | htmx (htmx.org) | 2.0.11 (published 2026-09-22) | 0BSD | https://cdn.jsdelivr.net/npm/htmx.org@2.0.11/dist/htmx.min.js |
| `pico.min.css` | Pico CSS (@picocss/pico) | 2.1.1 (published 2025-03-15) | MIT | https://cdn.jsdelivr.net/npm/@picocss/pico@2.1.1/css/pico.min.css |

SHA-256 (the same as the file inside the npm tarball of that version on
`registry.npmjs.org` and as the jsDelivr copy):

```
d6fdc75f204e6bdefa99b69bf1e6d4ac69b8a364f77929f45c13476b4000f717  htmx.min.js
fbc9a63fc9fc9f72d12fd7fc9806e11fa9f77ae4f9cad146b27003a1119ba3db  pico.min.css
```

The unit test `TestVendoredAssetsPinned` pins these sums.

To update: download the new files, compare them with the npm tarball, replace the files, the
versions and the sums here and in the test.

htmx is configured by `<meta name="htmx-config">` in the layout: `allowEval=false`,
`allowScriptTags=false`, `includeIndicatorStyles=false` (no inline `<style>`), `historyEnabled=false`.
The CSRF header for htmx requests is added by `app.js` (own code, not vendored).
