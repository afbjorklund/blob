# t003_oci_import_tampered — the verification feature, at the CLI level.
#
# Build a layout whose manifest names a digest whose *stored* bytes do not hash
# to it (the exact shape oci_test.go's buildTamperedLayout builds), then `oci
# import` it. The store's integrity gate must refuse it, and the post-invariant
# is that the store is still empty — no blob under the spoofed digest, no name.
# That is the "store never holds a content/digest mismatch" property, seen
# through the user-facing CLI.
t003_oci_import_tampered() {
  scenario_init t003

  command -v sha256sum >/dev/null 2>&1 || { echo "SKIP: t003 needs sha256sum"; return 77; }

  local layout="$WORK/good"
  printf 'the real layer content that the manifest does not claim\n' > "$WORK/real-layer" \
    || { echo "FAIL: seed real-layer" >&2; return 1; }
  local rc=0
  oci_layout_build "$layout" "$WORK/real-layer" || rc=$?
  if [ "$rc" -eq 77 ]; then
    echo "SKIP: t003 needs layout tooling"
    return 77
  fi
  [ "$rc" -eq 0 ] || { echo "FAIL: building layout rc=$rc" >&2; return 1; }

  # Corrupt the layout: overwrite the stored layer bytes with content that does
  # not hash to the digest the manifest still names. The layout is now
  # internally valid JSON, content-inconsistent — what Verify must reject.
  local dhex
  dhex="$(sha256sum "$WORK/real-layer" | awk '{print $1}')" || { echo "FAIL: compute layer digest" >&2; return 1; }
  printf 'TAMPERED BYTES NOT HASHING TO THE CLAIMED DIGEST\n' > "$layout/blobs/sha256/$dhex" \
    || { echo "FAIL: could not write the tampered blob" >&2; return 1; }

  # The import must fail (Verify refuses before a single blob is committed).
  expect_fail "oci import of a tampered layout" blob oci import "$layout" \
    || return 1

  # Post-invariant: nothing landed. No blob file, no name.
  assert_empty_store "tampered import left the store clean" \
    || return 1

  return 0
}
