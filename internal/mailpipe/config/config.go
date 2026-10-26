// Package config reads mailpipe's private per-user configuration: the Entra
// app registration it signs in through (~/mail/config.json) and the stages,
// policy and owner's addresses (~/mail/stages.json). Both name the user's
// tenant or real senders, so they live outside any repository with mode 0600,
// never in code; tokens and API keys stay in the Keychain.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config is the user's mailpipe configuration.
type Config struct {
	ClientID string `json:"client_id"`
	TenantID string `json:"tenant_id"`
}

const (
	pathEnv     = "MAILPIPE_CONFIG"
	clientIDEnv = "MAILPIPE_CLIENT_ID"
	tenantIDEnv = "MAILPIPE_TENANT_ID"
	// InitHint is the command that creates the configuration.
	InitHint = "mailpipe config init --client-id <uuid> --tenant-id <uuid>"
)

// ErrNotConfigured reports that no configuration exists yet.
var ErrNotConfigured = errors.New("mailpipe is not configured")

var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// DefaultPath is $MAILPIPE_CONFIG, else ~/mail/config.json.
func DefaultPath() string {
	if p := strings.TrimSpace(os.Getenv(pathEnv)); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "config.json"
	}
	return filepath.Join(home, "mail", "config.json")
}

func (c Config) validate() error {
	var bad []string
	if !uuid.MatchString(c.ClientID) {
		bad = append(bad, "client_id")
	}
	if !uuid.MatchString(c.TenantID) {
		bad = append(bad, "tenant_id")
	}
	if len(bad) > 0 {
		return fmt.Errorf("config: %s must be a UUID (from the app registration's Overview page)", strings.Join(bad, " and "))
	}
	return nil
}

// Load reads path, then applies $MAILPIPE_CLIENT_ID and $MAILPIPE_TENANT_ID.
// A missing file is fine when the environment supplies both values.
func Load(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &c); err != nil {
			return c, fmt.Errorf("config: %s is not valid JSON: %v", path, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return c, fmt.Errorf("config: read %s: %v", path, err)
	}
	if v := strings.TrimSpace(os.Getenv(clientIDEnv)); v != "" {
		c.ClientID = v
	}
	if v := strings.TrimSpace(os.Getenv(tenantIDEnv)); v != "" {
		c.TenantID = v
	}
	if c.ClientID == "" && c.TenantID == "" {
		return c, fmt.Errorf("%w (%s does not exist)", ErrNotConfigured, path)
	}
	return c, c.validate()
}

// Save writes c to path with mode 0600, creating its directory with mode 0700.
func Save(path string, c Config) error {
	if err := c.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
