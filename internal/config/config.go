package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	currentVersion = 1
	DefaultAPIURL  = "https://infrastry.ai"
)

// Config stores profile metadata by Infrastry installation so local
// development, staging, and production logins do not overwrite one another.
// OAuth tokens are normally redacted before this structure is written and are
// retained here only when the platform keyring is unavailable.
type Config struct {
	Version  int                `json:"version"`
	Current  string             `json:"current,omitempty"`
	Profiles map[string]Profile `json:"profiles,omitempty"`
	// Preserve repository mappings saved by other CLI versions, even though
	// this build does not use them. Saving profiles must not discard this data.
	Repositories json.RawMessage `json:"repositories,omitempty"`
}

type Profile struct {
	APIURL        string    `json:"api_url"`
	Issuer        string    `json:"issuer"`
	Resource      string    `json:"resource"`
	ClientID      string    `json:"client_id"`
	TokenEndpoint string    `json:"token_endpoint"`
	AccessToken   string    `json:"access_token,omitempty"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	TokenType     string    `json:"token_type"`
	ExpiresAt     time.Time `json:"expires_at"`
	Scopes        []string  `json:"scopes,omitempty"`
	TeamID        string    `json:"team_id,omitempty"`
	TeamSlug      string    `json:"team_slug,omitempty"`
	TeamRef       string    `json:"team_ref,omitempty"`
	TeamName      string    `json:"team_name,omitempty"`
}

type Store struct {
	Path string
}

func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user configuration directory: %w", err)
	}
	return filepath.Join(dir, "infrastry", "config.json"), nil
}

func (s Store) Load() (Config, error) {
	config := Config{Version: currentVersion, Profiles: make(map[string]Profile)}
	pathInfo, err := os.Lstat(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("inspect configuration: %w", err)
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		return Config{}, fmt.Errorf("refusing to read symlink at configuration path %q", s.Path)
	}
	file, err := os.Open(s.Path)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return Config{}, fmt.Errorf("inspect configuration: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("configuration %q is not a regular file", s.Path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return Config{}, fmt.Errorf("configuration %q has unsafe permissions %04o; run chmod 600 %q", s.Path, info.Mode().Perm(), s.Path)
	}

	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode configuration: %w", err)
	}
	if config.Version != currentVersion {
		return Config{}, fmt.Errorf("configuration version %d is unsupported", config.Version)
	}
	if config.Profiles == nil {
		config.Profiles = make(map[string]Profile)
	}
	return config, nil
}

func (s Store) Save(config Config) error {
	config.Version = currentVersion
	if config.Profiles == nil {
		config.Profiles = make(map[string]Profile)
	}

	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create configuration directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil && runtime.GOOS != "windows" {
		return fmt.Errorf("secure configuration directory: %w", err)
	}
	if info, err := os.Lstat(s.Path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to replace symlink at configuration path %q", s.Path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect configuration path: %w", err)
	}

	temporary, err := os.CreateTemp(dir, ".config-*")
	if err != nil {
		return fmt.Errorf("create temporary configuration: %w", err)
	}
	temporaryName := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryName)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary configuration: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(config); err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush configuration: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close configuration: %w", err)
	}
	if err := replace(temporaryName, s.Path); err != nil {
		return fmt.Errorf("replace configuration: %w", err)
	}
	committed = true
	return nil
}

func replace(source, destination string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(source, destination)
	}

	// Windows does not replace an existing file with os.Rename. Preserve the
	// previous config until the new one has reached its final name so a failed
	// replacement does not discard refresh credentials.
	backup := source + ".previous"
	if err := os.Rename(destination, backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(source, destination); err != nil {
		_ = os.Rename(backup, destination)
		return err
	}
	_ = os.Remove(backup)
	return nil
}

func (c Config) Profile(apiURL string) (Profile, bool) {
	profile, ok := c.Profiles[apiURL]
	return profile, ok
}

func (c *Config) SetProfile(profile Profile) {
	if c.Profiles == nil {
		c.Profiles = make(map[string]Profile)
	}
	c.Profiles[profile.APIURL] = profile
	c.Current = profile.APIURL
}

func (c *Config) SetCurrentAPIURL(apiURL string) {
	c.Current = apiURL
}

func (c *Config) SetTeam(apiURL, teamID, teamSlug, teamRef, teamName string) bool {
	profile, ok := c.Profiles[apiURL]
	if !ok {
		return false
	}
	profile.TeamID = teamID
	profile.TeamSlug = teamSlug
	profile.TeamRef = teamRef
	profile.TeamName = teamName
	c.Profiles[apiURL] = profile
	return true
}

func (c *Config) RemoveProfile(apiURL string) bool {
	if _, ok := c.Profiles[apiURL]; !ok {
		return false
	}
	delete(c.Profiles, apiURL)
	return true
}

func NormalizeAPIURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("API URL cannot be empty")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid API URL: %w", err)
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return "", errors.New("API URL must use http or https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("API URL must contain only a scheme, host, and optional path")
	}
	if parsed.Scheme == "http" {
		hostname := parsed.Hostname()
		if !strings.EqualFold(hostname, "localhost") && !net.ParseIP(hostname).IsLoopback() {
			return "", errors.New("API URL must use HTTPS except on localhost or a loopback IP address")
		}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}
