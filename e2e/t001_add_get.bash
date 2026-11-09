# t001_add_get — add → get round trip.
#
# The minimal CLI sanity: `blob add` records a file, `blob get` retrieves it by
# the CID `add` printed, and the bytes come back identical. Proves the binary,
# the dir store, and the get path all agree.
t001_add_get() {
  scenario_init t001

  printf 'payload-one\nsecond line of the round-trip\n' > "$WORK/input"

  local out cid
  out="$(blob add "$WORK/input")" || { echo "FAIL: blob add" >&2; return 1; }
  cid="$(printf '%s' "$out" | awk '{print $2}')" || return 1
  [ -n "$cid" ] || { echo "FAIL: add printed no CID (output: $out)" >&2; return 1; }

  expect_ok "blob get" blob get "$cid" -o "$WORK/out" || return 1
  assert_file_eq "round-trip bytes" "$WORK/input" "$WORK/out" || return 1

  # The name index: the basename is recorded by `add`.
  local listed
  listed="$(blob list)" || { echo "FAIL: blob list" >&2; return 1; }
  printf '%s\n' "$listed" | grep -qx "input" \
    || { echo "FAIL: list does not show name 'input' (got: $listed)" >&2; return 1; }

  return 0
}
