package ownerauth

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/argon2"
)

func TestHashPasswordUsesSaltedArgon2idPHC(t *testing.T) {
	password := []byte("  owner passphrase  ")
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=65536,t=3,p=4" {
		t.Fatal("hash must encode the Argon2id version and RFC 9106 parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		t.Fatal("hash must contain a 128-bit salt")
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) != 32 {
		t.Fatal("hash must contain a 256-bit tag")
	}
	if want := argon2.IDKey(password, salt, 3, 65536, 4, 32); !bytes.Equal(key, want) {
		t.Fatal("hash does not verify the exact password including whitespace")
	}
	if wrong := argon2.IDKey(bytes.TrimSpace(password), salt, 3, 65536, 4, 32); bytes.Equal(key, wrong) {
		t.Fatal("whitespace was removed from the password")
	}
	other, err := HashPassword(password)
	if err != nil || other == hash {
		t.Fatal("hashing the same password again must generate a fresh salt")
	}
}

func TestHashPasswordMinimumLengthCountsUnicodeCharacters(t *testing.T) {
	for _, password := range [][]byte{nil, []byte(strings.Repeat("a", 14)), []byte(strings.Repeat("あ", 14)), append(bytes.Repeat([]byte("a"), 15), 0xff)} {
		if hash, err := HashPassword(password); err == nil || hash != "" {
			t.Fatal("short or invalid UTF-8 password was accepted")
		}
	}
	for _, password := range []string{strings.Repeat("a", 15), strings.Repeat("あ", 15)} {
		if _, err := HashPassword([]byte(password)); err != nil {
			t.Fatalf("15-character password rejected: %v", err)
		}
	}
}
