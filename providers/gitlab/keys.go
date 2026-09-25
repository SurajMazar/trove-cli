package gitlab

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/SurajMazar/trove-cli/internal/domain"
)

type glSSHKey struct {
	ID        int64      `json:"id"`
	Title     string     `json:"title"`
	Key       string     `json:"key"`
	CreatedAt *time.Time `json:"created_at"`
}

func (k glSSHKey) toDomain() domain.SSHKey {
	out := domain.SSHKey{ID: strconv.FormatInt(k.ID, 10), Title: k.Title, Key: k.Key, Fingerprint: sshFingerprint(k.Key)}
	setTime(&out.CreatedAt, k.CreatedAt)
	return out
}

// sshFingerprint computes the OpenSSH SHA256 fingerprint of an
// authorized_keys line ("type base64 [comment]"), which GitLab's key API does
// not return. It returns "" if the key cannot be decoded.
func sshFingerprint(key string) string {
	fields := strings.Fields(key)
	if len(fields) < 2 {
		return ""
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(blob) == 0 {
		return ""
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

func (p *provider) ListSSHKeys(ctx context.Context) ([]domain.SSHKey, error) {
	keys, err := list[glSSHKey](ctx, p, "list SSH keys", "/user/keys", nil, 0, nil)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SSHKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.toDomain())
	}
	return out, nil
}

func (p *provider) AddSSHKey(ctx context.Context, title, key string) (*domain.SSHKey, error) {
	const op = "add SSH key"
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, p.invalid(op, "a public key is required")
	}
	if strings.Contains(key, "PRIVATE KEY") {
		// Never send private key material anywhere.
		return nil, p.invalid(op, "this looks like a private key; add the public key (the .pub file) instead")
	}
	body := map[string]any{"key": key}
	if title != "" {
		body["title"] = title
	} else if f := strings.Fields(key); len(f) >= 3 {
		body["title"] = strings.Join(f[2:], " ")
	} else {
		return nil, p.invalid(op, "a title is required for keys without a comment")
	}
	var k glSSHKey
	if _, err := p.do(ctx, call{method: http.MethodPost, path: "/user/keys", op: op, body: body}, &k); err != nil {
		return nil, err
	}
	d := k.toDomain()
	return &d, nil
}

func (p *provider) RemoveSSHKey(ctx context.Context, id string) error {
	op := "remove SSH key " + id
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return p.invalid(op, "SSH key ID %q is not numeric", id)
	}
	_, err := p.do(ctx, call{method: http.MethodDelete, path: "/user/keys/" + id, op: op}, nil)
	return err
}

// --- GPG ------------------------------------------------------------------------

type glGPGKey struct {
	ID        int64      `json:"id"`
	Key       string     `json:"key"` // ASCII-armored public key
	CreatedAt *time.Time `json:"created_at"`
}

// ListGPGKeys lists GPG keys. GitLab returns only the armored key, so the key
// ID and user-ID emails are derived by parsing the OpenPGP packets.
func (p *provider) ListGPGKeys(ctx context.Context) ([]domain.GPGKey, error) {
	keys, err := list[glGPGKey](ctx, p, "list GPG keys", "/user/gpg_keys", nil, 0, nil)
	if err != nil {
		return nil, err
	}
	out := make([]domain.GPGKey, 0, len(keys))
	for _, k := range keys {
		info := parseArmoredPublicKey(k.Key)
		g := domain.GPGKey{ID: strconv.FormatInt(k.ID, 10), KeyID: info.keyID, Emails: info.emails}
		setTime(&g.CreatedAt, k.CreatedAt)
		out = append(out, g)
	}
	return out, nil
}

type pgpKeyInfo struct {
	keyID  string // 16 upper-case hex digits, "" if unknown
	emails []string
}

// parseArmoredPublicKey extracts the primary key ID (RFC 4880 §12.2 for v4
// keys, RFC 9580 §5.5.4 for v6) and user-ID emails. Unparseable input yields
// an empty result rather than an error.
func parseArmoredPublicKey(armored string) pgpKeyInfo {
	data := dearmor(armored)
	var info pgpKeyInfo
	for len(data) > 0 {
		tag, body, rest, ok := nextPacket(data)
		if !ok {
			break
		}
		data = rest
		switch tag {
		case 6: // public key
			if info.keyID == "" {
				info.keyID = pgpKeyID(body)
			}
		case 13: // user ID, conventionally "Name (comment) <email>"
			uid := string(body)
			if i, j := strings.LastIndex(uid, "<"), strings.LastIndex(uid, ">"); i >= 0 && j > i+1 {
				info.emails = append(info.emails, uid[i+1:j])
			} else if strings.Contains(uid, "@") && !strings.ContainsAny(uid, " \t") {
				info.emails = append(info.emails, uid)
			}
		}
	}
	return info
}

func dearmor(s string) []byte {
	const begin = "-----BEGIN PGP PUBLIC KEY BLOCK-----"
	i := strings.Index(s, begin)
	if i < 0 {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(s[i+len(begin):], "\r\n", "\n"), "\n")
	var b64 strings.Builder
	inBody := false
loop:
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "-----END"):
			break loop
		case !inBody:
			// Armor headers ("Version: ...") end at the first blank line.
			if line == "" {
				inBody = true
			} else if !strings.Contains(line, ": ") {
				// No headers and no blank separator: this is already data.
				inBody = true
				b64.WriteString(line)
			}
		case strings.HasPrefix(line, "="): // CRC24 checksum line
			break loop
		default:
			b64.WriteString(line)
		}
	}
	out, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		return nil
	}
	return out
}

// nextPacket splits the first OpenPGP packet off data (old or new format).
func nextPacket(data []byte) (tag byte, body, rest []byte, ok bool) {
	if len(data) < 2 || data[0]&0x80 == 0 {
		return 0, nil, nil, false
	}
	hdr := data[0]
	var n, off int
	if hdr&0x40 != 0 { // new format
		tag = hdr & 0x3f
		o1 := int(data[1])
		switch {
		case o1 < 192:
			n, off = o1, 2
		case o1 < 224:
			if len(data) < 3 {
				return 0, nil, nil, false
			}
			n, off = (o1-192)<<8+int(data[2])+192, 3
		case o1 == 255:
			if len(data) < 6 {
				return 0, nil, nil, false
			}
			n, off = int(binary.BigEndian.Uint32(data[2:6])), 6
		default: // partial body lengths never occur in key packets
			return 0, nil, nil, false
		}
	} else { // old format
		tag = (hdr >> 2) & 0x0f
		switch hdr & 3 {
		case 0:
			n, off = int(data[1]), 2
		case 1:
			if len(data) < 3 {
				return 0, nil, nil, false
			}
			n, off = int(binary.BigEndian.Uint16(data[1:3])), 3
		case 2:
			if len(data) < 5 {
				return 0, nil, nil, false
			}
			n, off = int(binary.BigEndian.Uint32(data[1:5])), 5
		default:
			n, off = len(data)-1, 1
		}
	}
	if n < 0 || off+n > len(data) {
		return 0, nil, nil, false
	}
	return tag, data[off : off+n], data[off+n:], true
}

// pgpKeyID computes the key ID of a public-key packet body.
func pgpKeyID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	switch body[0] {
	case 4:
		if len(body) > 0xffff {
			return ""
		}
		h := sha1.New()
		h.Write([]byte{0x99, byte(len(body) >> 8), byte(len(body))})
		h.Write(body)
		fp := h.Sum(nil)
		return strings.ToUpper(hex.EncodeToString(fp[12:20]))
	case 5, 6:
		prefix := byte(0x9a)
		if body[0] == 6 {
			prefix = 0x9b
		}
		h := sha256.New()
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(body)))
		h.Write([]byte{prefix})
		h.Write(l[:])
		h.Write(body)
		fp := h.Sum(nil)
		return strings.ToUpper(hex.EncodeToString(fp[:8]))
	}
	return ""
}
