package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestPasswordHashing(t *testing.T) {
	h := hashPassword("correct horse")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash %q isn't in the expected format", h)
	}
	if h == hashPassword("correct horse") {
		t.Error("two hashes of one password match: the salt isn't random")
	}
	if ok, rehash, err := verifyPassword("correct horse", h); !ok || rehash || err != nil {
		t.Errorf("right password: ok %v, rehash %v, err %v", ok, rehash, err)
	}
	if ok, _, err := verifyPassword("correct horsE", h); ok || err != nil {
		t.Errorf("wrong password: ok %v, err %v", ok, err)
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$c2FsdA$a2V5", "$argon2id$v=18$m=1,t=1,p=1$c2FsdA$a2V5"} {
		if _, _, err := verifyPassword("x", bad); err == nil {
			t.Errorf("verifyPassword accepted the hash %q", bad)
		}
	}
}

func TestOldSettingsAskForARehash(t *testing.T) {
	old := argonParams{memory: 8 * 1024, time: 1, threads: 1}
	salt := []byte("0123456789abcdef")
	h := encodeHash(old, salt, argonKey("hunter22", salt, old, keyLen))
	ok, rehash, err := verifyPassword("hunter22", h)
	if !ok || !rehash || err != nil {
		t.Errorf("old settings: ok %v, rehash %v, err %v; want a match that asks for a rehash", ok, rehash, err)
	}
	if ok, rehash, _ := verifyPassword("hunter23", h); ok || rehash {
		t.Error("a wrong password must never ask for a rehash")
	}
}

func TestUsernameAndPasswordRules(t *testing.T) {
	for name, want := range map[string]bool{
		"ash": true, "Ash_Ketchum-99": true, "abcdefghijklmnopqrst": true,
		"ab": false, "abcdefghijklmnopqrstu": false, "ash ketchum": false, "pokémon": false, "": false,
	} {
		err := CheckUsername(name)
		var invalid *InvalidError
		if (err == nil) != want || (err != nil && !errors.As(err, &invalid)) {
			t.Errorf("CheckUsername(%q) = %v", name, err)
		}
	}
	for pw, want := range map[string]bool{
		"12345678": true, "ééééééée": true, strings.Repeat("x", 128): true,
		"1234567": false, strings.Repeat("x", 129): false,
	} {
		if err := CheckPassword(pw); (err == nil) != want {
			t.Errorf("CheckPassword(%.20q) = %v", pw, err)
		}
	}
}
