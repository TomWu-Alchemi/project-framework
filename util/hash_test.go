package util

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestBcryptHash_RoundTrip(t *testing.T) {
	const pw = "s3cret"
	hash, err := BcryptHash(pw)
	if err != nil {
		t.Fatal(err)
	}
	if !BcryptCheck(pw, hash) {
		t.Fatal("BcryptCheck failed for original password")
	}
	if BcryptCheck("other", hash) {
		t.Fatal("BcryptCheck matched wrong password")
	}
}

func TestBcryptHash_TooLong(t *testing.T) {
	_, err := BcryptHash(strings.Repeat("a", 73))
	if err == nil {
		t.Fatal("expected error for overly long password")
	}
	if !errors.Is(err, bcrypt.ErrPasswordTooLong) {
		t.Fatalf("got %v, want ErrPasswordTooLong", err)
	}
}
