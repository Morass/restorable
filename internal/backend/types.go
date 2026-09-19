// Package backend describes what restorable can read: a backup destination, the
// snapshots on it, and the two questions that matter — does it claim this path,
// and can it hand a file back.
package backend

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
)

// Kind names a backup system.
type Kind string

const (
	TimeMachine Kind = "timemachine"
	Restic      Kind = "restic"
	Borg        Kind = "borg"
)

// State says how much of a destination restorable could actually read.
type State string

const (
	// StateOK means the destination answered and its snapshots were listed.
	StateOK State = "ok"
	// StateLocked means the destination exists but needs a passphrase that the
	// user has not made available to this session.
	StateLocked State = "locked"
	// StateUnreachable means the repository or the backup disk is not there.
	StateUnreachable State = "unreachable"
	// StateError means the tool failed for another reason, kept in Err.
	StateError State = "error"
)

// Destination is one place backups are written to.
type Destination struct {
	Backend Kind     `json:"backend"`
	ID      string   `json:"id"`    // repository path, or the Time Machine destination ID
	Label   string   `json:"label"` // human name, may be empty
	Roots   []string `json:"roots"` // absolute paths this destination claims to cover
	State   State    `json:"state"`
	Err     string   `json:"error,omitempty"`
	// NotConfigured marks a backup system that exists on this machine but has
	// never been set up. It is only a problem when nothing else is.
	NotConfigured bool `json:"not_configured,omitempty"`

	Snapshots int       `json:"snapshots"`
	LastOK    time.Time `json:"last_ok,omitempty"` // newest completed snapshot
	// LastOKSource names where the date came from, because a Time Machine date
	// can be a local snapshot, a mounted destination, or a root-only plist.
	LastOKSource string `json:"last_ok_source,omitempty"`
}

// Snapshot is one recovery point.
type Snapshot struct {
	ID    string    `json:"id"`
	Time  time.Time `json:"time"`
	Paths []string  `json:"paths,omitempty"`
	Host  string    `json:"host,omitempty"`
}

// File is one entry inside a snapshot.
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir"`
}

// Exclusion explains why a path is not backed up by a destination.
type Exclusion struct {
	Excluded bool   `json:"excluded"`
	Reason   string `json:"reason,omitempty"` // as specific as the system allows
}

// ErrUnsupported is returned by a backend that cannot answer a question at all
// (Time Machine, for instance, cannot restore a single file without the disk).
var ErrUnsupported = errors.New("not supported by this backend")

// Backend is one backup system on this machine.
type Backend interface {
	Kind() Kind
	// Installed reports whether the tool needed to read this backend exists.
	Installed() bool
	// Destinations lists what this backend is configured to write to.
	Destinations(ctx context.Context) ([]Destination, error)
	// Snapshots lists recovery points on one destination, newest last.
	Snapshots(ctx context.Context, d Destination) ([]Snapshot, error)
	// Excluded says whether a path is kept out of this destination's backups.
	Excluded(ctx context.Context, d Destination, paths []string) ([]Exclusion, error)
	// List returns the entries a snapshot holds under one path.
	List(ctx context.Context, d Destination, snapshot, path string) ([]File, error)
	// Restore writes the given files from a snapshot under target, keeping their
	// absolute layout, and returns the paths it wrote.
	Restore(ctx context.Context, d Destination, snapshot string, files []string, target string) error
}

// Claims reports whether a destination claims a path: the path must live under
// one of its roots. Claiming is not the same as holding the file — that is what
// a drill proves.
func (d Destination) Claims(path string) bool {
	for _, r := range d.Roots {
		if underOrEqual(path, r) {
			return true
		}
	}
	return false
}

// ClaimedDepth is the length of the longest root that claims the path, so the
// most specific destination can be reported first.
func (d Destination) ClaimedDepth(path string) int {
	best := -1
	for _, r := range d.Roots {
		if underOrEqual(path, r) && len(r) > best {
			best = len(r)
		}
	}
	return best
}

func underOrEqual(path, root string) bool {
	path, root = clean(path), clean(root)
	if path == root || root == "/" {
		return true
	}
	return strings.HasPrefix(path, root+"/")
}

func clean(p string) string {
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	return p
}

// SortSnapshots orders snapshots oldest first.
func SortSnapshots(s []Snapshot) {
	sort.SliceStable(s, func(i, j int) bool { return s[i].Time.Before(s[j].Time) })
}

// Newest returns the newest snapshot, or false when there is none.
func Newest(s []Snapshot) (Snapshot, bool) {
	if len(s) == 0 {
		return Snapshot{}, false
	}
	best := s[0]
	for _, c := range s[1:] {
		if c.Time.After(best.Time) {
			best = c
		}
	}
	return best, true
}

// Walker is implemented by backends that can stream the contents of a snapshot,
// so a drill can sample from a million files without holding them in memory.
type Walker interface {
	Walk(ctx context.Context, d Destination, snapshot, path string, fn func(File) error) error
}
