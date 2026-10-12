package workspace

import (
	"strings"
	"testing"
)

func identity(t *testing.T, subject, session string) Identity {
	t.Helper()
	i, err := IdentityFromTrustedMeta(map[string]any{"openai/subject": subject, "openai/session": session})
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestTrustedIdentity(t *testing.T) {
	i := identity(t, "subject", "session")
	if i != identity(t, "subject", "session") {
		t.Fatal("unstable logical key")
	}
	for _, other := range []Identity{identity(t, "other", "session"), identity(t, "subject", "other"),
		identity(t, "ab", "c"), identity(t, "a", "bc")} {
		if i == other {
			t.Fatal("identity collision")
		}
	}
	if identity(t, "ab", "c") == identity(t, "a", "bc") {
		t.Fatal("ambiguous identity encoding")
	}
	withExtras, err := IdentityFromTrustedMeta(map[string]any{
		"openai/subject": "subject", "openai/session": "session",
		"workspace": "spoof", "user": "spoof", "session": "spoof", "Mcp-Session-Id": "spoof",
	})
	if err != nil || withExtras != i {
		t.Fatal("caller IDs influenced identity")
	}
	for _, meta := range []map[string]any{nil, {}, {"openai/subject": "s"}, {"openai/session": "s"},
		{"openai/subject": "", "openai/session": "s"}, {"openai/subject": "s", "openai/session": ""},
		{"openai/subject": 12, "openai/session": "s"}, {"user": "s", "session": "s"},
		{"Mcp-Session-Id": "s"}} {
		if _, err := IdentityFromTrustedMeta(meta); err == nil {
			t.Fatal("missing identity accepted")
		}
	}
	i = identity(t, "raw-subject/unsafe", "raw-session/unsafe")
	g, err := freshGeneration()
	if err != nil {
		t.Fatal(err)
	}
	name := objectName(Exact{i.Workspace(), g})
	if strings.Contains(name, "raw-") || !validHex(string(i.Workspace())) {
		t.Fatal("raw identity in name")
	}
}

func TestFreshGeneration(t *testing.T) {
	seen := make(map[Generation]bool)
	for range 100 {
		g, err := freshGeneration()
		if err != nil || !validHex(string(g)) || seen[g] {
			t.Fatalf("generation invalid or reused: %v", err)
		}
		seen[g] = true
	}
}
