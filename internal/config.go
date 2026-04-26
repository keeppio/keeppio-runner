package internal

import (
	"errors"
	"fmt"
	"os"
)

// Config is parsed once at startup from environment variables. Keep
// the surface tiny — anything per-action lives in actions.yml.
type Config struct {
	// Listen address. nginx terminates TLS in front; we serve plain HTTP.
	Addr string

	// Identifier for this runner instance. Drives bucket naming
	// nowhere; just used in the UI header and in commit-author hints.
	Env string

	// Path to the cloned keeppio-infrastructure repo. Working dir for
	// every ansible-playbook invocation.
	RepoPath string

	// Branch to track. We hard-reset to origin/<branch> on every run
	// so a hand-edited tree never drifts.
	RepoBranch string

	// HTTPS URL with embedded creds for `git push`. The same URL we
	// use today in the playbooks (https://x-access-token:<pat>@...).
	RepoURL string

	// File holding the ansible-vault password for this env. Mounted
	// read-only from the host.
	VaultPasswordFile string

	// Ansible vault label (e.g. "staging" or "production"). Passed
	// to ansible-playbook as `--vault-id <label>@<file>`.
	VaultLabel string

	// SQLite db file path. Persistent across restarts.
	DBPath string

	// Where to write each task's full log file. One file per task.
	LogDir string

	// Symmetric key for session cookie signing. 32 random bytes,
	// generated once and persisted via `runner_session_key` setting
	// when not provided here.
	SessionKey []byte

	// Initial admin password. Only used on first boot; ignored once
	// `admin_password_hash` is in the settings table.
	InitialAdminPassword string

	// Optional GitHub PAT for the GHCR-tags source resolver. If empty,
	// `enum: ghcr-tags` fields fall back to a single `main` choice.
	GitHubPAT string
}

// LoadConfig reads required env vars and validates them. Missing
// required ones are listed together so an operator sees all of them
// at once instead of fixing one and re-running.
func LoadConfig() (*Config, error) {
	c := &Config{
		Addr:                 envOr("RUNNER_ADDR", ":3000"),
		Env:                  os.Getenv("RUNNER_ENV"),
		RepoPath:             os.Getenv("RUNNER_REPO_PATH"),
		RepoBranch:           envOr("RUNNER_REPO_BRANCH", "main"),
		RepoURL:              os.Getenv("RUNNER_REPO_URL"),
		VaultPasswordFile:    os.Getenv("RUNNER_VAULT_PASSWORD_FILE"),
		VaultLabel:           os.Getenv("RUNNER_VAULT_LABEL"),
		DBPath:               envOr("RUNNER_DB_PATH", "/data/runner.db"),
		LogDir:               envOr("RUNNER_LOG_DIR", "/data/logs"),
		InitialAdminPassword: os.Getenv("RUNNER_ADMIN_PASSWORD"),
		GitHubPAT:            os.Getenv("RUNNER_GITHUB_PAT"),
	}

	var missing []string
	for k, v := range map[string]string{
		"RUNNER_ENV":                 c.Env,
		"RUNNER_REPO_PATH":           c.RepoPath,
		"RUNNER_REPO_URL":            c.RepoURL,
		"RUNNER_VAULT_PASSWORD_FILE": c.VaultPasswordFile,
		"RUNNER_VAULT_LABEL":         c.VaultLabel,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("required env vars unset: %v", missing)
	}

	// Confirm the vault password file is readable up-front. Failing
	// at boot is friendlier than blowing up on the first task run.
	if _, err := os.Stat(c.VaultPasswordFile); err != nil {
		return nil, fmt.Errorf("vault password file %s: %w", c.VaultPasswordFile, err)
	}

	return c, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ErrNotFound is returned by stores when a row doesn't exist. Keeps
// handlers' branching readable.
var ErrNotFound = errors.New("not found")
