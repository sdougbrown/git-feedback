# Dependency licences

`git-feedback` builds with `go build` (no cgo). The only direct dependency is
`modernc.org/sqlite`; everything else is transitive. This table records the full
module graph as resolved by `go list -m all` and the licence of each module,
verified against the `LICENSE` files in the Go module cache.

## Direct dependency

| Module | Version | Licence |
| --- | --- | --- |
| `modernc.org/sqlite` | v1.50.1 | MIT |

## Indirect dependencies

| Module | Version | Licence |
| --- | --- | --- |
| `github.com/dustin/go-humanize` | v1.0.1 | MIT |
| `github.com/google/uuid` | v1.6.0 | MIT |
| `github.com/mattn/go-isatty` | v0.0.20 | MIT |
| `github.com/ncruces/go-strftime` | v1.0.0 | MIT |
| `github.com/remyoudompheng/bigfft` | v0.0.0-20230129092748-24d4a6f8daec | MIT |
| `golang.org/x/sys` | v0.42.0 | BSD-3-Clause |
| `modernc.org/libc` | v1.72.3 | MIT |
| `modernc.org/mathutil` | v1.7.1 | MIT |
| `modernc.org/memory` | v1.11.0 | MIT |

## Transitive build/test dependencies

These are part of the module graph (and `go.sum`) but are build or test
dependencies of the `modernc.org` toolchain rather than direct imports.

| Module | Version | Licence |
| --- | --- | --- |
| `github.com/google/pprof` | v0.0.0-20250317173921-a4b03ec1a45e | Apache-2.0 |
| `github.com/hashicorp/golang-lru/v2` | v2.0.7 | MPL-2.0 |
| `golang.org/x/mod` | v0.33.0 | BSD-3-Clause |
| `golang.org/x/sync` | v0.20.0 | BSD-3-Clause |
| `golang.org/x/tools` | v0.42.0 | BSD-3-Clause |
| `modernc.org/cc/v4` | v4.28.2 | MIT |
| `modernc.org/ccgo/v4` | v4.34.0 | MIT |
| `modernc.org/fileutil` | v1.4.0 | MIT |
| `modernc.org/gc/v2` | v2.6.5 | MIT |
| `modernc.org/gc/v3` | v3.1.2 | MIT |
| `modernc.org/goabi0` | v0.2.0 | MIT |
| `modernc.org/opt` | v0.2.0 | MIT |
| `modernc.org/sortutil` | v1.2.1 | MIT |
| `modernc.org/strutil` | v1.2.1 | MIT |
| `modernc.org/token` | v1.1.0 | MIT |

## Notes

- `modernc.org/libc` ships `LICENSE-3RD-PARTY.md` for code and assets acquired
  from third-party sources (Go, musl libc, go-netdb, NixOS/nixpkgs); the main
  project is MIT.
- `modernc.org/memory` ships `LICENSE-GO` and `LICENSE-MMAP-GO` (both MIT) and a
  `LICENSE-LOGO` pointing at a Wikimedia Commons image reference.
- All licences are permissive (MIT, BSD-3-Clause, Apache-2.0, MPL-2.0); none
  impose copyleft obligations on `git-feedback`.
