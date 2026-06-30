# Vendored third-party libraries

These minified bundles are embedded into the `anchored` binary via `//go:embed`
so the dashboard runs fully offline. They are unmodified upstream releases,
pinned to the versions below. Each retains its original license; see the linked
homepage for the full license text.

| File | Library | Version | License | Homepage |
| --- | --- | --- | --- | --- |
| `chart.umd.min.js` | Chart.js | 4.4.7 | MIT | https://chartjs.org |
| `vis-network.min.js` | vis-network | 9.1.9 | Apache-2.0 OR MIT | https://visjs.github.io/vis-network/ |
| `marked.min.js` | marked | 12.0.2 | MIT | https://marked.js.org |
| `purify.min.js` | DOMPurify | 3.1.6 | Apache-2.0 OR MPL-2.0 | https://github.com/cure53/DOMPurify |

## Copyright notices

- **Chart.js** — Copyright (c) 2013-2024 chart.js Contributors.
- **vis-network** — Copyright (c) 2014-present vis.js contributors.
- **marked** — Copyright (c) 2011-2024, Christopher Jeffrey.
- **DOMPurify** — Copyright (c) 2015-2024, Cure53 and other contributors.

## License texts

The full text of each license is available at:

- MIT: https://opensource.org/licenses/MIT
- Apache-2.0: https://www.apache.org/licenses/LICENSE-2.0
- MPL-2.0: https://www.mozilla.org/MPL/2.0/

> Note: DOMPurify (Apache-2.0 / MPL-2.0) and vis-network (Apache-2.0 / MIT) require
> attribution on redistribution; this file satisfies that requirement. If you
> modify a vendored bundle, update the version pin above and re-pin the hash.
