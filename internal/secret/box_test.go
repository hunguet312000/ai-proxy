package secret

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestBoxRoundTrip(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{1}, KeySize))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	plaintext := []byte(`{"access_token":"secret"}`)
	first, err := box.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt() error = %v", err)
	}
	second, err := box.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt() second error = %v", err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("Encrypt() reused a nonce")
	}

	got, err := box.Decrypt(first)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("Decrypt() = %q, want %q", got, plaintext)
	}
}

func TestBoxRejectsInvalidInput(t *testing.T) {
	if _, err := New(make([]byte, KeySize-1)); err == nil {
		t.Fatal("New() accepted an invalid key")
	}

	box, err := New(make([]byte, KeySize))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := box.Decrypt([]byte("short")); err == nil {
		t.Fatal("Decrypt() accepted a short blob")
	}
}

func TestDecodeKey(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(make([]byte, KeySize))
	if _, err := DecodeKey(encoded); err != nil {
		t.Fatalf("DecodeKey() error = %v", err)
	}
	if _, err := DecodeKey("not-base64"); err == nil {
		t.Fatal("DecodeKey() accepted invalid base64")
	}
}
