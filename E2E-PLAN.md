# E2E implementation plan

End-to-end (CLI) testing plan for the `blob` binary. A proposal only — none
of these files exist yet. When implemented, the *usage* doc will live in
`E2E.md` (written then); this file is the before-the-fact plan.

## Why

The library tests (`oci`, `identity`, `daemon`, `mount`, `storage`) cover
the Go API surface, and the smoke tests (`*_smoke_test.go`) cover remote
backends over real sockets. None covers the *user-facing CLI*: `blob add`
→ `blob get` against a real directory, with the real flags, stdout, and
exit codes. A CLI-level mistake — a wrong flag-to-argument mapping, a
missing `--force` guard, the wrong exit code on `oci import` of a tampered
layout — is invisible to every test currently in the repo. This plan adds
that layer.

## Placement and scope

- The suite lives in **`e2e/`** (top-level, not `tests/e2e/`), one file
  per scenario.
- It is **opt-in**: `go test ./e2e/` with `BLOB_E2E=1`, exactly like the
  `*_smoke_test.go` suites. Unset → the single `TestE2E` skips, so
  `go test ./...` stays green with nothing extra installed.
- The **`main` package gets no `main_*_test.go`**. `main` is the CLI; it is
  exercised here as a black box (exec the binary), never by calling into it.
  Library functions keep their own package tests.
- No BATS, no Makefile. The build is still `go build`, the test is still
  `go test`; this suite is just a shell script run under that.

## Interpreters are not the implementation

The shell suite may invoke external interpreters — `python3`, `awk`,
`curl`, `sha256sum`, `cmp` — the same way the daemon smoke test shells out
to `curl`. These are *tools* we call; they are not part of the Go
implementation and add no dependency to `go.mod` (python/awk need not even
be installed for the suite to build or for `go test` to skip). This is
what makes a `python3` heredoc for assembling a tampered OCI layout
acceptable: it is fed to the interpreter at runtime, like an awk program,
not vendored Go.

## Layout

```
e2e/
  e2e_test.go       # Go wrapper (opt-in BLOB_E2E=1). Builds the binary to a
                    # temp dir, sets BLOB_BIN / BLOB_TMP, execs run.sh, maps
                    # the shell exit code to pass / skip / fail.
  run.sh            # Orchestrator: sources lib.sh, runs each tNNN_*.bash in
                    # name order, reports PASS/SKIP/FAIL, sets the exit code.
  lib.sh            # Shared helpers: expect_ok, expect_fail, assert_eq,
                    # assert_file_eq, skip, and the `blob` wrapper.
  t001_add_get.bash
  t002_oci_import_bare.bash
  t003_oci_import_tampered.bash
  t004_oci_export.bash
  t005_oci_names.bash
```

File naming is fixed as `t[0-9][0-9]_<verb>.bash`, so lexicographic order
is execution order and adding a case is "a file appears". First commit
ships t001–t005; `serve`/`mount` scenarios are deferred (each carries its
own availability gate + skip handling and they already have library-level
smoke coverage).

## The contract

The contract is the protocol, not the implementation. A future pure-Go
rewrite would adopt every line below unchanged and translate each bash
scenario function into a Go function one-for-one.

### Running the suite

Two equivalent entry points, both hitting the same `run.sh`:

- **Directly** (typical dev use): `go build && bash e2e/run.sh` — build the
  binary, then run the suite (which picks up the local `../blob`).
  `BLOB_E2E_KEEP=1` leaves the scratch dirs for inspection.
- **Via go test** (opt-in): `BLOB_E2E=1 go test -v -run TestE2E ./e2e/` —
  `TestE2E` is a thin adapter over `run.sh`; it does no building.

> **A build is a prerequisite — `go test` is not enough.** Neither path builds
> a binary (that would be odd for a test); both run the `blob` they resolve:
> an explicit `BLOB_BIN`, else the **local** build (`<repo>/blob`, what `go
> build` produces), else `blob` from `PATH` (what `go install` refreshes).
> So, exactly as we document that a build is required before `go test`, a
> current build is required before the suite: `go build` from the repo root.
> Local-first is deliberate — `go build && ./e2e/run.sh` tests the build you
> just made, not a shadowing install; `go install` is not needed at all for
> the suite to run (the local build is preferred). A missing binary (no local
> build *and* not on PATH) is a *setup failure*, not a skip: `run.sh` fails
> loudly rather than silently passing.

### Environment

The suite understands only these (nothing else is read):

| variable        | set by                   | meaning                                 |
|-----------------|--------------------------|-----------------------------------------|
| `BLOB_E2E`      | caller                   | `1` = run; unset = `TestE2E` skips      |
| `BLOB_BIN`      | caller                   | explicit binary path (else local/PATH)  |
| `BLOB_TMP`      | caller / the wrapper     | scratch root (wrapper: `t.TempDir()`)   |
| `BLOB_E2E_KEEP` | caller                   | `1` = leave scratch for debugging       |

### Exit codes (run.sh)

| code | meaning                                    |
|------|--------------------------------------------|
| `0`  | all passed, some may have skipped          |
| `77` | every scenario skipped (autotools SKIP)    |
| `1`  | at least one scenario failed               |

The wrapper maps `0` → pass, `77` → `t.Skip`, else → `t.Fatal`.

### Isolation

Each scenario creates its own `BLOB_DIR` under `BLOB_TMP` (`t.TempDir()`
is reaped on wrapper exit). No scenario reads another's store; there is no
cross-scenario state.

### Helper surface (lib.sh)

The helper names are the contract (a Go rewrite keeps them):

- `expect_ok "<desc>" cmd args...` — `cmd` must exit 0.
- `expect_fail "<desc>" cmd args...` — `cmd` must exit non-zero.
- `assert_eq "<desc>" want got`
- `assert_file_eq "<desc>" path-a path-b` (via `cmp`)
- `skip "<reason>"` — prints `SKIP: <reason>` and exits 77 (scenario-local).

`blob …` runs `$BLOB_BIN -p "$BLOB_DIR" …`; every assertion prints the
scenario, the assertion, and (on failure) both sides, for a readable diff.

## File sketches

### `e2e/e2e_test.go`

```go
package e2e

import (
    "os"
    "os/exec"
    "path/filepath"
    "testing"
)

// TestE2E is the opt-in (BLOB_E2E=1) CLI test. It is a thin adapter over the
// shell suite: it does NOT build a binary (the user runs `go build` / `go
// install` first — see the environment section), points scratch at a temp
// dir, execs run.sh from the suite dir, and maps the exit code to the
// testing framework (77 = everything skipped). BLOB_BIN is left to the
// shell-side resolution in lib.sh (explicit var, then <repo>/blob, then
// PATH); a missing binary is a setup failure, not a skip.
func TestE2E(t *testing.T) {
    if os.Getenv("BLOB_E2E") == "" {
        t.Skip("set BLOB_E2E=1 to run the CLI e2e test")
    }
    tmp := t.TempDir()
    env := append(os.Environ(), "BLOB_TMP="+tmp)
    // run.sh sits next to this test file; go test runs in the package dir.
    out, err := exec.Command("bash", filepath.Join("run.sh")).CombinedOutput()
    t.Logf("e2e:\n%s", out)
    if err != nil {
        if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 77 {
            t.Skip("all e2e scenarios skipped")
        }
        t.Fatalf("e2e failed: %v", err)
    }
}
```

The wrapper never builds. It just opts in, sets scratch, and execs the
suite — so a `go build` / `go install` that produced a stale binary is the
user's to keep fresh (see below).

### `e2e/run.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
source lib.sh

# Each tNNN_*.bash defines a function named after the file (minus the
# extension) and expects scenario_init to have run for its own store/work
# dir. We source them all (they share lib.sh) and call the function.
scenarios=()
for f in t[0-9][0-9]_*.bash; do
  scenarios+=("$(basename "$f" .bash)")
  source "$f"
done

skips=0
for s in "${scenarios[@]}"; do
    rc=0; "$s" || rc=$?
    case $rc in
       0)  echo "PASS  $s" ;;
      77)  echo "SKIP  $s"; skips=$((skips+1)) ;;
      *)   echo "FAIL  $s"; exit 1 ;;
    esac
done
[ "${#scenarios[@]}" -gt 0 ] && [ "$skips" -eq "${#scenarios[@]}" ] && exit 77
exit 0
```

### `e2e/lib.sh`

```bash
# shellcheck shell=bash
# Helpers shared by e2e scenarios. Self-contained so it runs directly and via
# the go-test wrapper (which reaps BLOB_TMP). cwd is <repo>/e2e (run.sh cds
# here before sourcing), so the local build is ../blob.

# Resolution is local-first: a build is a prerequisite, not an install.
# Try BLOB_BIN (if set), then ../blob (the `go build` output), then PATH.
if [ -z "${BLOB_BIN:-}" ]; then
  [ -x ../blob ] && BLOB_BIN=../blob
  [ -n "${BLOB_BIN:-}" ] || { BLOB_BIN=$(command -v blob || true); }
fi
[ -n "$BLOB_BIN" ] && [ -x "$BLOB_BIN" ] || {
  echo "FAIL: no blob binary found (go build, or set BLOB_BIN)" >&2; exit 1
}
BLOB_TMP=${BLOB_TMP:-$(mktemp -d "${TMPDIR:-/tmp}/blob-e2e.XXXXXX")}

# scenario_init NAME: sets BLOB_DIR, WORK under $BLOB_TMP/NAME
scenario_init() {
  local name=$1
  BLOB_DIR="$BLOB_TMP/$name/store"
  WORK="$BLOB_TMP/$name/work"
  mkdir -p "$BLOB_DIR" "$WORK"
}

# blob ... : run the store binary against this scenario's store dir.
blob() {
  echo "+ blob  $*"
  "$BLOB_BIN" -p "$BLOB_DIR" "$@"
}
```

(The exact flags are the CLI's; `lib.sh` settles them when the suite is
written. The shape is: one `blob` wrapper, one `scenario_init` per scenario,
plus the shared assertion helpers above. No scenario re-implements a path
or a scratch dir.)

## Scenarios

### `t001_add_get.bash` — add → lookup → get round trip

The minimal CLI sanity. `blob add` of a payload; `blob get` of it back to a
different path; `assert_file_eq`. Proves the binary, the store, and the
get path all agree.

### `t002_oci_import_bare.bash` — bare-file OCI import

A ~1 MiB random file; `blob oci import` (bare, no `--name`); then
`blob get` by the file's digest and `assert_file_eq` against the original.
This exercises the `artifactSource` fix end-to-end (fresh reader per `Get`,
not a one-shot reader consumed by `Verify`).

### `t003_oci_import_tampered.bash` — the verification feature

Build a layout on disk where a manifest names a digest whose *stored* bytes
do not hash to it (the exact shape `oci_test.go`'s `buildTamperedLayout`
builds). A `python3` heredoc assembles index/manifest + the two blobs under
their true digests, then the manifest is rewritten by hand to point at the
wrong digest — no Python dependency, just a script fed to the interpreter.
Then:
- `expect_fail` the `oci import`
- assert the store is still empty (no indexed name, no blob by the spoofed
  digest). This is the "store never holds a mismatch" invariant at the CLI
  level.

### `t004_oci_export.bash` — multi-blob layout round trip

Create a two-layer layout (again a `python3` heredoc), `blob oci import`,
`blob oci export` to a fresh dir, and diff the two directories blob-by-blob
(`diff -r`). Catches ordering / dedup regressions in the reachable set.

### `t005_oci_names.bash` — the `--name/--digest/--platform/--arch` flags

`blob oci import <file> --name a --name b --digest <sha256:…> --arch amd64`;
assert `blob lookup a` and `blob lookup b` both resolve, and `blob list`
shows both names. Verifies the "names are a restriction layer on top of the
store" contract end-to-end.

## Gates to add

`IMPLEMENTATION.md` gains one bullet, matching the existing smoke-test
pattern:

```
- **E2E** (opt-in via `BLOB_E2E`): the CLI suite (`e2e/`). Requires a current
  build (`go build` first; the suite does not build). Runs the scenarios
  against a local store. Skips if `BLOB_E2E` is unset; fails loudly if no
  `blob` binary is resolvable, and skips a scenario that needs a missing tool.
    BLOB_E2E=1 go test -v -run TestE2E ./e2e
```

And (cheapest gate, mirrors `golangci-lint` for the shell) `shellcheck` on
`e2e/*.sh e2e/*.bash`, if it is on the developer's machine.

## Deferred

- `serve` and `mount` scenarios — they need their own availability gates
  (FUSE, a live daemon) plus the existing smoke coverage at the library
  level already catches their core contract; deferred until the import
  layer is done and we want CLI-level coverage of them.
- A Makefile. The e2e workflow is drifting toward one (`go build` → `run.sh`,
  plus the existing `go test` / `go install` / `shellcheck` gates), a natural
  fit for a short Makefile (GNU `make`); shell scripts are bash. Deliberately
  **deferred to a future reorg** — this suite runs fine on plain
  `go build && bash e2e/run.sh` today.
- CI wiring. The repo has no CI file today; adding one is a separate
  decision.
- A pure-Go e2e suite. If the bash suite grows painful, the contract above
  lets a future `e2e/e2e.go` (or `e2e_test.go` with subtests) take over with
  the same helper names and the same per-scenario functions.

## Open questions

- Do t004 and t005 ship together with t001–t003, or is t001–t003 the first
  commit and t004–t005 a follow-up? (First commit = the verification
  feature, its fix, and the bare-file path.)

## Decisions

- **One file per scenario for the bash suite** (`tNNN_*.bash`). Each case is a
  self-contained function, so adding a case is "a file appears" and a failing
  case is easy to bisect by eye. When/if the suite is converted to Go we would
  consider collapsing to a single file with one function per scenario (the Go
  contract above — helper names + per-scenario functions — already supports
  either shape).
