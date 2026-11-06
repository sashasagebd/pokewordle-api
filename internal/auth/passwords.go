package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// An InvalidError is a username or password that breaks the rules. Its
// message is meant for the player.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

var validUsername = regexp.MustCompile(`^[A-Za-z0-9_-]{3,20}$`)

// CheckUsername checks a username: 3 to 20 letters, digits, _ or -. Names are
// shown as typed but unique ignoring case.
func CheckUsername(name string) error {
	if !validUsername.MatchString(name) {
		return &InvalidError{"A name is 3 to 20 letters, digits, _ or -."}
	}
	return nil
}

// CheckPassword checks a new password's length: 8 to 128 characters.
func CheckPassword(password string) error {
	if n := utf8.RuneCountInString(password); n < 8 || n > 128 {
		return &InvalidError{"A password is 8 to 128 characters."}
	}
	return nil
}

// argonParams are argon2id's settings. A stored hash records its own, so
// these can change: a login with an older hash rehashes it.
type argonParams struct {
	memory  uint32 // KiB
	time    uint32
	threads uint8
}

// currentParams are OWASP's minimum for argon2id: 19 MiB, 2 passes, 1 lane.
var currentParams = argonParams{memory: 19 * 1024, time: 2, threads: 1}

const (
	saltLen = 16
	keyLen  = 32
)

// hashSlots limits hashing to 2 at a time: each takes 19 MiB, and the
// container has 256 MiB and half a core, so a burst of logins must queue
// rather than run it out of memory.
var hashSlots = make(chan struct{}, 2)

func argonKey(password string, salt []byte, p argonParams, n uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey([]byte(password), salt, p.time, p.memory, p.threads, n)
}

// hashPassword returns password's argon2id hash in the usual encoded form,
// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<key>.
func hashPassword(password string) string {
	salt := make([]byte, saltLen)
	rand.Read(salt)
	return encodeHash(currentParams, salt, argonKey(password, salt, currentParams, keyLen))
}

func encodeHash(p argonParams, salt, key []byte) string {
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.time, p.threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

func decodeHash(encoded string) (p argonParams, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, errors.New("not an argon2id hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return p, nil, nil, fmt.Errorf("unsupported argon2 version %q", parts[2])
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return p, nil, nil, fmt.Errorf("bad argon2 parameters %q", parts[3])
	}
	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(parts[4]); err != nil {
		return p, nil, nil, err
	}
	if key, err = b64.DecodeString(parts[5]); err != nil {
		return p, nil, nil, err
	}
	return p, salt, key, nil
}

// verifyPassword reports whether password matches encoded, and whether the
// hash should be redone with the current settings.
func verifyPassword(password, encoded string) (ok, rehash bool, err error) {
	p, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, false, err
	}
	got := argonKey(password, salt, p, uint32(len(key)))
	ok = subtle.ConstantTimeCompare(got, key) == 1
	return ok, ok && p != currentParams, nil
}

// dummyHash is checked against when a login names no one, so a wrong name
// takes as long as a wrong password and can't be told apart by timing.
var dummyHash = hashPassword("not anyone's password")
