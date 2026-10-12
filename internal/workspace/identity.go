// Package workspace implements the host-side lifecycle core. Admission must
// authenticate metadata before constructing an Identity; no network ingress is
// supplied by this package.
package workspace

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
)

type WorkspaceKey string
type Generation string

// Exact is the only runtime/destructive authority. Neither component alone is
// sufficient. Values are validated at every runtime boundary.
type Exact struct {
	Workspace  WorkspaceKey `json:"workspace"`
	Generation Generation   `json:"generation"`
}

// Identity contains only a derived key, never the raw subject/session.
type Identity struct{ key WorkspaceKey }

// IdentityFromTrustedMeta accepts metadata attested by the host's trusted
// ingress, NOT arbitrary caller arguments or unverified wire _meta. Future
// transports must establish that trust before calling this function.
func IdentityFromTrustedMeta(meta map[string]any) (Identity, error) {
	subject, okSubject := meta["openai/subject"].(string)
	session, okSession := meta["openai/session"].(string)
	if !okSubject || !okSession || subject == "" || session == "" {
		return Identity{}, errors.New("required trusted OpenAI identity is missing")
	}
	h := sha256.New()
	h.Write([]byte("mcp-workspaced/identity/v1\x00"))
	for _, value := range []string{subject, session} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		h.Write(length[:])
		h.Write([]byte(value))
	}
	return Identity{WorkspaceKey(hex.EncodeToString(h.Sum(nil)))}, nil
}

func (i Identity) Workspace() WorkspaceKey { return i.key }

func freshGeneration() (Generation, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return Generation(hex.EncodeToString(bytes[:])), nil
}

func validHex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (e Exact) validate() error {
	if !validHex(string(e.Workspace)) || !validHex(string(e.Generation)) {
		return errors.New("invalid exact workspace generation")
	}
	return nil
}
