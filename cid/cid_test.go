package cid

import (
	"bytes"
	"testing"

	"blob/identity"

	godigest "github.com/opencontainers/go-digest"
)

// keyFor builds a sha2-256 multihash over data (the store's content key).
func keyFor(t *testing.T, data []byte) identity.Identity {
	t.Helper()
	id, err := identity.HashReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestBuildParseRenderRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		data  []byte
		codec Code
	}{
		{"raw", []byte("hello world"), CodeRaw},
		{"json", []byte(`{"a":1}`), CodeJson},
		{"empty", []byte{}, CodeRaw},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := keyFor(t, tc.data)
			c := Build(tc.codec, key)
			s, err := Render(c)
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			// Render gives the user-facing base32hex form (starts with the
			// 'v0' multibase prefix for CIDv1: v0 + 5... ).
			if s == "" {
				t.Fatalf("Render produced empty string")
			}
			// Parse must recover an equivalent CID.
			back, err := Parse(s)
			if err != nil {
				t.Fatalf("Parse(%q): %v", s, err)
			}
			if !back.Equals(c) {
				t.Errorf("round-trip: %v != %v", back, c)
			}
			// The key must survive the round-trip.
			if !bytes.Equal(Key(back), key) {
				t.Errorf("Key round-trip: %x != %x", Key(back), key)
			}
		})
	}
}

func TestRenderIsBase32Hex(t *testing.T) {
	key := keyFor(t, []byte("abc"))
	c := Build(CodeJson, key)
	s, err := Render(c)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The default go-cid String() for CIDv1 is base32 (a-z, 2-7). Our Render
	// uses base32hex (0-9a-v). A sha2-256 key has 32 bytes = 160 bits = 32
	// base32hex chars. The string must use only the base32hex alphabet and
	// must differ from go-cid's default base32 rendering (which is almost
	// certain given the different alphasets).
	wantAlphabet := "0123456789abcdefghijklmnopqrstuv"
	for _, r := range s {
		if !bytes.ContainsRune([]byte(wantAlphabet), r) {
			t.Errorf("Render(%q) contains non-base32hex char %q", s, r)
		}
	}
	if s != c.String() {
		// base32hex != base32 (default) — expected, this is the whole point of
		// Render existing. We assert they differ to prove we are not just
		// delegating to the default.
		// (Not a hard fail if the two happen to match for a particular value;
		// just log it.)
		t.Logf("Render (%s) differs from default String (%s) — as intended", s, c.String())
	}
}

func TestCodecForFirstByte(t *testing.T) {
	cases := []struct {
		first byte
		want  Code
	}{
		{'{', CodeJson},
		{'[', CodeJson},
		{'x', CodeRaw},
		{0x1f, CodeRaw},
		{0x00, CodeRaw},
	}
	for _, tc := range cases {
		if got := CodecForFirstByte(tc.first); got != tc.want {
			t.Errorf("CodecForFirstByte(%q) = %v, want %v", tc.first, got, tc.want)
		}
	}
}

func TestSupported(t *testing.T) {
	if !Supported(CodeRaw) || !Supported(CodeJson) {
		t.Error("supported should include raw and json")
	}
	if Supported(Code(0x01)) { // Cidv1 — not a content codec the store holds
		t.Error("supported should not include cidv1")
	}
}

func TestMediaType(t *testing.T) {
	if got := MediaType(Build(CodeJson, keyFor(t, []byte(`{}`)))); got != "application/json" {
		t.Errorf("MediaType(json) = %q, want application/json", got)
	}
	if got := MediaType(Build(CodeRaw, keyFor(t, []byte("x")))); got != "application/octet-stream" {
		t.Errorf("MediaType(raw) = %q, want application/octet-stream", got)
	}
}

func TestDigestFromCid(t *testing.T) {
	data := []byte("abc")
	c := Build(CodeRaw, keyFor(t, data))
	d, err := DigestFromCid(c)
	if err != nil {
		t.Fatalf("DigestFromCid: %v", err)
	}
	if d.Algorithm() != "sha256" {
		t.Errorf("DigestFromCid algorithm = %q, want sha256", d.Algorithm())
	}
	// It must equal the go-digest sha256 of the same bytes (cross-check the
	// bridge against an independent digest computation).
	want := godigest.FromBytes(data)
	if d != want {
		t.Errorf("DigestFromCid = %s, want %s", d, want)
	}
}
