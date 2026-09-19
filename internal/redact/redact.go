// Package redact removes what must never be printed, written to a receipt, or
// put in a JSON report: the credentials a repository location can carry, and any
// value the user's own environment holds as a secret.
package redact

import (
	"os"
	"regexp"
	"strings"
)

// secretVars are the environment variables whose *values* must never appear in
// anything restorable prints, because the user put a passphrase in them.
var secretVars = []string{
	"RESTIC_PASSWORD", "BORG_PASSPHRASE", "AWS_SECRET_ACCESS_KEY", "B2_ACCOUNT_KEY",
	"AZURE_ACCOUNT_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
}

// userinfo matches the credentials in a repository URL: rest:https://user:pw@host/repo.
var userinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)([^/@\s]+)@`)

// Location makes a repository location safe to show: the credentials some
// backends allow inside the URL are replaced, the rest is left as it is so the
// user still recognises their own repository.
func Location(s string) string {
	if s == "" {
		return s
	}
	// The whole userinfo goes: for S3 and B2 the "user" half is the access key
	// id, which is a credential in its own right.
	out := userinfo.ReplaceAllStringFunc(s, func(m string) string {
		parts := userinfo.FindStringSubmatch(m)
		return parts[1] + "***@"
	})
	return Secrets(out)
}

// All is the boundary every string from an external tool passes through: the
// credentials a location can carry, and any value the environment marks secret.
func All(s string) string { return Location(s) }

// Secrets replaces any value the environment marks as a secret wherever it
// appears — a tool's own error message can quote what it was given.
func Secrets(s string) string {
	if s == "" {
		return s
	}
	for _, name := range secretVars {
		v := os.Getenv(name)
		// Every passphrase is replaced, however short. A short one will also
		// blank the odd innocent word, which is the right way round to be wrong.
		if len(v) < 3 {
			continue
		}
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}
