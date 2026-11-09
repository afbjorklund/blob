# t004_oci_export — multi-blob layout round trip.
#
# A two-layer layout (index → manifest → config + two layers) is imported into
# a store, then exported to a fresh directory and compared blob-for-blob with
# the original. Catches any ordering / reachable-set / dedup regression on the
# import→export path, end-to-end.
t004_oci_export() {
  scenario_init t004

  command -v sha256sum >/dev/null 2>&1 || skip "t004 needs sha256sum"

  printf 'layer one payload (multi-blob image)\n' > "$WORK/layer1"
  printf 'layer two payload, different content\n' > "$WORK/layer2"
  local layout="$WORK/layout"
  oci_layout_build "$layout" "$WORK/layer1" "$WORK/layer2" || return 1

  expect_ok "oci import (2-layer layout)" blob oci import "$layout" --name img || return 1

  # Both layers must be reachable / listed through the store.
  local ls
  ls="$(expect_ok "oci ls img" blob oci ls img)" || return 1
  local l1hex l2hex
  l1hex="$(sha256sum "$WORK/layer1" | awk '{print $1}')"
  l2hex="$(sha256sum "$WORK/layer2" | awk '{print $1}')"
  printf '%s\n' "$ls" | grep -q "sha256:$l1hex" \
    || { echo "FAIL: oci ls missed layer 1" >&2; return 1; }
  printf '%s\n' "$ls" | grep -q "sha256:$l2hex" \
    || { echo "FAIL: oci ls missed layer 2" >&2; return 1; }

  # Export to a fresh dir, then byte-for-byte compare every shared blob.
  local expdir="$WORK/export"
  expect_ok "oci export img -o $expdir" blob oci export img -o "$expdir" || return 1
  [ -f "$expdir/index.json" ] || { echo "FAIL: export has no index.json" >&2; return 1; }

  # The exported blobs/ tree must carry each input layer byte-for-byte.
  for h in "$l1hex" "$l2hex"; do
    [ -f "$expdir/blobs/sha256/$h" ] \
      || { echo "FAIL: export layout is missing blob $h" >&2; return 1; }
  done
  local l1exp
  l1exp="$expdir/blobs/sha256/$l1hex"
  assert_file_eq "exported layer1 bytes" "$WORK/layer1" "$l1exp" || return 1
  local l2exp
  l2exp="$expdir/blobs/sha256/$l2hex"
  assert_file_eq "exported layer2 bytes" "$WORK/layer2" "$l2exp" || return 1

  return 0
}
