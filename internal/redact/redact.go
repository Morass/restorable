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
	out := userinfo.ReplaceAllStringFunc(s, func(m string) string {
		parts := userinfo.FindStringSubmatch(m)
		scheme, cred := parts[1], parts[2]
		name := cred
		if i := strings.IndexByte(cred, ':'); i >= 0 {
			name = cred[:i]
		}
		if name == "" {
			return scheme + "***@"
		}
		return scheme + name + ":***@"
	})
	return Secrets(out)
}

// Secrets replaces any value the environment marks as a secret wherever it
// appears — a tool's own error message can quote what it was given.
func Secrets(s string) string {
	if s == "" {
		return s
	}
	for _, name := range secretVars {
		v := os.Getenv(name)
		// A short value is a word, not a distinctive secret: replacing it
		// everywhere would mangle repository paths and ordinary messages.
		if len(v) < 8 {
			continue
		}
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}
