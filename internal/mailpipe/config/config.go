// Package config reads private provider configuration, configured principal and
// account/collection routing, external-processing consent, stages and policy.
// These name the user's
// tenant or real senders, so they live outside any repository with mode 0600,
// never in code; tokens and API keys stay in the Keychain.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/mrothroc/mailpipe/pkg/imapauth"
	"github.com/mrothroc/mailpipe/pkg/mailbox"
)

// Config is the user's mailpipe configuration.
type Config struct {
	ClientID    string                     `json:"client_id"`
	TenantID    string                     `json:"tenant_id"`
	Provider    string                     `json:"provider,omitempty"`
	AccountID   string                     `json:"account_id,omitempty"`
	Collections map[string]string          `json:"collections,omitempty"`
	IMAP        *IMAPConfig                `json:"imap,omitempty"`
	Credential  *CredentialState           `json:"credential,omitempty"`
	Processing  *Processing                `json:"processing,omitempty"`
	Extra       map[string]json.RawMessage `json:"-"`
}

var configKeys = []string{"client_id", "tenant_id", "provider", "account_id", "collections", "imap", "credential", "processing"}

func object(b []byte, out any, known []string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return nil, errors.New("config: expected object")
	}
	fields := map[string]json.RawMessage{}
	keys := map[string]bool{}
	for _, k := range known {
		keys[k] = true
	}
	for d.More() {
		t, e := d.Token()
		if e != nil {
			return nil, errors.New("config: invalid object")
		}
		k, ok := t.(string)
		if !ok {
			return nil, errors.New("config: invalid key")
		}
		for _, reserved := range known {
			if k != reserved && strings.EqualFold(k, reserved) {
				return nil, errors.New("config: ambiguous security-relevant key spelling")
			}
		}
		if _, exists := fields[k]; exists && keys[k] {
			return nil, errors.New("config: duplicate security-relevant key")
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return nil, errors.New("config: invalid value")
		}
		fields[k] = v
	}
	if _, e = d.Token(); e != nil {
		return nil, errors.New("config: invalid object")
	}
	var tail any
	if d.Decode(&tail) != io.EOF {
		return nil, errors.New("config: trailing JSON")
	}
	// Decode only exact known keys: encoding/json's case-insensitive struct
	// matching must never turn an unknown private key into security authority.
	typed := map[string]json.RawMessage{}
	for _, k := range known {
		if v, ok := fields[k]; ok {
			typed[k] = v
		}
	}
	typedJSON, err := json.Marshal(typed)
	if err != nil || json.Unmarshal(typedJSON, out) != nil {
		return nil, errors.New("config: invalid field type")
	}
	for _, k := range known {
		delete(fields, k)
	}
	if len(fields) == 0 {
		fields = nil
	}
	return fields, nil
}
func merged(v any, extra map[string]json.RawMessage, known []string) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	var m map[string]json.RawMessage
	if e = json.Unmarshal(b, &m); e != nil {
		return nil, e
	}
	reserved := map[string]bool{}
	for _, k := range known {
		reserved[k] = true
	}
	for k, v := range extra {
		alias := reserved[k]
		for _, key := range known {
			alias = alias || strings.EqualFold(k, key)
		}
		if !alias {
			m[k] = v
		}
	}
	return json.Marshal(m)
}
func (c *Config) UnmarshalJSON(b []byte) error {
	type plain Config
	var p plain
	x, e := object(b, &p, configKeys)
	*c = Config(p)
	c.Extra = x
	return e
}
func (c Config) MarshalJSON() ([]byte, error) {
	type plain Config
	return merged(plain(c), c.Extra, configKeys)
}

// IMAPConfig is a versioned configured principal, not an immutable remote subject.
type IMAPConfig struct {
	IdentityVersion int                        `json:"identity_version"`
	Endpoint        string                     `json:"endpoint"`
	Login           string                     `json:"login"`
	RoleMappings    map[string]string          `json:"role_mappings,omitempty"`
	Extra           map[string]json.RawMessage `json:"-"`
}

var imapKeys = []string{"identity_version", "endpoint", "login", "role_mappings"}

func (c *IMAPConfig) UnmarshalJSON(b []byte) error {
	type plain IMAPConfig
	var p plain
	x, e := object(b, &p, imapKeys)
	*c = IMAPConfig(p)
	c.Extra = x
	return e
}
func (c IMAPConfig) MarshalJSON() ([]byte, error) {
	type plain IMAPConfig
	return merged(plain(c), c.Extra, imapKeys)
}

// CredentialState is nonsecret persistence acknowledgement, never a password.
type CredentialState struct {
	Version    int                        `json:"version"`
	Generation string                     `json:"generation"`
	Ready      bool                       `json:"ready"`
	Extra      map[string]json.RawMessage `json:"-"`
}

var credentialKeys = []string{"version", "generation", "ready"}

func (c *CredentialState) UnmarshalJSON(b []byte) error {
	type plain CredentialState
	var p plain
	x, e := object(b, &p, credentialKeys)
	*c = CredentialState(p)
	c.Extra = x
	return e
}
func (c CredentialState) MarshalJSON() ([]byte, error) {
	type plain CredentialState
	return merged(plain(c), c.Extra, credentialKeys)
}

// Processing records explicit account-scoped external processing authority.
type Processing struct {
	Version        int                        `json:"version"`
	AccountID      string                     `json:"account_id"`
	Models         bool                       `json:"models"`
	ExternalStages bool                       `json:"external_stages"`
	Extra          map[string]json.RawMessage `json:"-"`
}

var processingKeys = []string{"version", "account_id", "models", "external_stages"}

func (c *Processing) UnmarshalJSON(b []byte) error {
	type plain Processing
	var p plain
	x, e := object(b, &p, processingKeys)
	*c = Processing(p)
	c.Extra = x
	return e
}
func (c Processing) MarshalJSON() ([]byte, error) {
	type plain Processing
	return merged(plain(c), c.Extra, processingKeys)
}
func (c Config) Mode() string {
	if c.Provider == "" {
		return "graph"
	}
	return c.Provider
}
func (c Config) Principal() (imapauth.Principal, error) {
	if c.Mode() != "yahoo" || c.IMAP == nil || c.IMAP.IdentityVersion != 1 {
		return imapauth.Principal{}, errors.New("config: supported versioned Yahoo principal required")
	}
	return imapauth.NewPrincipal("yahoo", c.IMAP.Endpoint, c.IMAP.Login)
}
func (c Config) Account() mailbox.Account {
	return mailbox.Account{ID: mailbox.AccountID(c.AccountID), ProviderID: mailbox.ProviderID(c.Mode())}
}
func (c *Config) normalize() error {
	if c.Mode() == "yahoo" {
		p, e := c.Principal()
		if e != nil {
			return e
		}
		copy := *c.IMAP
		copy.Endpoint = p.Endpoint
		c.IMAP = &copy
	}
	return c.validate()
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
var credentialGeneration = regexp.MustCompile(`^[0-9a-f]{32}$`)

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
	if c.Processing != nil && c.Processing.Version != 1 {
		return errors.New("config: unsupported processing version")
	}
	if c.Credential != nil && c.Credential.Version != 1 {
		return errors.New("config: unsupported credential state version")
	}
	if c.Credential != nil {
		g := c.Credential.Generation
		if (g != "" && !credentialGeneration.MatchString(g)) || (c.Credential.Ready && g == "") {
			return errors.New("config: invalid credential generation")
		}
	}
	if c.Mode() == "yahoo" {
		p, e := c.Principal()
		if e != nil {
			return e
		}
		if c.AccountID != string(p.Account().ID) {
			return errors.New("config: configured account does not match endpoint/login principal; select a separate configuration and database")
		}
		for role, name := range c.IMAP.RoleMappings {
			if (role != "sent" && role != "trash" && role != "junk") || name == "" || len(name) > 4096 || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\r\n") {
				return errors.New("config: invalid explicit IMAP role mapping")
			}
		}
		return nil
	}
	if c.Mode() != "graph" {
		return errors.New("config: unsupported provider; select an installed mailbox provider")
	}
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
	if v := strings.TrimSpace(os.Getenv(clientIDEnv)); c.Mode() == "graph" && v != "" {
		c.ClientID = v
	}
	if v := strings.TrimSpace(os.Getenv(tenantIDEnv)); c.Mode() == "graph" && v != "" {
		c.TenantID = v
	}
	if c.Mode() == "graph" && c.ClientID == "" && c.TenantID == "" {
		return c, fmt.Errorf("%w (%s does not exist)", ErrNotConfigured, path)
	}
	return c, c.normalize()
}

// Save writes c to path with mode 0600, creating its directory with mode 0700.
func Save(path string, c Config) error {
	if err := c.normalize(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".mailpipe-config-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
