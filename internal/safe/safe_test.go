package safe_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/morass/restorable/internal/safe"
)

func TestAPathFromABackupCannotLeaveTheTarget(t *testing.T) {
	target := "/tmp/drill"
	for _, hostile := range []string{
		"../../etc/passwd",
		"/x/../../../../etc/passwd",
		"/../../../etc/shadow",
		"etc/passwd",
		"",
	} {
		if got, err := safe.Join(target, hostile); err == nil {
			t.Errorf("Join(%q) = %q, want it refused", hostile, got)
		}
	}
	got, err := safe.Join(target, "/Users/x/Documents/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(target, "Users/x/Documents/a.txt"); got != want {
		t.Errorf("Join = %q, want %q", got, want)
	}
	// A name that merely contains dots is fine.
	if _, err := safe.Join(target, "/Users/x/..hidden/a..b.txt"); err != nil {
		t.Errorf("an ordinary name with dots was refused: %v", err)
	}
}

func TestAnArgumentThatWouldBeReadAsAnOptionIsRefused(t *testing.T) {
	for _, bad := range []string{"-r", "--repo=/tmp", "", "a\nb", "a\x00b"} {
		if err := safe.Argument("path", bad); err == nil {
			t.Errorf("Argument(%q) was accepted", bad)
		}
	}
	if err := safe.Argument("path", "/Users/x/a -b.txt"); err != nil {
		t.Errorf("an ordinary path was refused: %v", err)
	}
}

func TestASnapshotIdMustLookLikeOne(t *testing.T) {
	for _, bad := range []string{"-r", "--host", "latest;rm -rf /", "zz", ""} {
		if err := safe.SnapshotID(bad); err == nil {
			t.Errorf("SnapshotID(%q) was accepted", bad)
		}
	}
	for _, good := range []string{"latest", "eb58252a", "AB12cd34"} {
		if err := safe.SnapshotID(good); err != nil {
			t.Errorf("SnapshotID(%q) was refused: %v", good, err)
		}
	}
}

func TestTheRefusalSaysWhat(t *testing.T) {
	_, err := safe.Join("/tmp/drill", "../../etc/passwd")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error = %v, want it to name the problem", err)
	}
}
