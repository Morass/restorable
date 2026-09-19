// Package tmbk reads Time Machine: which destinations are configured, when one
// last finished, what is excluded and why, and a restore that is a file copy out
// of a mounted backup.
package tmbk

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/morass/restorable/internal/backend"
	"github.com/morass/restorable/internal/plist"
	"github.com/morass/restorable/internal/run"
)

// PreferencesPath is where Time Machine records its destinations and the date
// each one last completed. It is readable on most Macs and root-only on some, so
// every use of it degrades instead of failing.
const PreferencesPath = "/Library/Preferences/com.apple.TimeMachine.plist"

// Backend reads Time Machine through tmutil and its preferences file.
type Backend struct {
	Runner run.Runner
	// Prefs overrides PreferencesPath in tests.
	Prefs string
	// BackupRoot overrides where a mounted destination's backups are found, for
	// tests and for a destination whose mount point tmutil cannot tell us.
	BackupRoot string
}

func (b *Backend) Kind() backend.Kind { return backend.TimeMachine }

func (b *Backend) Installed() bool { return run.Tmutil.Available() }

func (b *Backend) prefs() string {
	if b.Prefs != "" {
		return b.Prefs
	}
	return PreferencesPath
}

// destination is one entry of tmutil destinationinfo -X.
type destination struct {
	Name, Kind, ID, MountPoint string
}

// Destinations lists the configured Time Machine destinations. A Mac with none
// gets one row saying exactly that, because "no destination" is the answer people
// most often need to see.
func (b *Backend) Destinations(ctx context.Context) ([]backend.Destination, error) {
	res, err := b.Runner.Run(ctx, run.Tmutil, "destinationinfo", "-X")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return []backend.Destination{{
			Backend: backend.TimeMachine, ID: "", Label: "Time Machine",
			State: backend.StateError, Err: firstLine(res.Stderr),
		}}, nil
	}
	root, err := plist.Parse(res.Stdout)
	if err != nil {
		return nil, fmt.Errorf("tmutil destinationinfo: %w", err)
	}
	var infos []destination
	for _, d := range plist.Dicts(root, "Destinations") {
		infos = append(infos, destination{
			Name:       plist.Str(d, "Name"),
			Kind:       plist.Str(d, "Kind"),
			ID:         plist.Str(d, "ID"),
			MountPoint: plist.Str(d, "MountPoint"),
		})
	}
	if len(infos) == 0 {
		return []backend.Destination{{
			Backend: backend.TimeMachine, Label: "Time Machine",
			State:         backend.StateUnreachable,
			NotConfigured: true,
			Err:           "no Time Machine destination is configured on this Mac",
		}}, nil
	}

	prefs := b.readPrefs(ctx)
	out := make([]backend.Destination, 0, len(infos))
	for _, d := range infos {
		dest := backend.Destination{
			Backend: backend.TimeMachine,
			ID:      d.ID,
			Label:   strings.TrimSpace(d.Name),
			Roots:   []string{"/"}, // Time Machine covers the volumes it is not told to skip
			State:   backend.StateOK,
		}
		if dest.Label == "" {
			dest.Label = d.Kind
		}
		b.fillLast(ctx, &dest, d.MountPoint, prefs)
		out = append(out, dest)
	}
	return out, nil
}

// fillLast dates a destination from the most trustworthy source available: the
// mounted destination itself, then Time Machine's own preferences, then the local
// APFS snapshots, which only prove that Time Machine ran at all.
func (b *Backend) fillLast(ctx context.Context, dest *backend.Destination, mount string, prefs plist.Value) {
	if mount != "" {
		if t, n, ok := b.latestOnDisk(ctx, mount); ok {
			dest.LastOK, dest.LastOKSource, dest.Snapshots = t, "the mounted destination", n
			return
		}
	}
	if prefs != nil {
		for _, p := range plist.Dicts(prefs, "Destinations") {
			if !sameID(plist.Str(p, "DestinationID"), dest.ID) {
				continue
			}
			dates := plist.Times(p, "SnapshotDates")
			if len(dates) > 0 {
				dest.Snapshots = len(dates)
			}
			if t, ok := plist.Time(p, "BACKUP_COMPLETED_DATE"); ok {
				dest.LastOK, dest.LastOKSource = t, "Time Machine's preferences"
				return
			}
			if len(dates) > 0 {
				newest := dates[0]
				for _, d := range dates[1:] {
					if d.After(newest) {
						newest = d
					}
				}
				dest.LastOK, dest.LastOKSource = newest, "Time Machine's preferences"
				return
			}
		}
	}
	if t, ok := b.latestLocalSnapshot(ctx); ok {
		dest.LastOK, dest.LastOKSource = t, "a local snapshot (the destination was not reachable)"
		return
	}
	dest.State = backend.StateUnreachable
	dest.Err = "the destination is not mounted and no date could be read, so the last backup is unknown"
	if !b.prefsReadable(ctx) {
		dest.Err += " (Time Machine's own record is behind Full Disk Access)"
	}
}

// readPrefs reads Time Machine's own preferences, which hold the last completed
// date even when the disk is elsewhere. It is root-only on some Macs, so a
// failure here is silent and the caller falls back.
func (b *Backend) readPrefs(ctx context.Context) plist.Value {
	v, err := plist.FromFile(ctx, b.Runner, b.prefs())
	if err != nil {
		return nil
	}
	return v
}

// prefsReadable reports whether Time Machine's preferences could be read at all,
// so the report can say that the date is behind Full Disk Access rather than
// leaving the user guessing.
func (b *Backend) prefsReadable(ctx context.Context) bool {
	return b.readPrefs(ctx) != nil
}

// latestOnDisk reads the backups on a mounted destination.
func (b *Backend) latestOnDisk(ctx context.Context, mount string) (time.Time, int, bool) {
	res, err := b.Runner.Run(ctx, run.Tmutil, "listbackups", "-m", "-d", mount)
	if err != nil || res.ExitCode != 0 {
		return time.Time{}, 0, false
	}
	var newest time.Time
	count := 0
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		count++
		if t, ok := parseBackupName(line); ok && t.After(newest) {
			newest = t
		}
	}
	if count == 0 || newest.IsZero() {
		return time.Time{}, count, false
	}
	return newest, count, true
}

func (b *Backend) latestLocalSnapshot(ctx context.Context) (time.Time, bool) {
	res, err := b.Runner.Run(ctx, run.Tmutil, "listlocalsnapshotdates")
	if err != nil || res.ExitCode != 0 {
		return time.Time{}, false
	}
	var newest time.Time
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if t, ok := parseSnapshotDate(line); ok && t.After(newest) {
			newest = t
		}
	}
	return newest, !newest.IsZero()
}

// Snapshots lists the backups of a mounted destination, oldest first.
func (b *Backend) Snapshots(ctx context.Context, d backend.Destination) ([]backend.Snapshot, error) {
	args := []string{"listbackups", "-m"}
	if root := b.backupRoot(); root != "" {
		args = append(args, "-d", root)
	}
	res, err := b.Runner.Run(ctx, run.Tmutil, args...)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("tmutil listbackups: %s", firstLine(res.Stderr))
	}
	var snaps []backend.Snapshot
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		t, _ := parseBackupName(line)
		snaps = append(snaps, backend.Snapshot{ID: line, Time: t, Paths: []string{"/"}})
	}
	backend.SortSnapshots(snaps)
	return snaps, nil
}

func (b *Backend) backupRoot() string { return b.BackupRoot }

// Excluded asks Time Machine itself whether paths are kept out, in one call, and
// then looks for the reason. tmutil's answer is authoritative; the reason is best
// effort, because macOS does not expose its standard exclusions.
func (b *Backend) Excluded(ctx context.Context, _ backend.Destination, paths []string) ([]backend.Exclusion, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := append([]string{"isexcluded"}, paths...)
	res, err := b.Runner.Run(ctx, run.Tmutil, args...)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	out := make([]backend.Exclusion, len(paths))
	for i := range paths {
		if i >= len(lines) {
			return nil, fmt.Errorf("tmutil isexcluded answered about %d of %d paths", len(lines), len(paths))
		}
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "[Excluded]"):
			out[i] = backend.Exclusion{Excluded: true, Reason: b.reason(paths[i])}
		case strings.HasPrefix(line, "[Included]"):
			out[i] = backend.Exclusion{Excluded: false}
		default:
			return nil, fmt.Errorf("tmutil isexcluded: unexpected answer %q", line)
		}
	}
	return out, nil
}

// reason names the mechanism that excluded a path, when it can be established
// from the item itself.
func (b *Backend) reason(path string) string {
	if hasExcludeAttr(path) {
		return "excluded by a sticky flag on the item itself (xattr)"
	}
	if p := stdExclusionReason(path); p != "" {
		return p
	}
	return "excluded by Time Machine (macOS does not say which rule)"
}

const excludeAttr = "com.apple.metadata:com_apple_backup_excludeItem"

func hasExcludeAttr(path string) bool {
	buf := make([]byte, 1)
	_, err := getxattr(path, excludeAttr, buf)
	return err == nil
}

// stdExclusionReason recognises the exclusions every Mac has, so the common case
// gets a real explanation rather than a shrug.
func stdExclusionReason(path string) string {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)
	switch {
	case strings.HasSuffix(clean, "/Library/Caches"), strings.Contains(clean, "/Library/Caches/"):
		return "a cache directory, which Time Machine never backs up"
	case base == ".Trash", strings.Contains(clean, "/.Trash/"):
		return "the Trash, which Time Machine never backs up"
	case clean == "/private/tmp" || strings.HasPrefix(clean, "/private/tmp/") ||
		clean == "/tmp" || strings.HasPrefix(clean, "/tmp/"):
		return "a temporary directory, which Time Machine never backs up"
	case clean == "/private/var/vm" || strings.HasPrefix(clean, "/private/var/vm/"):
		return "swap and sleep image files, which Time Machine never backs up"
	case strings.HasSuffix(clean, ".nosync"), strings.Contains(clean, ".nosync/"):
		return "a .nosync directory, which Time Machine never backs up"
	}
	return ""
}

// List returns what a backup holds under one path. A Time Machine backup is an
// ordinary directory tree, so this is a directory read.
func (b *Backend) List(_ context.Context, _ backend.Destination, snapshot, path string) ([]backend.File, error) {
	dir := backupPath(snapshot, path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]backend.File, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, backend.File{
			Path: filepath.Join(path, e.Name()),
			Size: info.Size(),
			Dir:  e.IsDir(),
		})
	}
	return out, nil
}

// Restore copies files out of a mounted backup into target, keeping their
// absolute layout. Nothing is ever written towards the backup.
func (b *Backend) Restore(_ context.Context, _ backend.Destination, snapshot string, files []string, target string) error {
	for _, f := range files {
		src := backupPath(snapshot, f)
		dst := filepath.Join(target, strings.TrimPrefix(filepath.Clean(f), "/"))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("restore %s: %w", f, err)
		}
	}
	return nil
}

// backupPath joins a backup directory and an absolute path from the live disk.
// Time Machine stores each volume under the backup, so the volume directory is
// part of the snapshot id we were given.
func backupPath(snapshot, path string) string {
	return filepath.Join(snapshot, strings.TrimPrefix(filepath.Clean(path), "/"))
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func sameID(a, b string) bool {
	return strings.EqualFold(strings.Trim(a, "{}"), strings.Trim(b, "{}"))
}

// parseBackupName reads the date out of a backup directory name, which looks
// like .../2026-09-18-174501.backup or .../2026-09-18-174501.previous.
func parseBackupName(p string) (time.Time, bool) {
	base := filepath.Base(strings.TrimRight(p, "/"))
	if i := strings.IndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	t, err := time.ParseInLocation("2006-01-02-150405", base, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// parseSnapshotDate reads a line of tmutil listlocalsnapshotdates.
func parseSnapshotDate(line string) (time.Time, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasSuffix(line, ":") {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02-150405", line, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// Walk streams what a backup holds under a path: a Time Machine backup is a
// directory tree, so this is a filesystem walk that never leaves it.
func (b *Backend) Walk(_ context.Context, _ backend.Destination, snapshot, path string, fn func(backend.File) error) error {
	root := backupPath(snapshot, path)
	return filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		live := "/" + strings.TrimPrefix(strings.TrimPrefix(p, snapshot), "/")
		if d.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return fn(backend.File{Path: filepath.Clean(live), Size: info.Size()})
	})
}
