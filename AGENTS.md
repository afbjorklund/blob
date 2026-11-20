# AGENTS.md

A content-addressable blob store (CIDv1, base32hex) with `dir`, `sql`
(SQLite), `http` and `s3` backends, plus OCI import/export and read-only
serve surfaces — an HTTP gateway + OCI-distribution read (`blob serve`) and
a FUSE read-only mount (`blob mount`). Single Go module. Everything here is a
**pointer** — the details live in the docs and the code, and where a fact is
stated, the code is the authority, not any doc.

| document                               | what it is                                                                                 |
|----------------------------------------|--------------------------------------------------------------------------------------------|
| [README.md](README.md)                 | usage: every command and flag, examples                                                    |
| [CONCEPTS.md](CONCEPTS.md)             | the ideas: content addressing, CAS, name index, gateway, OCI, comparisons                  |
| [DESIGN.md](DESIGN.md)                 | the *why* behind the design choices                                                        |
| [BACKEND.md](BACKEND.md)               | how to use each backend, incl. the on-wire contracts an external server must               |
| [IMPLEMENTATION.md](IMPLEMENTATION.md) | how the code is organized: packages, layers, storage internals, development gates, testing |

## Working Go

Go 1.26. The build / test / lint gates and their order live in the
`Makefile` (`make build test lint end2end`, or `make gate` for all of them
in order); the per-check rationale and the opt-in parts are documented in
[IMPLEMENTATION.md](IMPLEMENTATION.md) (Development). Run **all** of them —
`make gate` — before considering a change done, not just the Go bits:

- `make lint` runs three different linters, and the non-Go two are the ones
  that are easy to think do not apply to a Go edit: `markdownlint-cli2` for
  every `.md` file touched (README, AGENTS, DESIGN, …) and `shellcheck` for
  the `e2e/*.sh`/`e2e/*.bash` scripts. A `.go`-only change still has to pass
  them, because the repo's `.md` and shell files are part of the gate, not
  just the files you edited — run the gate, not only the parts you expect to
  be affected.
- For the storage concurrency guard, run `make race` (`go test ./... -race`)
  when you touch `storage/` — `make test`/`make gate` run it without `-race`,
  so the guard is opt-in and easy to miss.
- Inspect a dependency's API with `go doc <pkg> [symbol]` (e.g.
  `go doc github.com/multiformats/go-multicodec DagPb`) — it resolves the
  *selected* version from go.mod. Do not grep the module cache: the versioned
  directory names have `@` in them, globs mangle it, and
  `ls $(go env GOMODCACHE)/...` is the fragile path `go doc` exists to replace.
- A human editor formats on save, so it catches non-gofmt code immediately; an
  AI edit often mangles whitespace/indentation silently — so right after
  editing any `.go` file, re-format: `gofmt -w <file>` (or `goimports -w` for
  import grouping).

The `blob` binary, `.blob-store/`, and `*.sif`/`*.tar`/`*.tgz` are
gitignored.
