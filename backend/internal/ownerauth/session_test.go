package ownerauth

import (
	"strings"
	"testing"
)

func TestVerifyPasswordMatchesOnlyTheSetPassword(t *testing.T) {
	hash, err := HashPassword([]byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	for password, want := range map[string]bool{
		"correct horse battery staple":  true,
		"correct horse battery staple ": false,
		"Correct horse battery staple":  false,
		"":                              false,
	} {
		got, err := VerifyPassword([]byte(password), hash)
		if err != nil || got != want {
			t.Fatalf("VerifyPassword(%q) = %v, %v; want %v", password, got, err, want)
		}
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	hash, err := HashPassword([]byte("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"", "plain", strings.Replace(hash, "argon2id", "argon2i", 1), strings.Replace(hash, "v=19", "v=16", 1),
		strings.Replace(hash, "m=65536", "m=0", 1), hash[:strings.LastIndex(hash, "$")] + "$!!!", hash + "$extra",
	} {
		if ok, err := VerifyPassword([]byte("correct horse battery staple"), bad); ok || err == nil {
			t.Fatalf("VerifyPassword accepted malformed hash %q: ok=%v err=%v", bad, ok, err)
		}
	}
}

func TestSessionTokensAreRandomCanonicalAndHaveDistinctCSRF(t *testing.T) {
	a, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b || !CanonicalSessionToken(a) || !CanonicalSessionToken(b) {
		t.Fatalf("tokens %q %q", a, b)
	}
	for _, bad := range []string{"", "short", a + "=", a + "A", strings.ToUpper(a[:1]) + "%" + a[2:]} {
		if CanonicalSessionToken(bad) {
			t.Fatalf("accepted non-canonical token %q", bad)
		}
	}
	if CSRFToken(a) != CSRFToken(a) || CSRFToken(a) == CSRFToken(b) || CSRFToken(a) == a {
		t.Fatal("CSRF token must be stable per session and differ between sessions")
	}
	if TokenHash(a) == a || len(TokenHash(a)) != 64 {
		t.Fatalf("token hash %q", TokenHash(a))
	}
}
