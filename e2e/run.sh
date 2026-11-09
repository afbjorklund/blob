#!/usr/bin/env bash
# Orchestrator for the e2e CLI suite. Sources lib.sh (which settles BLOB_BIN,
# BLOB_TMP and the assertions), then each tNNN_*.bash in name order. Each file
# defines a function named after the file stem; we call it and map its exit
# code: 0 = PASS, 77 = SKIP (counted), anything else = FAIL (abort).
#
# Exit codes: 0 = all passed (skips allowed); 77 = every scenario skipped;
# 1 = at least one failed. The Go wrapper (e2e_test.go) maps these onto
# pass / t.Skip / t.Fatal. run with: `go build && bash e2e/run.sh` from the repo
# root (it cds here and picks up ../blob), or `BLOB_E2E=1 go test -run TestE2E ./e2e`.
#
# The lint suppression for this file's `source`s (SC1090/SC1091 — sourcing shell
# scripts is the design) lives in the repo's .shellcheckrc, not here — it is
# config, like golangci-lint's .golangci.yml.
set -euo pipefail
cd "$(dirname "$0")"
shopt -s nullglob
source lib.sh

scenarios=()
for f in t[0-9][0-9][0-9]_*.bash; do
  scenarios+=("$(basename "$f" .bash)")
  source "$f"
done

if [ "${#scenarios[@]}" -eq 0 ]; then
  echo "FAIL: no e2e scenarios found (expected tNNN_…)" >&2
  exit 1
fi

skips=0
for s in "${scenarios[@]}"; do
  rc=0
  "$s" || rc=$?
  case "$rc" in
    0) echo "PASS  $s" ;;
    77) echo "SKIP  $s"; skips=$((skips+1)) ;;
    *) echo "FAIL  $s (exit $rc)"; exit 1 ;;
  esac
done

if [ "${#scenarios[@]}" -eq "$skips" ]; then
  echo "all skipped: ${#scenarios[@]} scenario(s)"
  exit 77
fi
echo "done: ${#scenarios[@]} scenario(s), $skips skipped"
exit 0
