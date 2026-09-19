// Package resticbk reads restic repositories: what they cover, when they last
// ran, what a snapshot holds, and a restore into a directory of our choosing.
package resticbk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/morass/restorable/internal/backend"
	"github.com/morass/restorable/internal/run"
)

// Repo is one configured restic repository.
type Repo struct {
	Name string // label the user gave it
	Repo string // -r value: a path, or any restic location
	// PasswordCommand is the user's own command that prints the passphrase. It
	// is handed to restic as RESTIC_PASSWORD_COMMAND and never stored by us.
	PasswordCommand string
}

// Backend reads a set of restic repositories.
type Backend struct {
	Repos  []Repo
	Runner run.Runner
}

func (b *Backend) Kind() backend.Kind { return backend.Restic }

func (b *Backend) Installed() bool { return run.Restic.Available() }

func (b *Backend) env(r Repo) []string {
	env := []string{"RESTIC_REPOSITORY=" + r.Repo}
	if r.PasswordCommand != "" {
		env = append(env, "RESTIC_PASSWORD_COMMAND="+r.PasswordCommand)
	}
	return env
}

func (b *Backend) runner(r Repo) run.Runner {
	rr := b.Runner
	rr.Env = append(append([]string{}, b.Runner.Env...), b.env(r)...)
	return rr
}

type snapJSON struct {
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
	Time     time.Time `json:"time"`
	Paths    []string  `json:"paths"`
	Hostname string    `json:"hostname"`
}

// Destinations lists every configured repository with what it covers, taken
// from the paths its snapshots recorded.
func (b *Backend) Destinations(ctx context.Context) ([]backend.Destination, error) {
	var out []backend.Destination
	for _, r := range b.Repos {
		d := backend.Destination{Backend: backend.Restic, ID: r.Repo, Label: r.Name}
		snaps, err := b.snapshots(ctx, r)
		switch {
		case err != nil:
			d.State, d.Err = classify(err)
		default:
			d.State = backend.StateOK
			d.Snapshots = len(snaps)
			if n, ok := backend.Newest(snaps); ok {
				d.LastOK = n.Time
				d.LastOKSource = "restic snapshots"
			}
			d.Roots = rootsOf(snaps)
		}
		out = append(out, d)
	}
	return out, nil
}

// Snapshots lists the recovery points in a destination, oldest first.
func (b *Backend) Snapshots(ctx context.Context, d backend.Destination) ([]backend.Snapshot, error) {
	r, ok := b.repo(d)
	if !ok {
		return nil, fmt.Errorf("restic: no configured repository for %q", d.ID)
	}
	return b.snapshots(ctx, r)
}

func (b *Backend) repo(d backend.Destination) (Repo, bool) {
	for _, r := range b.Repos {
		if r.Repo == d.ID {
			return r, true
		}
	}
	return Repo{}, false
}

func (b *Backend) snapshots(ctx context.Context, r Repo) ([]backend.Snapshot, error) {
	res, err := b.runner(r).Run(ctx, run.Restic, "--json", "--no-lock", "snapshots")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("restic snapshots (exit %d): %s", res.ExitCode, firstLine(res.Stderr))
	}
	var raw []snapJSON
	if err := json.Unmarshal(res.Stdout, &raw); err != nil {
		return nil, fmt.Errorf("restic snapshots: unreadable answer: %w", err)
	}
	snaps := make([]backend.Snapshot, 0, len(raw))
	for _, s := range raw {
		id := s.ShortID
		if id == "" {
			id = s.ID
		}
		snaps = append(snaps, backend.Snapshot{ID: id, Time: s.Time, Paths: s.Paths, Host: s.Hostname})
	}
	backend.SortSnapshots(snaps)
	return snaps, nil
}

// Excluded cannot be answered from a restic repository: restic stores the files
// it took, not the rules it was given. Coverage verifies by listing instead.
func (b *Backend) Excluded(context.Context, backend.Destination, []string) ([]backend.Exclusion, error) {
	return nil, backend.ErrUnsupported
}

type lsNode struct {
	MessageType string `json:"message_type"`
	StructType  string `json:"struct_type"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Path        string `json:"path"`
	Size        int64  `json:"size"`
}

// List returns what a snapshot holds under one path.
func (b *Backend) List(ctx context.Context, d backend.Destination, snapshot, path string) ([]backend.File, error) {
	r, ok := b.repo(d)
	if !ok {
		return nil, fmt.Errorf("restic: no configured repository for %q", d.ID)
	}
	args := []string{"--json", "--no-lock", "ls", snapshot}
	if path != "" {
		args = append(args, path)
	}
	res, err := b.runner(r).Run(ctx, run.Restic, args...)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("restic ls (exit %d): %s", res.ExitCode, firstLine(res.Stderr))
	}
	var files []backend.File
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var n lsNode
		if err := json.Unmarshal([]byte(line), &n); err != nil {
			continue // restic mixes a snapshot header in; skip what is not a node
		}
		if n.StructType == "snapshot" || n.MessageType == "snapshot" || n.Path == "" {
			continue
		}
		files = append(files, backend.File{Path: n.Path, Size: n.Size, Dir: n.Type == "dir"})
	}
	return files, nil
}

// Restore writes the named files from a snapshot under target.
func (b *Backend) Restore(ctx context.Context, d backend.Destination, snapshot string, files []string, target string) error {
	r, ok := b.repo(d)
	if !ok {
		return fmt.Errorf("restic: no configured repository for %q", d.ID)
	}
	if len(files) == 0 {
		return fmt.Errorf("restic restore: no files asked for")
	}
	args := []string{"--json", "--no-lock", "restore", snapshot, "--target", target}
	for _, f := range files {
		args = append(args, "--include", f)
	}
	rr := b.runner(r)
	if rr.Timeout == 0 {
		rr.Timeout = 10 * time.Minute // a restore is allowed to take longer than a listing
	}
	res, err := rr.Run(ctx, run.Restic, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("restic restore (exit %d): %s", res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

func rootsOf(snaps []backend.Snapshot) []string {
	seen := map[string]bool{}
	var roots []string
	for _, s := range snaps {
		for _, p := range s.Paths {
			if !seen[p] {
				seen[p] = true
				roots = append(roots, p)
			}
		}
	}
	return roots
}

// classify turns a restic failure into a state the report can explain, without
// echoing the whole error at the user.
func classify(err error) (backend.State, string) {
	msg := err.Error()
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "wrong password"), strings.Contains(low, "no key could be found"),
		strings.Contains(low, "empty password"), strings.Contains(low, "password_command"):
		return backend.StateLocked, "the repository needs a passphrase this session does not have"
	case strings.Contains(low, "unable to open config file"), strings.Contains(low, "no such file or directory"),
		strings.Contains(low, "does not exist"), strings.Contains(low, "stat "):
		return backend.StateUnreachable, "the repository is not there (disk not mounted, or the path is wrong)"
	case strings.Contains(low, "not installed"):
		return backend.StateError, "restic is not installed"
	}
	return backend.StateError, firstLineStr(msg)
}

func firstLine(b []byte) string { return firstLineStr(string(b)) }

func firstLineStr(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// Walk streams what a snapshot holds, one entry at a time.
func (b *Backend) Walk(ctx context.Context, d backend.Destination, snapshot, path string, fn func(backend.File) error) error {
	r, ok := b.repo(d)
	if !ok {
		return fmt.Errorf("restic: no configured repository for %q", d.ID)
	}
	args := []string{"--json", "--no-lock", "ls", snapshot}
	if path != "" {
		args = append(args, path)
	}
	return b.runner(r).Stream(ctx, run.Restic, args, func(line []byte) error {
		var n lsNode
		if err := json.Unmarshal(line, &n); err != nil {
			return nil
		}
		if n.StructType == "snapshot" || n.MessageType == "snapshot" || n.Path == "" {
			return nil
		}
		return fn(backend.File{Path: n.Path, Size: n.Size, Dir: n.Type == "dir"})
	})
}
