# Shared helpers for the e2e CLI suite. Sourced first by run.sh to settle the
# binary and scratch root, then it defines the assertions and the `blob` wrapper
# the scenarios share. cwd is the e2e/ dir (run.sh cds here before sourcing), so
# the local build is ../blob. Lint config (dialect + source-suppressions) lives
# in the repo's .shellcheckrc, not per file.
#
# The contract (helper names, the exit-code meaning of the scenario functions)
# is what a future pure-Go rewrite keeps; the scenarios are the only thing that
# changes shape.

# --- binary resolution: local-first, a build is a prerequisite (not an install) ---
# Try BLOB_BIN (if the caller set it), else ../blob (what `go build` produces in
# the repo root), else blob on PATH. A missing binary is a setup failure — fail
# loudly rather than pass the suite with nothing to run.
if [ -z "${BLOB_BIN:-}" ]; then
  if [ -x ../blob ]; then
    BLOB_BIN=../blob
  else
    BLOB_BIN="$(command -v blob || true)"
  fi
fi
if [ -z "$BLOB_BIN" ] || [ ! -x "$BLOB_BIN" ]; then
  echo "FAIL: no blob binary to run (run \`go build\` from the repo root, or set BLOB_BIN)" >&2
  exit 1
fi

# --- scratch root: the Go wrapper passes BLOB_TMP (its t.TempDir); direct runs
# mint one of our own and reap it on exit unless BLOB_E2E_KEEP=1. ---
_e2e_owns_tmp=0
if [ -z "${BLOB_TMP:-}" ]; then
  BLOB_TMP="$(mktemp -d "${TMPDIR:-/tmp}/blob-e2e.XXXXXX")"
  _e2e_owns_tmp=1
fi
_e2e_cleanup() {
  if [ "$_e2e_owns_tmp" -eq 1 ] && [ "${BLOB_E2E_KEEP:-0}" != "1" ]; then
    rm -rf "$BLOB_TMP"
  fi
}
trap '_e2e_cleanup' EXIT

# scenario_init NAME: point this scenario's store and work dir under BLOB_TMP.
# Each scenario gets its own subtree; no scenario reads another's state.
scenario_init() {
  local name=$1
  BLOB_DIR="$BLOB_TMP/$name/store"
  WORK="$BLOB_TMP/$name/work"
  mkdir -p "$BLOB_DIR" "$WORK"
}

# blob: run the store binary against this scenario's dir (dir backend, the
# default) with the given verb and arguments. The scenario's own stdout is
# whatever the command prints; assertions are separate so a command's message
# never pollutes a captured value.
blob() {
  "$BLOB_BIN" -p "$BLOB_DIR" "$@"
}

# expect_ok "desc" cmd args... — cmd must exit 0.
expect_ok() {
  local desc=$1
  shift
  if ! "$@"; then
    echo "FAIL: $desc (command: $*)" >&2
    return 1
  fi
}

# expect_fail "desc" cmd args... — cmd must exit non-zero.
expect_fail() {
  local desc=$1
  shift
  if "$@"; then
    echo "FAIL: $desc (command unexpectedly succeeded: $*)" >&2
    return 1
  fi
}

# assert_eq "desc" want got.
assert_eq() {
  local desc=$1 want=$2 got=$3
  if [ "$want" != "$got" ]; then
    echo "FAIL: $desc (want: $want; got: $got)" >&2
    return 1
  fi
}

# assert_file_eq "desc" a b — via cmp, byte-for-byte.
assert_file_eq() {
  local desc=$1 a=$2 b=$3
  if ! cmp -s "$a" "$b"; then
    echo "FAIL: $desc (files differ: $a vs $b)" >&2
    return 1
  fi
}

# skip "reason" — mark this scenario skipped (run.sh counts it, not a failure).
skip() {
  echo "SKIP: $1"
  return 77
}

# oci_layout_build <dir> <layer>... builds a valid, single-manifest OCI layout
# (index.json + blobs/sha256/* + the oci-layout marker) with the given layer
# files. The config is the canonical empty {} config; every digest is derived
# from the content, so the layout is internally consistent — t004 imports it,
# t003 builds it and then corrupts the layer to make it a rejected import.
oci_layout_build() {
  local layout=$1
  shift
  command -v sha256sum >/dev/null 2>&1 || { echo "SKIP: needs sha256sum" >&2; return 77; }

  local blobs="$layout/blobs/sha256"
  mkdir -p "$blobs" || return 1
  printf '%s' '{"imageLayoutVersion":"1.0.0"}' > "$layout/oci-layout"

  local cfg='{}'
  local cfghex cfgsize
  cfghex="$(printf '%s' "$cfg" | sha256sum | awk '{print $1}')" || return 1
  cfgsize="$(printf '%s' "$cfg" | wc -c | tr -d '[:space:]')"
  printf '%s' "$cfg" > "$blobs/$cfghex" || return 1

  local layers_json l shex ssize manhex mansize
  for l in "$@"; do
    [ -f "$l" ] || { echo "oci_layout_build: no such layer: $l" >&2; return 1; }
    shex="$(sha256sum "$l" | awk '{print $1}')" || return 1
    ssize="$(wc -c < "$l" | tr -d '[:space:]')"
    cp "$l" "$blobs/$shex" || return 1
    layers_json="${layers_json:+$layers_json,}{\"mediaType\":\"application/vnd.oci.image.layer.v1.tar\",\"digest\":\"sha256:$shex\",\"size\":$ssize}"
  done

  local manifest
  manifest="{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"config\":{\"mediaType\":\"application/vnd.oci.image.config.v1+json\",\"digest\":\"sha256:$cfghex\",\"size\":$cfgsize},\"layers\":[$layers_json]}"
  manhex="$(printf '%s' "$manifest" | sha256sum | awk '{print $1}')" || return 1
  mansize="$(printf '%s' "$manifest" | wc -c | tr -d '[:space:]')"
  printf '%s' "$manifest" > "$blobs/$manhex" || return 1

  local index
  index="{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.index.v1+json\",\"manifests\":[{\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"digest\":\"sha256:$manhex\",\"size\":$mansize}]}"
  printf '%s' "$index" > "$layout/index.json" || return 1
  return 0
}

# assert_empty_store "desc" — the store holds nothing (no blob file, and the
# name index is absent or empty). The post-invariant for a refused import: the
# store never keeps a content/digest mismatch nor a name.
assert_empty_store() {
  local desc=$1
  local stray names
  stray="$(find "$BLOB_DIR" -type f 2>/dev/null)" || true
  if [ -n "$stray" ]; then
    echo "FAIL: $desc — the store has blob content: $(printf '%s' "$stray" | tr '\n' ' ')" >&2
    return 1
  fi
  names="$(find "$BLOB_DIR/index" -mindepth 1 2>/dev/null)" || true
  if [ -n "$names" ]; then
    echo "FAIL: $desc — the store has index names: $(printf '%s' "$names" | tr '\n' ' ')" >&2
    return 1
  fi
  return 0
}
