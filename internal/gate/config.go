// Package gate is Lumen's front door: a password login and session layer in
// front of a stock DeepSeek Harness (DSH) web server, meant to be published
// through Sol as a Tor onion service.
//
// The gate supervises DSH as a child process bound to 127.0.0.1 on a random
// port, performs DSH's launch-token exchange itself, and attaches DSH's
// session cookie to every request it forwards. The browser only ever holds
// the gate's own session cookie; DSH's token, its cookie, and the Gemini API
// key never leave the machine.
//
// Because the gate rewrites Host and Origin to DSH's loopback address (DSH
// rejects foreign origins), it enforces the cross-site checks DSH would have
// done: the request Origin must match the gate's own host.
package gate

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// pbkdf2Iterations follows OWASP guidance for PBKDF2-SHA256. Tests lower it.
var pbkdf2Iterations = 600_000

// Config is ~/.lumen/config.json (mode 0600).
type Config struct {
	Listen       string `json:"listen"`        // default 127.0.0.1:7777
	PasswordSalt []byte `json:"password_salt"` // random 16 bytes
	PasswordHash []byte `json:"password_hash"` // pbkdf2-sha256
	GeminiKey    string `json:"gemini_key"`    // passed to DSH as GEMINI_API_KEY
	DSHVersion   string `json:"dsh_version"`   // pinned npm version
	Workspace    string `json:"workspace"`     // working directory for DSH
}

// DefaultDSHVersion is the DSH release Lumen was built and tested against.
const DefaultDSHVersion = "0.2.2-rc.1"

// Dir returns ~/.lumen (or $LUMEN_HOME).
func Dir() (string, error) {
	if d := os.Getenv("LUMEN_HOME"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".lumen"), nil
}

// LoadConfig reads the config from dir.
func LoadConfig(dir string) (*Config, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("Lumen is not set up yet; run `lumen setup`")
	}
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7777"
	}
	if c.DSHVersion == "" {
		c.DSHVersion = DefaultDSHVersion
	}
	if len(c.PasswordHash) == 0 {
		return nil, errors.New("no password set; run `lumen setup`")
	}
	return c, nil
}

// Save writes the config atomically with mode 0600.
func (c *Config) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".config.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "config.json"))
}

// SetPassword stores a salted pbkdf2 hash of pw.
func (c *Config) SetPassword(pw string) error {
	if len(pw) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	h, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, 32)
	if err != nil {
		return err
	}
	c.PasswordSalt, c.PasswordHash = salt, h
	return nil
}

// CheckPassword reports whether pw matches, in constant time.
func (c *Config) CheckPassword(pw string) bool {
	h, err := pbkdf2.Key(sha256.New, pw, c.PasswordSalt, pbkdf2Iterations, 32)
	return err == nil && subtle.ConstantTimeCompare(h, c.PasswordHash) == 1
}
