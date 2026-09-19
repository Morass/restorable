package redact_test

import (
	"strings"
	"testing"

	"github.com/morass/restorable/internal/redact"
)

func TestCredentialsInARepositoryUrlAreRemoved(t *testing.T) {
	cases := map[string]string{
		// The whole userinfo goes: an S3 access key id is a credential too.
		"rest:https://alice:hunter2@backup.example.org/repo": "rest:https://***@backup.example.org/repo",
		"s3:https://KEYID:SECRETKEY@s3.example.org/bucket":   "s3:https://***@s3.example.org/bucket",
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
	// A short passphrase is still a passphrase: it is replaced even where that
	// makes an ordinary word disappear, because the other way round leaks.
	t.Setenv("RESTIC_PASSWORD", "demo")
	if got := redact.Secrets("reading /tmp/demo-repo"); strings.Contains(got, "demo-repo") {
		t.Errorf("a short passphrase survived: %q", got)
	}
}
