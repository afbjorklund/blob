# t008_oci_import_compressed_layer — the compressed-layer import fix.
#
# A real OCI image whose manifest names a *compressed* (gzip) layer: the
# config's rootfs.diff_ids name the *uncompressed* sha256 (the .tar), but the
# layout stores the .tar.gz under its own (different) digest. Before this
# change the import stored the compressed bytes as-is, so the config's
# diff_id could never be resolved from the store — the documented
# "compressed or absent" failure. Now `oci import` decompresses the layer
# into the canonical .tar and stores it under the diff_id, so the config,
# the synthetic manifest, and the CAS all agree.
#
# What we assert
# --------------
#   * import succeeds for a real-image layout whose single layer is gzip.
#   * the name resolves to the image config (Docker Image ID), and `oci ls`
#     of that config names the *uncompressed* (diff_id) layer digest.
#   * the decompressed .tar is in the store under the diff_id, byte-for-byte
#     the original tar; the compressed digest is NOT in the store (default
#     path does not keep the compressed form).
#   * `oci ls <config>` synthesizes the canonical tree from the config +
#     layers (the configImage read path) — consistent with what we stored.
t008_oci_import_compressed_layer() {
  scenario_init t008

  command -v sha256sum >/dev/null 2>&1 || skip "t008 needs sha256sum"
  command -v gzip >/dev/null 2>&1 || skip "t008 needs gzip"
  command -v tar >/dev/null 2>&1 || skip "t008 needs tar"

  # Build a real tar layer with a couple of files, plus its gzip form.
  local root="$WORK/root"
  mkdir -p "$root/bin" || { echo "FAIL: cannot seed root" >&2; return 1; }
  printf 'hello from layer\n' > "$root/hello.txt" || return 1
  printf '#!/bin/sh\necho hi\n' > "$root/bin/tool" || return 1
  local tartargz="$WORK/layer.tar.gz"
  ( cd "$root" && tar -cf - . | gzip -n > "$tartargz" ) || { echo "FAIL: cannot build gzip layer" >&2; return 1; }

  local tarsha gzsha
  gzsha="$(sha256sum "$tartargz" | awk '{print $1}')" || return 1
  local tarbytes="$WORK/layer.tar"
  gzip -d < "$tartargz" > "$tarbytes" || { echo "FAIL: cannot gunzip" >&2; return 1; }
  tarsha="$(sha256sum "$tarbytes" | awk '{print $1}')" || return 1
  local gzsize
  gzsize="$(wc -c < "$tartargz" | tr -d '[:space:]')"

  # The config names the UNCOMPRESSED sha256 (the .tar) as its single diff_id.
  local layout="$WORK/layout"
  local blobs="$layout/blobs/sha256"
  mkdir -p "$blobs" || return 1
  printf '%s' '{"imageLayoutVersion":"1.0.0"}' > "$layout/oci-layout"

  local cfghex cfgsize
  local cfg="{\"architecture\":\"amd64\",\"os\":\"linux\",\"rootfs\":{\"type\":\"layers\",\"diff_ids\":[\"sha256:$tarsha\"]}}"
  cfghex="$(printf '%s' "$cfg" | sha256sum | awk '{print $1}')" || return 1
  cfgsize="$(printf '%s' "$cfg" | wc -c | tr -d '[:space:]')"
  printf '%s' "$cfg" > "$blobs/$cfghex" || return 1

  # The layout stores the COMPRESSED bytes under the compressed digest.
  cp "$tartargz" "$blobs/$gzsha" || return 1

  local manifest
  manifest="{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"config\":{\"mediaType\":\"application/vnd.oci.image.config.v1+json\",\"digest\":\"sha256:$cfghex\",\"size\":$cfgsize},\"layers\":[{\"mediaType\":\"application/vnd.oci.image.layer.v1.tar+gzip\",\"digest\":\"sha256:$gzsha\",\"size\":$gzsize}]}"
  local manhex mansize
  manhex="$(printf '%s' "$manifest" | sha256sum | awk '{print $1}')" || return 1
  mansize="$(printf '%s' "$manifest" | wc -c | tr -d '[:space:]')"
  printf '%s' "$manifest" > "$blobs/$manhex" || return 1

  local index
  index="{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.index.v1+json\",\"manifests\":[{\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"digest\":\"sha256:$manhex\",\"size\":$mansize}]}"
  printf '%s' "$index" > "$layout/index.json" || return 1

  # (a) Import succeeds.
  expect_ok "oci import (compressed layer)" blob oci import "$layout" --name img || return 1

  # (b) The name resolves to the image config (Docker Image ID); `oci ls` of it
  #     names the UNCOMPRESSED (diff_id) layer digest, not the compressed one.
  local lscfg
  lscfg="$(expect_ok "oci ls <config>" blob oci ls "sha256:$cfghex")" || return 1
  printf '%s\n' "$lscfg" | grep -q "sha256:$tarsha" \
    || { echo "FAIL: oci ls <config> missed the uncompressed diff_id layer" >&2; return 1; }
  printf '%s\n' "$lscfg" | grep -q "sha256:$gzsha" \
    && { echo "FAIL: oci ls <config> named the compressed digest (want the diff_id)" >&2; return 1; }

  # (c) The decompressed .tar is in the store under the diff_id, byte-for-byte
  #     the original tar (gunzipped) — the canonical uncompressed layer.
  local out_c="$WORK/out_c"
  expect_ok "oci export <config>" blob oci export "sha256:$cfghex" -o "$out_c" || return 1
  [ -f "$out_c/blobs/sha256/$tarsha" ] \
    || { echo "FAIL: store missing the decompressed layer under the diff_id" >&2; return 1; }
  assert_file_eq "decompressed layer bytes" "$tarbytes" "$out_c/blobs/sha256/$tarsha" || return 1

  # (d) The compressed digest is NOT in the store (default path does not keep
  #     the compressed form; it is only --original that would).
  expect_fail "compressed digest not stored (default path)" blob cat "sha256:$gzsha" || return 1

  return 0
}
