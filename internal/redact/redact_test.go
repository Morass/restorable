package redact_test

import (
	"strings"
	"testing"

	"github.com/morass/restorable/internal/redact"
)

func TestCredentialsInARepositoryUrlAreRemoved(t *testing.T) {
	cases := map[string]string{
		"rest:https://alice:hunter2@backup.example.org/repo": "rest:https://alice:***@backup.example.org/repo",
		"s3:https://KEYID:SECRETKEY@s3.example.org/bucket":   "s3:https://KEYID:***@s3.example.org/bucket",
		"sftp:user@host:/srv/repo":                           "sftp:user@host:/srv/repo", // no scheme:// , nothing to hide
		"/Volumes/backup/restic":                             "/Volumes/backup/restic",
	}
	for in, want := range cases {
		if got := redact.Location(in); got != want {
			t.Errorf("Location(%q) = %q, want %q", in, got, want)
		}
	}
	if strings.Contains(redact.Location("rest:https://alice:hunter2@host/repo"), "hunter2") {
		t.Error("the passphrase survived")
	}
}

func TestASecretFromTheEnvironmentIsNeverEchoed(t *testing.T) {
	t.Setenv("RESTIC_PASSWORD", "correct-horse-battery")
	msg := "restic: config load failed for repository with password correct-horse-battery"
	got := redact.Secrets(msg)
	if strings.Contains(got, "correct-horse-battery") {
		t.Errorf("Secrets left the passphrase in: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("Secrets = %q, want the value replaced", got)
	}
	// A short value is a word, not a secret to chase through every message: a
	// passphrase of "demo" must not turn /tmp/demo-repo into /tmp/***-repo.
	t.Setenv("RESTIC_PASSWORD", "demo")
	if got := redact.Secrets("reading /tmp/demo-repo"); got != "reading /tmp/demo-repo" {
		t.Errorf("Secrets mangled an ordinary path: %q", got)
	}
}
