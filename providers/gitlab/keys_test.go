package gitlab

import (
	"crypto/ed25519"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// crc24 is the OpenPGP armor checksum (RFC 4880 §6.1).
func crc24(b []byte) uint32 {
	crc := uint32(0xB704CE)
	for _, c := range b {
		crc ^= uint32(c) << 16
		for i := 0; i < 8; i++ {
			crc <<= 1
			if crc&0x1000000 != 0 {
				crc ^= 0x1864CFB
			}
		}
	}
	return crc & 0xFFFFFF
}

func armor(data []byte) string {
	var b strings.Builder
	b.WriteString("-----BEGIN PGP PUBLIC KEY BLOCK-----\nComment: test\n\n")
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 64 {
		b.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	b.WriteString(enc + "\n")
	c := crc24(data)
	b.WriteString("=" + base64.StdEncoding.EncodeToString([]byte{byte(c >> 16), byte(c >> 8), byte(c)}) + "\n")
	b.WriteString("-----END PGP PUBLIC KEY BLOCK-----\n")
	return b.String()
}

func newPacket(tag byte, body []byte) []byte {
	if len(body) < 192 {
		return append([]byte{0xC0 | tag, byte(len(body))}, body...)
	}
	return append([]byte{0xC0 | tag, 0xFF, byte(len(body) >> 24), byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
}

func oldPacket(tag byte, body []byte) []byte {
	return append([]byte{0x80 | tag<<2 | 1, byte(len(body) >> 8), byte(len(body))}, body...)
}

// v4EdDSAKey builds a v4 EdDSA public-key packet body (RFC 4880bis).
func v4EdDSAKey(seed byte) []byte {
	pub := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	pub[0] ^= seed
	body := []byte{4, 0x65, 0x00, 0x00, 0x01, 22}
	oid := []byte{0x2B, 0x06, 0x01, 0x04, 0x01, 0xDA, 0x47, 0x0F, 0x01}
	body = append(body, byte(len(oid)))
	body = append(body, oid...)
	point := append([]byte{0x40}, pub...)
	body = binary.BigEndian.AppendUint16(body, uint16(len(point)*8-1))
	return append(body, point...)
}

func v4KeyID(body []byte) string {
	h := sha1.New()
	h.Write([]byte{0x99, byte(len(body) >> 8), byte(len(body))})
	h.Write(body)
	return strings.ToUpper(hex.EncodeToString(h.Sum(nil)[12:]))
}

func TestParseArmoredPublicKey(t *testing.T) {
	key := v4EdDSAKey(0)
	data := newPacket(6, key)
	data = append(data, newPacket(13, []byte("Alice Example <alice@example.com>"))...)
	data = append(data, newPacket(2, []byte{4, 0x13})...) // signature: ignored
	data = append(data, newPacket(13, []byte("alice@work.example"))...)
	sub := newPacket(14, v4EdDSAKey(1)) // subkey: must not replace the primary ID
	data = append(data, sub...)
	info := parseArmoredPublicKey(armor(data))
	if info.keyID != v4KeyID(key) || len(info.keyID) != 16 {
		t.Fatalf("keyID = %q, want %q", info.keyID, v4KeyID(key))
	}
	if !reflect.DeepEqual(info.emails, []string{"alice@example.com", "alice@work.example"}) {
		t.Fatalf("emails = %v", info.emails)
	}

	// Old-format packet headers (as produced by older GnuPG versions).
	old := append(oldPacket(6, key), oldPacket(13, []byte("Bob <bob@example.com>"))...)
	info = parseArmoredPublicKey(armor(old))
	if info.keyID != v4KeyID(key) || !reflect.DeepEqual(info.emails, []string{"bob@example.com"}) {
		t.Fatalf("old format: %+v", info)
	}

	// v6 key IDs are the first 8 bytes of the SHA-256 fingerprint.
	v6 := append([]byte{6}, key[1:]...)
	h := sha256.New()
	h.Write([]byte{0x9b, 0, 0, 0, byte(len(v6))})
	h.Write(v6)
	if got := pgpKeyID(v6); got != strings.ToUpper(hex.EncodeToString(h.Sum(nil)[:8])) {
		t.Fatalf("v6 key ID = %q", got)
	}

	for _, bad := range []string{"", "not a key", "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\n!!!\n-----END PGP PUBLIC KEY BLOCK-----"} {
		if info := parseArmoredPublicKey(bad); info.keyID != "" || info.emails != nil {
			t.Fatalf("garbage %q parsed as %+v", bad, info)
		}
	}
	// Truncated packet.
	if info := parseArmoredPublicKey(armor(newPacket(6, key)[:10])); info.keyID != "" {
		t.Fatalf("truncated packet parsed: %+v", info)
	}
}

func TestListGPGKeys(t *testing.T) {
	f := newFake(t)
	p := f.newProvider(t)
	key := v4EdDSAKey(0)
	armored := armor(append(newPacket(6, key), newPacket(13, []byte("Alice <alice@example.com>"))...))
	f.json("GET /api/v4/user/gpg_keys", 200, []any{
		map[string]any{"id": 1, "key": armored, "created_at": "2025-01-01T00:00:00Z"},
		map[string]any{"id": 2, "key": "garbage"},
	})
	keys, err := p.ListGPGKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].KeyID != v4KeyID(key) || keys[0].Emails[0] != "alice@example.com" || keys[0].CreatedAt.IsZero() ||
		keys[1].ID != "2" || keys[1].KeyID != "" {
		t.Fatalf("gpg keys = %+v", keys)
	}
	f.handle("GET /api/v4/user/gpg_keys", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, map[string]any{"message": "403 Forbidden"})
	})
	if _, err := p.ListGPGKeys(ctx); err == nil {
		t.Fatal("403 not reported")
	}
}

func TestSSHFingerprintInvalid(t *testing.T) {
	for _, k := range []string{"", "ssh-ed25519", "ssh-ed25519 !!!"} {
		if fp := sshFingerprint(k); fp != "" {
			t.Fatalf("%q -> %q", k, fp)
		}
	}
}

func TestErrorMessageShapes(t *testing.T) {
	cases := map[string]string{
		`{"message":"404 Project Not Found"}`:                           "404 Project Not Found",
		`{"message":["a","b"]}`:                                         "a; b",
		`{"message":{"base":["x"],"name":["y","z"]}}`:                   "base x; name y, z",
		`{"message":{"nested":{"field":["bad"]}}}`:                      "nested.field bad",
		`{"error":"invalid_token","error_description":"Token expired"}`: "Token expired",
		`{"error":"scope does not have a valid value"}`:                 "scope does not have a valid value",
		`not json`: "",
		`{"message":"leak glpat-ABCDEFGHIJKLMNOPQRSTUV"}`: "leak [REDACTED]",
	}
	for body, want := range cases {
		if got, _ := errorMessage([]byte(body)); got != want {
			t.Errorf("errorMessage(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestSnippetRef(t *testing.T) {
	cases := []struct{ raw, path, want string }{
		{"https://gl/-/snippets/1/raw/main/a.rb", "a.rb", "main"},
		{"https://gl/-/snippets/1/raw/master/dir/a.rb", "dir/a.rb", "master"},
		{"https://gl/-/snippets/1/raw", "a.rb", "HEAD"},
		{"https://gl/-/snippets/1/raw/main/other", "a.rb", "HEAD"},
	}
	for _, c := range cases {
		if got := snippetRef(c.raw, c.path); got != c.want {
			t.Errorf("snippetRef(%q, %q) = %q, want %q", c.raw, c.path, got, c.want)
		}
	}
}
