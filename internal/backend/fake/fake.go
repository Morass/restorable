// Package fake is a backup destination that exists only in a test: it holds
// bytes in memory, claims paths it is told to claim, and can be made to fail.
package fake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/morass/restorable/internal/backend"
)

// Backend is a backup system made of maps.
type Backend struct {
	Name backend.Kind
	// Dest is the destination this backend offers.
	Dest backend.Destination
	// Snaps are its snapshots, oldest first.
	Snaps []backend.Snapshot
	// Contents maps a snapshot id to the files it holds: path -> bytes.
	Contents map[string]map[string][]byte
	// Excludes maps a path to the reason it is excluded.
	Excludes map[string]string
	// ExcludeErr, when set, is returned by every exclusion question.
	ExcludeErr error
	// Unsupported makes exclusion questions unanswerable, like restic.
	Unsupported bool
	// RestoreErr, when set, fails every restore.
	RestoreErr error
	// SkipRestore lists paths the restore silently does not write.
	SkipRestore map[string]bool
	// Calls counts what was asked, so a test can prove questions are batched.
	Calls struct {
		Excluded  int
		Paths     int
		Restores  int
		WalkCalls int
	}
}

// New makes a backend that claims roots and holds one snapshot of files.
func New(kind backend.Kind, label string, roots []string, snapshot string, files map[string][]byte) *Backend {
	return &Backend{
		Name: kind,
		Dest: backend.Destination{
			Backend: kind, ID: "fake:" + label, Label: label,
			Roots: roots, State: backend.StateOK, Snapshots: 1,
		},
		Snaps:    []backend.Snapshot{{ID: snapshot, Paths: roots}},
		Contents: map[string]map[string][]byte{snapshot: files},
		Excludes: map[string]string{},
	}
}

func (b *Backend) Kind() backend.Kind { return b.Name }
func (b *Backend) Installed() bool    { return true }

func (b *Backend) Destinations(context.Context) ([]backend.Destination, error) {
	return []backend.Destination{b.Dest}, nil
}

func (b *Backend) Snapshots(context.Context, backend.Destination) ([]backend.Snapshot, error) {
	return b.Snaps, nil
}

func (b *Backend) Excluded(_ context.Context, _ backend.Destination, paths []string) ([]backend.Exclusion, error) {
	b.Calls.Excluded++
	b.Calls.Paths += len(paths)
	if b.Unsupported {
		return nil, backend.ErrUnsupported
	}
	if b.ExcludeErr != nil {
		return nil, b.ExcludeErr
	}
	out := make([]backend.Exclusion, len(paths))
	for i, p := range paths {
		if reason, ok := b.match(p); ok {
			out[i] = backend.Exclusion{Excluded: true, Reason: reason}
		}
	}
	return out, nil
}

// match treats an excluded directory as excluding everything under it.
func (b *Backend) match(path string) (string, bool) {
	clean := filepath.Clean(path)
	for pattern, reason := range b.Excludes {
		p := filepath.Clean(pattern)
		if clean == p || strings.HasPrefix(clean, p+string(filepath.Separator)) {
			return reason, true
		}
	}
	return "", false
}

func (b *Backend) List(_ context.Context, _ backend.Destination, snapshot, path string) ([]backend.File, error) {
	files, ok := b.Contents[snapshot]
	if !ok {
		return nil, fmt.Errorf("fake: no snapshot %q", snapshot)
	}
	var out []backend.File
	for p, data := range files {
		if path != "" && !strings.HasPrefix(p, path) {
			continue
		}
		out = append(out, backend.File{Path: p, Size: int64(len(data))})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (b *Backend) Walk(_ context.Context, _ backend.Destination, snapshot, path string, fn func(backend.File) error) error {
	b.Calls.WalkCalls++
	files, ok := b.Contents[snapshot]
	if !ok {
		return fmt.Errorf("fake: no snapshot %q", snapshot)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if path != "" && !strings.HasPrefix(p, path) {
			continue
		}
		if err := fn(backend.File{Path: p, Size: int64(len(files[p]))}); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) Restore(_ context.Context, _ backend.Destination, snapshot string, files []string, target string) error {
	b.Calls.Restores++
	if b.RestoreErr != nil {
		return b.RestoreErr
	}
	content, ok := b.Contents[snapshot]
	if !ok {
		return fmt.Errorf("fake: no snapshot %q", snapshot)
	}
	for _, f := range files {
		if b.SkipRestore[f] {
			continue
		}
		data, ok := content[f]
		if !ok {
			continue
		}
		dst := filepath.Join(target, strings.TrimPrefix(filepath.Clean(f), "/"))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}
