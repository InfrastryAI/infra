package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/zalando/go-keyring"
)

const keyringService = "infrastry-cli"

var ErrCredentialsNotFound = errors.New("credentials not found")

type Credentials struct {
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

type CredentialStore interface {
	Get(apiURL string) (Credentials, error)
	Set(apiURL string, credentials Credentials) error
	Delete(apiURL string) error
}

// KeyringCredentialStore stores OAuth tokens in the platform credential
// manager: Keychain on macOS, Secret Service on Linux, and Credential Manager
// on Windows.
type KeyringCredentialStore struct{}

func (KeyringCredentialStore) Get(apiURL string) (Credentials, error) {
	encoded, err := keyring.Get(keyringService, credentialAccount(apiURL))
	if errors.Is(err, keyring.ErrNotFound) {
		return Credentials{}, ErrCredentialsNotFound
	}
	if err != nil {
		return Credentials{}, err
	}
	var credentials Credentials
	if err := json.Unmarshal([]byte(encoded), &credentials); err != nil {
		return Credentials{}, fmt.Errorf("decode keyring credentials: %w", err)
	}
	if credentials.AccessToken == "" && credentials.RefreshToken == "" {
		return Credentials{}, errors.New("keyring credentials are missing OAuth tokens")
	}
	return credentials, nil
}

func (KeyringCredentialStore) Set(apiURL string, credentials Credentials) error {
	if credentials.AccessToken == "" && credentials.RefreshToken == "" {
		return errors.New("cannot store credentials without an OAuth token")
	}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		return fmt.Errorf("encode keyring credentials: %w", err)
	}
	return keyring.Set(keyringService, credentialAccount(apiURL), string(encoded))
}

func (KeyringCredentialStore) Delete(apiURL string) error {
	err := keyring.Delete(keyringService, credentialAccount(apiURL))
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrCredentialsNotFound
	}
	return err
}

func credentialAccount(apiURL string) string {
	digest := sha256.Sum256([]byte(apiURL))
	return "profile-" + base64.RawURLEncoding.EncodeToString(digest[:])
}

// MemoryCredentialStore is useful for embedding the CLI and for tests that
// must not access the user's platform keyring.
type MemoryCredentialStore struct {
	mu          sync.Mutex
	credentials map[string]Credentials
}

func (store *MemoryCredentialStore) Get(apiURL string) (Credentials, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	credentials, ok := store.credentials[apiURL]
	if !ok {
		return Credentials{}, ErrCredentialsNotFound
	}
	return credentials, nil
}

func (store *MemoryCredentialStore) Set(apiURL string, credentials Credentials) error {
	if credentials.AccessToken == "" && credentials.RefreshToken == "" {
		return errors.New("cannot store credentials without an OAuth token")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.credentials == nil {
		store.credentials = make(map[string]Credentials)
	}
	store.credentials[apiURL] = credentials
	return nil
}

func (store *MemoryCredentialStore) Delete(apiURL string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.credentials[apiURL]; !ok {
		return ErrCredentialsNotFound
	}
	delete(store.credentials, apiURL)
	return nil
}
