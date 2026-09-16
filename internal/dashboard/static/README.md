# internal/dashboard/static

Vendored, embedded assets (SPEC-10 §2.7). No build chain, no bundler, no CDN, no
network fetch at runtime: everything here is compiled into the binary with
`//go:embed static/*`.

| File | Bytes | sha256 | Provenance |
|---|---|---|---|
| `htmx.min.js` | 50917 | `e209dda5c8235479f3166defc7750e1dbcd5a5c1808b7792fc2e6733768fb447` | upstream `htmx.org@2.0.4` `dist/htmx.min.js`, fetched once at build-vendor time |

SPEC-10 §2.7 records the vendored asset as 47,755 B (the fleet's 1.9.x-era
measurement). v0.1 pins htmx 2.0.4, whose minified bundle is 50,917 B: the
difference is 3,162 B and is the only deviation from that line. The pin — not the
byte count — is what matters, and the hash above is the contract.

`app.css` and `app.js` are authored in this repository (dark, mobile-first, no web
fonts, no inline script or style anywhere, so `script-src 'self'` suffices).
