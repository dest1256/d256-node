package vault

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTripAndBinding(t *testing.T) {
	s, err := FromSecret([]byte("node private key bytes"), "test")
	if err != nil {
		t.Fatal(err)
	}
	blob := s.Seal([]byte("secret payload"), "journal/batch")
	if bytes.Contains(blob, []byte("secret payload")) {
		t.Fatal("plaintext visible in the sealed blob")
	}
	got, err := s.Open(blob, "journal/batch")
	if err != nil || string(got) != "secret payload" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := s.Open(blob, "cache/state"); err == nil {
		t.Fatal("a blob must not open under another purpose")
	}
	blob[len(blob)-1] ^= 1
	if _, err := s.Open(blob, "journal/batch"); err == nil {
		t.Fatal("tampering must be detected")
	}
	other, _ := FromSecret([]byte("another key"), "test")
	if _, err := other.Open(s.Seal([]byte("x"), "p"), "p"); err == nil {
		t.Fatal("wrong key must not open the blob")
	}
	if _, err := s.Open([]byte{1, 2, 3}, "p"); err == nil {
		t.Fatal("short blob must be rejected")
	}
	if a, b := s.Seal([]byte("same"), "p"), s.Seal([]byte("same"), "p"); bytes.Equal(a, b) {
		t.Fatal("nonces must differ between seals")
	}
}

func TestFromSecretRejectsEmpty(t *testing.T) {
	if _, err := FromSecret(nil, "x"); err == nil {
		t.Fatal("empty secret accepted")
	}
}
