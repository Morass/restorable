// Package config reads restorable's own configuration: which repositories to
// look at, what to walk, and when a backup counts as stale. It never holds a
// passphrase — only the user's own command that prints one.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResticRepo is one restic repository the user wants watched.
type ResticRepo struct {
	Name            string `json:"name"`
	Repo            string `json:"repo"`
	PasswordCommand string `json:"password_command,omitempty"`
}

// Config is the whole file.
type Config struct {
	Roots           []string     `json:"roots,omitempty"`
	StaleAfterHours int          `json:"stale_after_hours,omitempty"`
	Restic          []ResticRepo `json:"restic,omitempty"`
	// TimeMachine can be switched off on a Mac that does not use it.
	TimeMachine *bool `json:"time_machine,omitempty"`

	path string
}

// DefaultStaleHours is when a backup starts counting as stale.
const DefaultStaleHours = 48

// Path returns the file the configuration was read from, empty when defaults.
func (c Config) Path() string { return c.path }

// Dir is the configuration directory, honouring XDG_CONFIG_HOME.
func Dir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "restorable"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "restorable"), nil
}

// File is the configuration file path.
func File() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// Load reads the configuration. A missing file is not an error: the defaults
// plus whatever the environment already says about a repository are enough to be
// useful on a machine with no configuration at all.
func Load() (Config, error) {
	var c Config
	path, err := File()
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		c.fromEnvironment()
		return c.withDefaults()
	case err != nil:
		return c, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return c, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return c, fmt.Errorf("%s is readable by other users (mode %04o); run: chmod 600 %s",
			path, info.Mode().Perm(), path)
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	c.path = path
	c.fromEnvironment()
	return c.withDefaults()
}

// fromEnvironment adds the repository the user's shell already points at, so the
// tool is useful before anything is configured.
func (c *Config) fromEnvironment() {
	repo := os.Getenv("RESTIC_REPOSITORY")
	if repo == "" {
		return
	}
	for _, r := range c.Restic {
		if r.Repo == repo {
			return
		}
	}
	c.Restic = append(c.Restic, ResticRepo{
		Name:            repo, // the repository itself is the clearest name for it
		Repo:            repo,
		PasswordCommand: os.Getenv("RESTIC_PASSWORD_COMMAND"),
	})
}

func (c Config) withDefaults() (Config, error) {
	if c.StaleAfterHours <= 0 {
		c.StaleAfterHours = DefaultStaleHours
	}
	if len(c.Roots) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return c, err
		}
		c.Roots = []string{home}
	}
	for i, r := range c.Roots {
		e, err := ExpandPath(r)
		if err != nil {
			return c, err
		}
		c.Roots[i] = e
	}
	for i, r := range c.Restic {
		if err := validateRepo("restic", r.Name, r.Repo); err != nil {
			return c, err
		}
		c.Restic[i].Repo = r.Repo
	}
	return c, nil
}

// TimeMachineEnabled reports whether Time Machine should be read.
func (c Config) TimeMachineEnabled() bool { return c.TimeMachine == nil || *c.TimeMachine }

// ExpandPath turns ~ and ~/x into absolute paths and rejects the rest.
func ExpandPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("empty path in configuration")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("%q is not an absolute path", p)
	}
	return filepath.Clean(p), nil
}

// validateRepo refuses a repository string that would be read as an option by
// the tool we hand it to, and one that cannot be a repository at all.
func validateRepo(kind, name, repo string) error {
	switch {
	case strings.TrimSpace(repo) == "":
		return fmt.Errorf("%s repository %q has no repo", kind, name)
	case strings.HasPrefix(repo, "-"):
		return fmt.Errorf("%s repository %q starts with '-', which %s would read as an option", kind, name, kind)
	case strings.ContainsAny(repo, "\x00\n"):
		return fmt.Errorf("%s repository %q contains a newline or a NUL", kind, name)
	}
	return nil
}

// Example is the configuration written by `restorable config --example`.
const Example = `{
  "roots": ["~"],
  "stale_after_hours": 48,
  "restic": [
    {
      "name": "backup disk",
      "repo": "/Volumes/backup/restic",
      "password_command": "security find-generic-password -s restic-backup -w"
    }
  ]
}
`
