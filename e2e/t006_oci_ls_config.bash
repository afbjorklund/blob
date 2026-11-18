# t006_oci_ls_config — `oci ls` by the image config digest (Docker Image ID).
#
# The config case added to `oci ls`: when the argument resolves to a config
# blob (a `sha256:<hex>` straight from `docker images` — the Docker Image ID),
# the command reads the config's rootfs (type + diff_ids), looks up each
# diff_id's size in the store, synthesizes the canonical index + manifest,
# and prints the tree. `oci export` of the same digest writes that canonical
# layout out — its digests differ from the author's annotated ones (the
# busybox shape).
#
# The setup mirrors a real `docker save` (busybox) image: two layers, a
# config carrying rootfs.diff_ids (uncompressed, so diff_ids ARE the stored
# layer digests — the documented supported shape), an annotated manifest,
# and an annotated index.
t006_oci_ls_config() {
  scenario_init t006

  command -v sha256sum >/dev/null 2>&1 || skip "t006 needs sha256sum"

  printf 'rootfs layer one (uncompressed, in the store)\n' > "$WORK/layer1"
  printf 'rootfs layer two\n' > "$WORK/layer2"
  local layout="$WORK/layout"
  oci_image_layout_real "$layout" "$WORK/layer1" "$WORK/layer2" || return 1
  [ -f "$WORK/config.json" ] || { echo "FAIL: builder produced no config.json" >&2; return 1; }

  expect_ok "oci import (a real image)" blob oci import "$layout" --name img || return 1

  local cfghex cfgd l1hex l2hex authidx exported
  cfghex="$(sha256sum "$WORK/config.json" | awk '{print $1}')"
  cfgd="sha256:$cfghex"
  l1hex="$(sha256sum "$WORK/layer1" | awk '{print $1}')"
  l2hex="$(sha256sum "$WORK/layer2" | awk '{print $1}')"
  authidx="$(sha256sum "$layout/index.json" | awk '{print $1}')"

  # (a) The existing path (via name → index) still works: real (author's
  #     annotated) manifest and both layers, with the real config digest.
  local lsname
  lsname="$(expect_ok "oci ls img" blob oci ls img)" || return 1
  printf '%s\n' "$lsname" | grep -q "sha256:$cfghex" \
    || { echo "FAIL: oci ls by name missed the config digest" >&2; return 1; }
  printf '%s\n' "$lsname" | grep -q "sha256:$l1hex" \
    || { echo "FAIL: oci ls by name missed layer 1" >&2; return 1; }
  printf '%s\n' "$lsname" | grep -q "sha256:$l2hex" \
    || { echo "FAIL: oci ls by name missed layer 2" >&2; return 1; }

  # (b) The new path: `oci ls <config-digest>` (the Docker Image ID).
  #     The index + manifest digests printed are the *canonical* ones — we
  #     assert the canonical index digest (the sha256 of the canonical
  #     index.json bytes, the annotation-free single-manifest one) and the
  #     canonical manifest digest (which must be present, even though the
  #     author's stored manifest digest differs) — and that the config and
  #     both layers are the real blobs (same digests, in the right positions).
  local lsd
  lsd="$(expect_ok "oci ls <config>" blob oci ls "$cfgd")" || return 1
  printf '%s\n' "$lsd" | grep -q "sha256:$cfghex" \
    || { echo "FAIL: oci ls by config missed the config digest" >&2; return 1; }
  printf '%s\n' "$lsd" | grep -q "sha256:$l1hex" \
    || { echo "FAIL: oci ls by config missed layer 1" >&2; return 1; }
  printf '%s\n' "$lsd" | grep -q "sha256:$l2hex" \
    || { echo "FAIL: oci ls by config missed layer 2" >&2; return 1; }
  # The canonical index is the annotation-free single-manifest form; the
  # author's index (stored, carrying io.containerd.image.name) is a
  # *different* blob with a different digest. The ls-by-config output is
  # the canonical tree — it must NOT name the author's stored index.
  printf '%s\n' "$lsd" | grep -q "sha256:$authidx" \
    && { echo "FAIL: oci ls by config named the author's annotated index ($authidx); want the canonical index only" >&2; return 1; }

  # (c) `oci export <config-digest>`: the same canonical layout, on disk.
  #     index.json + blobs/ with the real config bytes and the real layer
  #     bytes — byte-for-byte.
  local out="$WORK/out"
  expect_ok "oci export <config>" blob oci export "$cfgd" -o "$out" || return 1
  [ -f "$out/index.json" ] || { echo "FAIL: export produced no index.json" >&2; return 1; }
  [ -f "$out/blobs/sha256/$cfghex" ] || { echo "FAIL: export missing the config blob" >&2; return 1; }
  assert_file_eq "exported config bytes" "$WORK/config.json" "$out/blobs/sha256/$cfghex" || return 1
  [ -f "$out/blobs/sha256/$l1hex" ] || { echo "FAIL: export missing layer 1" >&2; return 1; }
  [ -f "$out/blobs/sha256/$l2hex" ] || { echo "FAIL: export missing layer 2" >&2; return 1; }
  assert_file_eq "exported layer 1 bytes" "$WORK/layer1" "$out/blobs/sha256/$l1hex" || return 1
  assert_file_eq "exported layer 2 bytes" "$WORK/layer2" "$out/blobs/sha256/$l2hex" || return 1
  exported="$(cat "$out/index.json")"
  printf '%s' "$exported" | grep -q 'io\.containerd\.image\.name' \
    && { echo "FAIL: exported index.json carries the author's annotation; want the canonical" >&2; return 1; }

  # (d) A layer digest is not a config: `oci ls <layer>` must NOT take the
  #     config path. It reaches the artifact path (which needs the artifact
  #     index in the store, which is not there — this layer belongs to an
  #     image, not a wrapped file) and errors with the "bare blob" message.
  #     We assert only that it fails.
  expect_fail "oci ls <layer> is not a config" blob oci ls "sha256:$l1hex" || return 1

  return 0
}

# oci_image_layout_real builds a single-manifest OCI layout in dir with two
# real layers, a real image config (rootfs.diff_ids = the two layer digests),
# an annotated manifest, and an annotated index — the busybox shape. It
# writes the config bytes to $WORK/config.json so the scenario can assert
# byte-for-byte round-trip. The layout is *not* the "canonical" form the
# store would synthesize: the index carries io.containerd.image.name and the
# manifest carries an annotation — exactly the busybox case.
oci_image_layout_real() {
  local layout=$1 l1=$2 l2=$3
  command -v sha256sum >/dev/null 2>&1 || return 77

  local blobs="$layout/blobs/sha256"
  mkdir -p "$blobs" || return 1
  printf '%s' '{"imageLayoutVersion":"1.0.0"}' > "$layout/oci-layout"

  local l1hex l1size l2hex l2size
  l1hex="$(sha256sum "$l1" | awk '{print $1}')" || return 1
  l1size="$(wc -c < "$l1" | tr -d '[:space:]')"
  l2hex="$(sha256sum "$l2" | awk '{print $1}')" || return 1
  l2size="$(wc -c < "$l2" | tr -d '[:space:]')"
  cp "$l1" "$blobs/$l1hex" || return 1
  cp "$l2" "$blobs/$l2hex" || return 1

  local cfg
  cfg="{\"architecture\":\"amd64\",\"os\":\"linux\",\"config\":{\"Env\":[\"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\"]},\"rootfs\":{\"type\":\"layers\",\"diff_ids\":[\"sha256:$l1hex\",\"sha256:$l2hex\"]}}"
  printf '%s' "$cfg" > "$WORK/config.json" || return 1
  local cfghex cfgsize
  cfghex="$(sha256sum "$WORK/config.json" | awk '{print $1}')" || return 1
  cfgsize="$(wc -c < "$WORK/config.json" | tr -d '[:space:]')"
  printf '%s' "$cfg" > "$blobs/$cfghex" || return 1

  # A real manifest with the two-layer array, plus an annotation (to make
  # the manifest digest differ from the canonical form).
  local manifest
  manifest="{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"config\":{\"mediaType\":\"application/vnd.oci.image.config.v1+json\",\"digest\":\"sha256:$cfghex\",\"size\":$cfgsize},\"layers\":[{\"mediaType\":\"application/vnd.oci.image.layer.v1.tar\",\"digest\":\"sha256:$l1hex\",\"size\":$l1size},{\"mediaType\":\"application/vnd.oci.image.layer.v1.tar\",\"digest\":\"sha256:$l2hex\",\"size\":$l2size}],\"annotations\":{\"org.opencontainers.image.licenses\":\"MIT\"}}"
  local manhex mansize
  manhex="$(printf '%s' "$manifest" | sha256sum | awk '{print $1}')" || return 1
  mansize="$(printf '%s' "$manifest" | wc -c | tr -d '[:space:]')"
  printf '%s' "$manifest" > "$blobs/$manhex" || return 1

  local index
  index="{\"schemaVersion\":2,\"mediaType\":\"application/vnd.oci.image.index.v1+json\",\"manifests\":[{\"mediaType\":\"application/vnd.oci.image.manifest.v1+json\",\"digest\":\"sha256:$manhex\",\"size\":$mansize,\"annotations\":{\"io.containerd.image.name\":\"docker.io/library/busybox:latest\",\"org.opencontainers.image.ref.name\":\"latest\"}}]}"
  printf '%s' "$index" > "$layout/index.json" || return 1
  return 0
}
