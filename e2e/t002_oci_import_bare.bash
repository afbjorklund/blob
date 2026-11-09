# t002_oci_import_bare — bare-file OCI import, the artifact path.
#
# A ~1 MiB random file imported by `oci import` (a bare file is wrapped as a
# single-layer OCI artifact). The name and the CID both resolve back to the
# file's own bytes; `oci export` reconstructs the artifact scaffold. Exercises
# the artifactSource contract end-to-end: every blob must be re-readable (a
# fresh reader per Get; import's Verify and Copy each read the layer once).
t002_oci_import_bare() {
  scenario_init t002

  command -v sha256sum >/dev/null 2>&1 || skip "t002 needs sha256sum"

  # ~1 MiB random payload. A bare file is wrapped as a single-layer artifact;
  # the name recorded (basename) points at the file's own bytes.
  head -c 1048576 /dev/urandom > "$WORK/blob" \
    || { echo "FAIL: could not seed payload" >&2; return 1; }

  expect_ok "oci import (bare file)" blob oci import "$WORK/blob" || return 1

  # The name (the file's basename) points at the file's own bytes — the
  # artifact's single layer, the same key `blob add` uses. `lookup` resolves
  # it to a CID; `get` returns the file, not the scaffold.
  local name lookupcid
  name="$(basename "$WORK/blob")"
  lookupcid="$(expect_ok "lookup by name" blob lookup "$name")" || return 1
  [ -n "$lookupcid" ] || { echo "FAIL: lookup gave no CID" >&2; return 1; }
  expect_ok "get by name" blob get "$name" -o "$WORK/round" || return 1
  assert_file_eq "imported bytes round-trip" "$WORK/blob" "$WORK/round" || return 1

  # The scaffold must be reconstructable: `oci export` to a fresh dir produces a
  # valid layout (index.json + blobs) whose single layer is our file.
  local expdir="$WORK/export"
  expect_ok "oci export to dir" blob oci export "$name" -o "$expdir" || return 1
  [ -f "$expdir/index.json" ] || { echo "FAIL: export has no index.json" >&2; return 1; }
  local layerhex
  layerhex="$(sha256sum "$WORK/blob" | awk '{print $1}')" || return 1
  [ -f "$expdir/blobs/sha256/$layerhex" ] \
    || { echo "FAIL: export layout is missing the layer blob ($layerhex)" >&2; return 1; }
  assert_file_eq "exported layer bytes" "$WORK/blob" "$expdir/blobs/sha256/$layerhex" || return 1

  return 0
}
