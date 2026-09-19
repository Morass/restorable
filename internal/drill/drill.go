// Package drill proves a backup can hand files back: it samples files from a
// snapshot, restores them into a temporary directory, and compares the bytes with
// what is on the live disk. Nothing is ever written towards the backup.
package drill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/morass/restorable/internal/backend"
)

// Status is what happened to one sampled file.
type Status string

const (
	// Match means the restored bytes are the bytes on disk.
	Match Status = "match"
	// Differs means the restored bytes are not the bytes on disk, and the live
	// file has not changed since the snapshot — the backup holds something else.
	Differs Status = "differs"
	// ChangedSince means the file was edited after the snapshot, so a difference
	// is expected and proves nothing either way.
	ChangedSince Status = "changed-since"
	// GoneLive means the file is no longer on disk; the backup still has it,
	// which is what a backup is for.
	GoneLive Status = "gone-from-disk"
	// NotRestored means the backup did not hand the file back at all.
	NotRestored Status = "not-restored"
	// Failed means the attempt itself failed (a read error, a tool error).
	Failed Status = "failed"
)

// Bad reports whether a status means the backup let the user down.
func (s Status) Bad() bool { return s == Differs || s == NotRestored || s == Failed }

// FileResult is one sampled file.
type FileResult struct {
	Path      string `json:"path"`
	Bytes     int64  `json:"bytes"`
	Status    Status `json:"status"`
	Detail    string `json:"detail,omitempty"`
	BackupSHA string `json:"backup_sha256,omitempty"`
	LiveSHA   string `json:"live_sha256,omitempty"`
}

// Receipt is the record of one drill, written to the state directory so a machine
// keeps a track record rather than a single good day.
type Receipt struct {
	Tool        string        `json:"tool"`
	Version     string        `json:"version"`
	RanAt       time.Time     `json:"ran_at"`
	Backend     backend.Kind  `json:"backend"`
	Destination string        `json:"destination"`
	Label       string        `json:"label,omitempty"`
	Snapshot    string        `json:"snapshot"`
	SnapshotAt  time.Time     `json:"snapshot_at"`
	Seed        int64         `json:"seed"`
	Asked       int           `json:"asked"`
	Files       []FileResult  `json:"files"`
	Duration    time.Duration `json:"duration"`
	Pass        bool          `json:"pass"`
	Note        string        `json:"note,omitempty"`
}

// Counts summarises a receipt.
func (r Receipt) Counts() map[Status]int {
	out := map[Status]int{}
	for _, f := range r.Files {
		out[f.Status]++
	}
	return out
}

// Options control one drill.
type Options struct {
	Count    int    // how many files to sample; default 12
	Seed     int64  // 0 asks the clock, so two drills sample differently
	MaxBytes int64  // skip files bigger than this; default 32 MiB
	Target   string // where to restore; default a temporary directory
	Keep     bool   // keep the restored copies instead of removing them
	Version  string // tool version, recorded in the receipt
	Progress func(string)
}

// DefaultCount and DefaultMaxBytes keep a drill quick enough to run often.
const (
	DefaultCount    = 12
	DefaultMaxBytes = 32 << 20
)

// Run drills one destination and returns the receipt.
func Run(ctx context.Context, b backend.Backend, d backend.Destination, snap backend.Snapshot, opts Options) (Receipt, error) {
	if opts.Count <= 0 {
		opts.Count = DefaultCount
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.Seed == 0 {
		opts.Seed = time.Now().UnixNano()
	}
	start := time.Now()
	rec := Receipt{
		Tool: "restorable", Version: opts.Version, RanAt: start,
		Backend: d.Backend, Destination: d.ID, Label: d.Label,
		Snapshot: snap.ID, SnapshotAt: snap.Time, Seed: opts.Seed, Asked: opts.Count,
	}

	walker, ok := b.(backend.Walker)
	if !ok {
		return rec, fmt.Errorf("%s cannot list a snapshot, so it cannot be drilled", d.Backend)
	}

	sample, total, err := reservoir(ctx, walker, d, snap, opts)
	if err != nil {
		return rec, err
	}
	if len(sample) == 0 {
		rec.Note = fmt.Sprintf("the snapshot holds no file under %d bytes to sample (%d entries seen)", opts.MaxBytes, total)
		rec.Pass = false
		rec.Duration = time.Since(start)
		return rec, nil
	}

	target := opts.Target
	if target == "" {
		target, err = os.MkdirTemp("", "restorable-drill-*")
		if err != nil {
			return rec, err
		}
		if !opts.Keep {
			defer os.RemoveAll(target)
		}
	} else if err := os.MkdirAll(target, 0o700); err != nil {
		return rec, err
	}
	if opts.Keep {
		rec.Note = "restored copies kept in " + target
	}

	paths := make([]string, 0, len(sample))
	for _, f := range sample {
		paths = append(paths, f.Path)
	}
	if opts.Progress != nil {
		opts.Progress(fmt.Sprintf("restoring %d files from %s", len(paths), snap.ID))
	}
	restoreErr := b.Restore(ctx, d, snap.ID, paths, target)

	for _, f := range sample {
		res := FileResult{Path: f.Path, Bytes: f.Size}
		restored := filepath.Join(target, strings.TrimPrefix(filepath.Clean(f.Path), "/"))
		bh, err := hashFile(restored)
		switch {
		case err != nil && restoreErr != nil:
			res.Status, res.Detail = NotRestored, shorten(restoreErr.Error())
		case errors.Is(err, os.ErrNotExist):
			res.Status, res.Detail = NotRestored, "the backup did not hand this file back"
		case err != nil:
			res.Status, res.Detail = Failed, shorten(err.Error())
		default:
			res.BackupSHA = bh
			res.Status, res.Detail, res.LiveSHA = compareLive(f, snap, bh)
		}
		rec.Files = append(rec.Files, res)
	}

	sort.SliceStable(rec.Files, func(i, j int) bool {
		if rec.Files[i].Status.Bad() != rec.Files[j].Status.Bad() {
			return rec.Files[i].Status.Bad()
		}
		return rec.Files[i].Path < rec.Files[j].Path
	})

	rec.Pass = true
	for _, f := range rec.Files {
		if f.Status.Bad() {
			rec.Pass = false
		}
	}
	rec.Duration = time.Since(start)
	return rec, nil
}

// compareLive compares a restored file with what is on the live disk, and is
// careful not to call an ordinary edit a backup failure.
func compareLive(f backend.File, snap backend.Snapshot, backupHash string) (Status, string, string) {
	info, err := os.Lstat(f.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return GoneLive, "the file is no longer on disk; the backup still has it", ""
	case err != nil:
		return GoneLive, shorten(err.Error()), ""
	case info.IsDir():
		return Failed, "a directory is on disk where the backup holds a file", ""
	}
	lh, err := hashFile(f.Path)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return GoneLive, "no permission to read the live file, so nothing was compared", ""
		}
		return Failed, shorten(err.Error()), ""
	}
	if lh == backupHash {
		return Match, "", lh
	}
	if !snap.Time.IsZero() && info.ModTime().After(snap.Time) {
		return ChangedSince, fmt.Sprintf("edited after the snapshot (%s)", info.ModTime().Format(time.RFC3339)), lh
	}
	return Differs, "the backup holds different bytes and the file has not been edited since", lh
}

// reservoir samples files from a snapshot in one pass, without holding the
// listing in memory. It returns the sample and how many files were seen.
func reservoir(ctx context.Context, w backend.Walker, d backend.Destination, snap backend.Snapshot, opts Options) ([]backend.File, int, error) {
	rnd := rand.New(rand.NewSource(opts.Seed))
	sample := make([]backend.File, 0, opts.Count)
	seen := 0
	err := w.Walk(ctx, d, snap.ID, "", func(f backend.File) error {
		if f.Dir || f.Size <= 0 || f.Size > opts.MaxBytes {
			return nil
		}
		seen++
		if len(sample) < opts.Count {
			sample = append(sample, f)
			return nil
		}
		if j := rnd.Intn(seen); j < opts.Count {
			sample[j] = f
		}
		return nil
	})
	if err != nil {
		return nil, seen, err
	}
	sort.SliceStable(sample, func(i, j int) bool { return sample[i].Path < sample[j].Path })
	return sample, seen, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func shorten(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 160 {
		return s[:160] + "…"
	}
	return s
}
