package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/morass/restorable/internal/config"
)

// sandbox points HOME and XDG_CONFIG_HOME at a temporary directory and returns
// the configuration path inside it.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	t.Setenv("RESTIC_REPOSITORY", "")
	t.Setenv("RESTIC_PASSWORD_COMMAND", "")
	os.Unsetenv("RESTIC_REPOSITORY")
	os.Unsetenv("RESTIC_PASSWORD_COMMAND")
	path, err := config.File()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestNoFileIsNotAnError(t *testing.T) {
	sandbox(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("a machine with no configuration must still work: %v", err)
	}
	if cfg.Path() != "" {
		t.Errorf("path = %q, want empty when there is no file", cfg.Path())
	}
	if cfg.StaleAfterHours != config.DefaultStaleHours {
		t.Errorf("stale = %d, want the default %d", cfg.StaleAfterHours, config.DefaultStaleHours)
	}
	home, _ := os.UserHomeDir()
	if len(cfg.Roots) != 1 || cfg.Roots[0] != home {
		t.Errorf("roots = %v, want the home directory", cfg.Roots)
	}
	if !cfg.TimeMachineEnabled() {
		t.Error("Time Machine is read unless it is switched off")
	}
}

func TestAWorldReadableConfigurationIsRefused(t *testing.T) {
	path := sandbox(t)
	write(t, path, `{"roots":["~"]}`, 0o644)
	_, err := config.Load()
	if err == nil {
		t.Fatal("a configuration other users can read must be refused: it names a password command")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error = %v, want it to say how to fix it", err)
	}
}

func TestAnUnknownFieldIsRefused(t *testing.T) {
	path := sandbox(t)
	write(t, path, `{"rootz":["~"]}`, 0o600)
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "rootz") {
		t.Errorf("error = %v, want the misspelled field named", err)
	}
}

func TestTildeIsExpandedAndRelativePathsRefused(t *testing.T) {
	path := sandbox(t)
	write(t, path, `{"roots":["~/Documents"]}`, 0o600)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if want := filepath.Join(home, "Documents"); cfg.Roots[0] != want {
		t.Errorf("root = %q, want %q", cfg.Roots[0], want)
	}

	write(t, path, `{"roots":["Documents"]}`, 0o600)
	if _, err := config.Load(); err == nil {
		t.Error("a relative root must be refused, not guessed at")
	}
}

func TestARepositoryThatLooksLikeAnOptionIsRefused(t *testing.T) {
	path := sandbox(t)
	write(t, path, `{"restic":[{"name":"x","repo":"--no-lock"}]}`, 0o600)
	_, err := config.Load()
	if err == nil || !strings.Contains(err.Error(), "option") {
		t.Errorf("error = %v, want a repository starting with '-' refused", err)
	}

	write(t, path, `{"restic":[{"name":"x","repo":"/repo\nrm -rf /"}]}`, 0o600)
	if _, err := config.Load(); err == nil {
		t.Error("a repository with a newline must be refused")
	}
}

func TestTheEnvironmentsRepositoryIsPickedUpOnce(t *testing.T) {
	path := sandbox(t)
	write(t, path, `{"restic":[{"name":"disk","repo":"/Volumes/backup/restic"}]}`, 0o600)
	t.Setenv("RESTIC_REPOSITORY", "/Volumes/backup/restic")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Restic) != 1 {
		t.Errorf("repositories = %d, want the configured one not to be duplicated by the environment", len(cfg.Restic))
	}

	t.Setenv("RESTIC_REPOSITORY", "/elsewhere/repo")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Restic) != 2 {
		t.Errorf("repositories = %d, want the environment's repository added", len(cfg.Restic))
	}
}

func TestTimeMachineCanBeSwitchedOff(t *testing.T) {
	path := sandbox(t)
	write(t, path, `{"time_machine":false}`, 0o600)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TimeMachineEnabled() {
		t.Error("time_machine:false must switch it off")
	}
}

func TestTheExampleConfigurationIsValid(t *testing.T) {
	path := sandbox(t)
	write(t, path, config.Example, 0o600)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("the example we hand users must load: %v", err)
	}
	if len(cfg.Restic) != 1 {
		t.Errorf("the example should configure one repository, got %d", len(cfg.Restic))
	}
}
