package password

import (
	"strings"
	"testing"
)

func TestArgon2idHashAndVerification(t *testing.T) {
	value := []byte("a test passphrase with spaces ")
	first, err := Hash(value)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Hash(value)
	if err != nil || first == second || !strings.HasPrefix(first, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatal("password hashes must use independently random salts")
	}
	if Validate(first) != nil || !Verify(first, value) || Verify(first, []byte("incorrect password")) || Verify(first, []byte(strings.TrimSpace(string(value)))) {
		t.Fatal("password verification changed the password or accepted a mismatch")
	}
	for _, length := range []int{0, 11, MaxBytes + 1} {
		if _, err := Hash([]byte(strings.Repeat("a", length))); err == nil {
			t.Fatal("invalid password length accepted")
		}
	}
	for _, hash := range []string{"plain-password", "", strings.Replace(first, "argon2id", "argon2i", 1), strings.Replace(first, "v=19", "v=18", 1), strings.Replace(first, "m=19456", "m=1", 1), strings.Replace(first, "m=19456", "m=99999999", 1), strings.Replace(first, "t=2", "t=999999", 1), strings.Replace(first, "p=1", "p=256", 1), first + "$extra", first[:len(first)-1]} {
		if Validate(hash) == nil || Verify(hash, value) {
			t.Fatalf("invalid or unsafe hash accepted: %q", hash)
		}
	}
}
