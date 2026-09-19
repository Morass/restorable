// Package safe holds the rules that keep a restore inside the directory it was
// given, however the paths inside a backup are spelled.
package safe

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Join places an absolute path from a backup under a target directory and
// refuses anything that would land outside it. A snapshot is data, not a
// promise: an entry can be spelled "../../etc/passwd" or hold a "..", and
// filepath.Join would happily walk out of the target with it.
func Join(target, path string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("no target directory")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%q is not an absolute path", path)
	}
	// The components are checked before cleaning: "/x/../../../etc/passwd"
	// cleans to a path inside the target, but it is not the file the backup
	// named, and comparing it with the live /etc/passwd would be a lie.
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("%q climbs out of the backup", path)
		}
	}
	clean := filepath.Clean(path)
	joined := filepath.Join(target, strings.TrimPrefix(clean, string(filepath.Separator)))
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absTarget, joined)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q would be written outside %s", path, target)
	}
	return joined, nil
}

// Argument refuses a value that a command-line tool would read as an option.
// Snapshot ids and paths come out of a repository, which the user may not have
// made, so they are arguments we choose to trust only after looking at them.
func Argument(kind, value string) error {
	switch {
	case value == "":
		return fmt.Errorf("empty %s", kind)
	case strings.HasPrefix(value, "-"):
		return fmt.Errorf("%s %q starts with '-', which the tool would read as an option", kind, value)
	case strings.ContainsAny(value, "\x00\n"):
		return fmt.Errorf("%s %q contains a newline or a NUL", kind, value)
	}
	return nil
}

// SnapshotID refuses an id that is not one: restic ids are hexadecimal, and
// "latest" is the only word it accepts.
func SnapshotID(id string) error {
	if id == "latest" {
		return nil
	}
	if id == "" {
		return fmt.Errorf("empty snapshot id")
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return fmt.Errorf("snapshot id %q is not hexadecimal", id)
		}
	}
	return nil
}
