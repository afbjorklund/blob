# t005_oci_names — the --name/--digest/--arch flags of `oci import`.
#
# A bare file imported with two explicit names plus an origin reference
# (a --digest and an --arch, which together pin the
# oci/<variant>/<os>/<arch>/<algo>/<hex> name). Verifies the "names are a
# restriction layer on top of the one blob" contract end-to-end: every name
# resolves to the same CID, the plain names list by default, and the OCI origin
# reference appears only under --oci.
t005_oci_names() {
  scenario_init t005

  command -v sha256sum >/dev/null 2>&1 || skip "t005 needs sha256sum"

  printf 'a named artifact of the same content\n' > "$WORK/imgfile"
  local dhex
  dhex="$(sha256sum "$WORK/imgfile" | awk '{print $1}')" || return 1

  local ref="oci/blob/linux/amd64/sha256/$dhex"
  expect_ok "oci import --name a --name b --digest --arch" \
    blob oci import "$WORK/imgfile" --name a --name b --digest "sha256:$dhex" --arch amd64 \
    || return 1

  # Both user names resolve to the same CID (same underlying blob).
  local ca cb
  ca="$(expect_ok "lookup a" blob lookup a)" || return 1
  cb="$(expect_ok "lookup b" blob lookup b)" || return 1
  [ -n "$ca" ] || { echo "FAIL: lookup a gave no CID" >&2; return 1; }
  assert_eq "a and b resolve to the same CID" "$ca" "$cb" || return 1

  # Plain names are visible in the default listing.
  local listed
  listed="$(expect_ok "blob list" blob list)" || return 1
  printf '%s\n' "$listed" | grep -qx "a" \
    || { echo "FAIL: list missing name 'a' (got: $listed)" >&2; return 1; }
  printf '%s\n' "$listed" | grep -qx "b" \
    || { echo "FAIL: list missing name 'b' (got: $listed)" >&2; return 1; }

  # The OCI origin reference is hidden by default, shown with --oci.
  local oci_list
  oci_list="$(expect_ok "blob list --oci" blob list --oci)" || return 1
  printf '%s\n' "$oci_list" | grep -qx "$ref" \
    || { echo "FAIL: list --oci missing the origin reference $ref (got: $oci_list)" >&2; return 1; }

  # And that origin reference also resolves to the same CID as the names.
  local cref
  cref="$(expect_ok "lookup the origin reference" blob lookup "$ref")" || return 1
  assert_eq "origin reference resolves to the same CID" "$ca" "$cref" || return 1

  return 0
}
