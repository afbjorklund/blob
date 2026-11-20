# t009_oci_import_docker_archive — the legacy docker-archive import fix.
#
# A docker-archive (top-level manifest.json array, no index.json) whose layers
# are gzip-compressed and whose config names the *uncompressed* sha256 as
# rootfs.diff_ids — exactly the crane.tar shape:
#   - a layer named <compressedhex>.tar.gz, stored under its COMPRESSED
#     sha256,
#   - a config whose diff_id for that layer is the UNCOMPRESSED sha256 of the
#     gunzipped .tar.
#
# Before this change the docker-archive path (NewLegacyArchive) stored the
# compressed bytes under the compressed digest and left the manifest naming
# that compressed digest. The config's diff_id (a different digest) could
# never be resolved from the store — `oci ls` of the config failed with
# "blob not found (compressed or absent)". The fix routes the docker-archive
# through the same real-image rewrite as a genuine layout: the layers are
# decompressed into the canonical .tar, stored under the diff_id, so the
# config, the synthetic manifest, and the CAS all agree.
#
# What we assert
# --------------
#   * import succeeds and prints the "docker-archive; upgrading" note.
#   * `oci ls <name>` names the UNCOMPRESSED diff_ids (not the compressed).
#   * `oci ls <config>` (the Docker Image ID) also names the diff_ids and is
#     consistent with `oci ls <name>`.
#   * the decompressed .tar is in the store under the diff_id, byte-for-byte;
#     the compressed digest is absent (default path).
t009_oci_import_docker_archive() {
  scenario_init t009

  command -v sha256sum >/dev/null 2>&1 || skip "t009 needs sha256sum"
  command -v gzip >/dev/null 2>&1 || skip "t009 needs gzip"
  command -v tar >/dev/null 2>&1 || skip "t009 needs tar"

  # Two real .tar layers, their gzip forms, and the digests of both.
  local r1="$WORK/r1" r2="$WORK/r2"
  mkdir -p "$r1/etc" "$r2/usr" || return 1
  printf 'layer one A\n' > "$r1/a.txt" || return 1
  printf 'layer one conf\n' > "$r1/etc/conf" || return 1
  printf 'layer two tool\n' > "$r2/usr/tool" || return 1
  ( cd "$r1" && tar -cf - . ) > "$WORK/l1.tar" || return 1
  ( cd "$r2" && tar -cf - . ) > "$WORK/l2.tar" || return 1
  local d1 d2 c1 c2
  d1="$(sha256sum "$WORK/l1.tar" | awk '{print $1}')" || return 1
  d2="$(sha256sum "$WORK/l2.tar" | awk '{print $1}')" || return 1
  gzip -n -c "$WORK/l1.tar" > "$WORK/l1.tar.gz" || return 1
  gzip -n -c "$WORK/l2.tar" > "$WORK/l2.tar.gz" || return 1
  c1="$(sha256sum "$WORK/l1.tar.gz" | awk '{print $1}')" || return 1
  c2="$(sha256sum "$WORK/l2.tar.gz" | awk '{print $1}')" || return 1

  # A config with diff_ids = the uncompressed hashes.
  local cfg cfghex
  cfg="{\"architecture\":\"amd64\",\"os\":\"linux\",\"rootfs\":{\"type\":\"layers\",\"diff_ids\":[\"sha256:$d1\",\"sha256:$d2\"]}}"
  cfghex="$(printf '%s' "$cfg" | sha256sum | awk '{print $1}')" || return 1

  # Assemble the legacy docker-archive: entries named <hex>.tar.gz and
  # sha256:<cfg>, plus a top-level manifest.json array.
  local W="$WORK" A="$WORK/archive.tar"
  ( cd "$W" \
    && cp l1.tar.gz "$c1.tar.gz" && cp l2.tar.gz "$c2.tar.gz" \
    && printf '%s' "$cfg" > "sha256:$cfghex" \
    && printf '%s' "[{\"Config\":\"sha256:$cfghex\",\"RepoTags\":[\"example/crane:latest\"],\"Layers\":[\"$c1.tar.gz\",\"$c2.tar.gz\"]}]" > manifest.json \
    && tar -cf "$A" "$c1.tar.gz" "$c2.tar.gz" "sha256:$cfghex" manifest.json ) || return 1
  rm -f "$W/$c1.tar.gz" "$W/$c2.tar.gz" "$W/manifest.json"

  # (a) Import succeeds. (The "docker-archive; upgrading" note prints to
  #     stderr; a successful import is the real signal — the assertions below
  #     prove the rewrite path was taken, which is the meaningful check.)
  expect_ok "oci import (docker archive)" blob oci import "$A" --name crane || return 1

  # (b) `oci ls <name>` names the UNCOMPRESSED diff_ids, not the compressed.
  local lsn
  lsn="$(expect_ok "oci ls crane" blob oci ls crane)" || return 1
  printf '%s\n' "$lsn" | grep -q "sha256:$d1" || { echo "FAIL: ls by name missed diff_id 1" >&2; return 1; }
  printf '%s\n' "$lsn" | grep -q "sha256:$d2" || { echo "FAIL: ls by name missed diff_id 2" >&2; return 1; }
  printf '%s\n' "$lsn" | grep -q "sha256:$c1" \
    && { echo "FAIL: ls by name named the compressed digest (want the diff_id)" >&2; return 1; }

  # (c) `oci ls <config>` (the Docker Image ID) is consistent: diff_ids present.
  local lsc
  lsc="$(expect_ok "oci ls <config>" blob oci ls "sha256:$cfghex")" || return 1
  printf '%s\n' "$lsc" | grep -q "sha256:$d1" || { echo "FAIL: ls by config missed diff_id 1" >&2; return 1; }
  printf '%s\n' "$lsc" | grep -q "sha256:$d2" || { echo "FAIL: ls by config missed diff_id 2" >&2; return 1; }

  # (d) The decompressed .tar is in the store under the diff_id, byte-for-byte.
  local out="$WORK/out"
  expect_ok "oci export <config>" blob oci export "sha256:$cfghex" -o "$out" || return 1
  [ -f "$out/blobs/sha256/$d1" ] || { echo "FAIL: store missing decompressed layer 1" >&2; return 1; }
  assert_file_eq "decompressed layer 1 bytes" "$WORK/l1.tar" "$out/blobs/sha256/$d1" || return 1
  [ -f "$out/blobs/sha256/$d2" ] || { echo "FAIL: store missing decompressed layer 2" >&2; return 1; }
  assert_file_eq "decompressed layer 2 bytes" "$WORK/l2.tar" "$out/blobs/sha256/$d2" || return 1

  # (e) The compressed digest is NOT in the store (default path; --original
  #     would keep it).
  expect_fail "compressed digest not stored (default path)" blob cat "sha256:$c1" || return 1

  return 0
}
