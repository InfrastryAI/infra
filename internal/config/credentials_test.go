package config

import (
	"errors"
	"strings"
	"testing"
)

func TestMemoryCredentialStoreRoundTrip(t *testing.T) {
	store := &MemoryCredentialStore{}
	want := Credentials{AccessToken: "access", RefreshToken: "refresh"}
	if err := store.Set("https://example.test", want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get("https://example.test")
	if err != nil || got != want {
		t.Fatalf("Get() = %#v, %v", got, err)
	}
	if err := store.Delete("https://example.test"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("https://example.test"); !errors.Is(err, ErrCredentialsNotFound) {
		t.Fatalf("Get() after Delete() error = %v", err)
	}
}

func TestCredentialAccountIsStableAndOpaque(t *testing.T) {
	first := credentialAccount("https://example.test")
	if first != credentialAccount("https://example.test") || first == credentialAccount("https://other.test") {
		t.Fatalf("credential accounts are not stable and distinct")
	}
	if strings.Contains(first, "example") {
		t.Fatalf("credential account exposes API URL: %q", first)
	}
}
