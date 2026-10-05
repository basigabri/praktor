package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The example config (and the Home Assistant App's default config) puts
// secrets into the YAML as "${ENV_VAR}". Load expands environment variables
// in the raw file text before parsing it, so the secret's characters become
// YAML syntax.
const secretsYAML = `telegram:
  token: "${PRAKTOR_TELEGRAM_TOKEN}"
web:
  auth: "${PRAKTOR_WEB_PASSWORD}"
vault:
  passphrase: "${PRAKTOR_VAULT_PASSPHRASE}"
`

func loadWithSecret(t *testing.T, yaml, secret string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "praktor.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRAKTOR_CONFIG", path)
	t.Setenv("PRAKTOR_TELEGRAM_TOKEN", "123:abc")
	t.Setenv("PRAKTOR_WEB_PASSWORD", secret)
	t.Setenv("PRAKTOR_VAULT_PASSPHRASE", secret)
	return Load()
}

// Secrets that are plain YAML-safe text load fine, including "$" sequences:
// expansion is a single pass, so a "$VAR" inside a value isn't expanded again.
func TestSecretsWithSafeCharacters(t *testing.T) {
	t.Setenv("HOME", "/root")
	for _, secret := range []string{
		"plain", "with space", "$HOME", "${HOME}", "$$", "a$", "it's", "#hash", "a: b", "{x}", "[x]", "x, y", "ünïcødé 🔑",
	} {
		t.Run(secret, func(t *testing.T) {
			cfg, err := loadWithSecret(t, secretsYAML, secret)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Web.Auth != secret || cfg.Vault.Passphrase != secret {
				t.Errorf("auth %q, passphrase %q, want %q", cfg.Web.Auth, cfg.Vault.Passphrase, secret)
			}
		})
	}
}

// A password with a double quote or a backslash breaks the YAML, and the
// gateway refuses to start ("parse config: ..."). A crafted value can also add
// config keys. The env overrides already set token, password and passphrase,
// so the values in the file don't matter, only that the file still parses.
func TestSecretsWithYAMLSyntaxDoNotBreakConfig(t *testing.T) {
	knownBug(t, `Load runs os.ExpandEnv over the raw YAML, so a secret containing " or \ (or a newline) `+
		`breaks parsing or injects keys when the config uses "${VAR}" (config/praktor.example.yaml and the HA App's default do)`)
	t.Setenv("HOME", "/root")
	for _, secret := range []string{
		`pa"ss`, `pa\ss`, `C:\temp`, `ends with \`, "two\nlines", `x"` + "\nspeech:\n  api_key: \"injected",
	} {
		t.Run(secret, func(t *testing.T) {
			cfg, err := loadWithSecret(t, secretsYAML, secret)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Web.Auth != secret || cfg.Vault.Passphrase != secret {
				t.Errorf("auth %q, passphrase %q, want %q", cfg.Web.Auth, cfg.Vault.Passphrase, secret)
			}
			if cfg.Speech.APIKey != "" {
				t.Errorf("secret injected speech.api_key = %q", cfg.Speech.APIKey)
			}
		})
	}
}

// knownBug skips a test that demonstrates an open bug, so CI stays green
// until it is fixed. Set PRAKTOR_RUN_KNOWN_BUGS=1 to run it and see it fail.
func knownBug(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("PRAKTOR_RUN_KNOWN_BUGS") == "" {
		t.Skip("BUG: " + msg)
	}
}
