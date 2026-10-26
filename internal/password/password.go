package password

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

const MaxBytes = 1024

type parameters struct {
	memory, iterations uint32
	parallelism        uint8
	salt, key          []byte
}

// Hash uses the OWASP Argon2id baseline: 19 MiB, two iterations, one lane.
func Hash(value []byte) (string, error) {
	if len(value) < 12 || len(value) > MaxBytes {
		return "", errors.New("password must contain between 12 and 1024 bytes")
	}
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	key := argon2.IDKey(value, salt[:], 2, 19456, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt[:]), base64.RawStdEncoding.EncodeToString(key)), nil
}

func Validate(encoded string) error {
	_, err := parse(encoded)
	return err
}

func Verify(encoded string, value []byte) bool {
	p, err := parse(encoded)
	if err != nil || len(value) == 0 || len(value) > MaxBytes {
		return false
	}
	key := argon2.IDKey(value, p.salt, p.iterations, p.memory, p.parallelism, uint32(len(p.key)))
	return subtle.ConstantTimeCompare(key, p.key) == 1
}

func parse(encoded string) (parameters, error) {
	invalid := errors.New("unsupported password hash; use password-hash to generate Argon2id")
	var p parameters
	parts := strings.Split(encoded, "$")
	if len(encoded) > 512 || len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return p, invalid
	}
	settings := strings.Split(parts[3], ",")
	if len(settings) != 3 {
		return p, invalid
	}
	values := make([]uint64, 3)
	for i, name := range []string{"m=", "t=", "p="} {
		if !strings.HasPrefix(settings[i], name) {
			return p, invalid
		}
		value, err := strconv.ParseUint(strings.TrimPrefix(settings[i], name), 10, 32)
		if err != nil {
			return p, invalid
		}
		values[i] = value
	}
	// Bound resource use even when an administrator imports an external PHC hash.
	if values[0] < 19456 || values[0] > 65536 || values[1] < 2 || values[1] > 4 || values[2] < 1 || values[2] > 4 {
		return p, invalid
	}
	p.memory, p.iterations, p.parallelism = uint32(values[0]), uint32(values[1]), uint8(values[2])
	var err error
	p.salt, err = base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(p.salt) < 16 || len(p.salt) > 64 {
		return p, invalid
	}
	p.key, err = base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(p.key) != 32 {
		return p, invalid
	}
	return p, nil
}
