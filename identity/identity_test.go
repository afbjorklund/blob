package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	godigest "github.com/opencontainers/go-digest"
)

// known vector for sha256 of "abc" (NIST) so a regression is obvious.
var abc = []byte("abc")

func sha256Hex(t *testing.T, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestHashReaderKnownVector(t *testing.T) {
	id, err := HashReader(bytes.NewReader(abc))
	if err != nil {
		t.Fatalf("HashReader: %v", err)
	}
	// 0x12 (sha2-256) 0x20 (32) + 32-byte digest
	raw, err := hex.DecodeString(sha256Hex(t, abc))
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte{0x12, 0x20}, raw...)
	if !bytes.Equal(id, want) {
		t.Errorf("HashReader(abc) = %x, want %x", id, want)
	}
}

func TestFromOCI_RoundTrip(t *testing.T) {
	cases := []string{"abc", "hello", string(make([]byte, 1024))}
	for _, payload := range cases {
		d := godigest.FromBytes([]byte(payload))
		got, err := FromOCI(d)
		if err != nil {
			t.Fatalf("FromOCI(%s): %v", d, err)
		}
		back, err := ToOCI(got)
		if err != nil {
			t.Fatalf("ToOCI(%q): %v", got, err)
		}
		if back != d {
			t.Errorf("round-trip %q: %s != %s", payload, back, d)
		}
	}
}

func TestDigestBytes(t *testing.T) {
	d := godigest.FromBytes(abc)
	id, err := FromOCI(d)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := DigestBytes(id)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(inner) != sha256Hex(t, abc) {
		t.Errorf("DigestBytes = %x, want %s", inner, sha256Hex(t, abc))
	}
}

func TestCheck(t *testing.T) {
	id, err := HashReader(bytes.NewReader(abc))
	if err != nil {
		t.Fatal(err)
	}
	if !Check(bytes.NewReader(abc), id) {
		t.Errorf("Check(abc, hash(abc)) = false, want true")
	}
	if Check(bytes.NewReader([]byte("abd")), id) {
		t.Errorf("Check(abd, hash(abc)) = true, want false")
	}
}

func TestUnsupportedAlgorithm(t *testing.T) {
	// Build a sha384 digest by hand; confirm the algorithm gate rejects it.
	raw, _ := hex.DecodeString("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff00112233")
	d384 := godigest.NewDigestFromBytes(godigest.SHA384, raw)
	if _, err := FromOCI(d384); err == nil {
		t.Errorf("FromOCI(sha384) = nil, want an error (ErrUnsupported)")
	}
}
