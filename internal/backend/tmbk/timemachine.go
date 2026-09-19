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
	"github.com/morass/restorable/internal/redact"
	"github.com/morass/restorable/internal/run"
	"github.com/morass/restorable/internal/safe"
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
			Mount:   d.MountPoint,
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
			dest.Connected = true
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
	if err := safe.Argument("mount point", mount); err != nil {
		return time.Time{}, 0, false
	}
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

// Snapshots lists the backups of one mounted destination, oldest first. The
// destination's own mount point is used, so a machine with two backup disks
// cannot have one disk's backups reported as the other's.
func (b *Backend) Snapshots(ctx context.Context, d backend.Destination) ([]backend.Snapshot, error) {
	args := []string{"listbackups", "-m"}
	switch {
	case b.backupRoot() != "":
		args = append(args, "-d", b.backupRoot())
	case d.Mount != "":
		// The mount point comes out of tmutil's own answer; it is still checked,
		// because an argument that starts with '-' would be read as an option.
		if err := safe.Argument("mount point", d.Mount); err != nil {
			return nil, err
		}
		args = append(args, "-d", d.Mount)
	case !d.Connected:
		return nil, fmt.Errorf("%s is not mounted, so its backups cannot be listed", d.Label)
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
	for _, p := range paths {
		if strings.ContainsAny(p, "\n\x00") {
			// tmutil answers one line per path; a name with a newline in it makes
			// the answers impossible to tell apart, so it is not asked about.
			return nil, fmt.Errorf("%q holds a newline, so Time Machine cannot be asked about it", p)
		}
	}
	// "--" so a path can never be read as an option, whatever it is called.
	args := append([]string{"isexcluded", "--"}, paths...)
	res, err := b.Runner.Run(ctx, run.Tmutil, args...)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("tmutil isexcluded (exit %d): %s", res.ExitCode, firstLine(res.Stderr))
	}
	lines := strings.Split(strings.TrimRight(string(res.Stdout), "\n"), "\n")
	if len(lines) != len(paths) {
		// A name holding a newline splits tmutil's answer, and matching answers
		// to paths by position would then attach one path's verdict to another.
		return nil, fmt.Errorf("tmutil isexcluded answered with %d lines for %d paths", len(lines), len(paths))
	}
	out := make([]backend.Exclusion, len(paths))
	for i, line := range lines {
		verdict, answered, ok := strings.Cut(line, "]")
		if !ok {
			return nil, fmt.Errorf("tmutil isexcluded: unexpected answer %q", line)
		}
		// tmutil echoes the path it judged (resolved), so the answer is checked
		// against the path that was asked about rather than trusted by position.
		answered = strings.TrimSpace(answered)
		if answered != "" && !samePath(answered, paths[i]) {
			return nil, fmt.Errorf("tmutil isexcluded answered about %q when asked about %q", answered, paths[i])
		}
		switch verdict + "]" {
		case "[Excluded]":
			out[i] = backend.Exclusion{Excluded: true, Reason: b.reason(paths[i])}
		case "[Included]":
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
	dir, err := backupPath(snapshot, path)
	if err != nil {
		return nil, err
	}
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
// absolute layout. Nothing is ever written towards the backup, and neither the
// read nor the write can leave its own directory: both go through an os.Root, so
// a symlink anywhere along the way — inside the backup or inside the target —
// cannot lead out of it.
func (b *Backend) Restore(_ context.Context, _ backend.Destination, snapshot string, files []string, target string) error {
	dstRoot, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer dstRoot.Close()

	for _, f := range files {
		src, err := backupPath(snapshot, f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		// The source is opened relative to the backup, not by absolute path.
		srcRoot, err := os.OpenRoot(snapshot)
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(snapshot, src)
		if relErr != nil {
			srcRoot.Close()
			return relErr
		}
		in, err := srcRoot.Open(rel)
		srcRoot.Close()
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}

		dstRel := strings.TrimPrefix(filepath.Clean(f), string(filepath.Separator))
		if _, err := safe.Join(target, f); err != nil {
			in.Close()
			return fmt.Errorf("restore %s: %w", f, err)
		}
		if err := mkdirAllIn(dstRoot, filepath.Dir(dstRel)); err != nil {
			in.Close()
			return fmt.Errorf("restore %s: %w", f, err)
		}
		out, err := dstRoot.OpenFile(dstRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			in.Close()
			return fmt.Errorf("restore %s: %w", f, err)
		}
		_, copyErr := io.Copy(out, in)
		in.Close()
		closeErr := out.Close()
		if copyErr != nil {
			return fmt.Errorf("restore %s: %w", f, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("restore %s: %w", f, closeErr)
		}
	}
	return nil
}

// mkdirAllIn creates a directory inside a root, one component at a time, so no
// part of the path can be followed out of it.
func mkdirAllIn(root *os.Root, dir string) error {
	if dir == "." || dir == "" || dir == string(filepath.Separator) {
		return nil
	}
	var built string
	for _, part := range strings.Split(filepath.Clean(dir), string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		built = filepath.Join(built, part)
		if err := root.Mkdir(built, 0o700); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

// backupPath joins a backup directory and an absolute path from the live disk.
// A Time Machine backup holds one directory per volume ("Macintosh HD"), and the
// live path hangs below that, so the volume has to be found rather than assumed.
func backupPath(snapshot, path string) (string, error) {
	for _, vol := range volumes(snapshot) {
		candidate, err := safe.Join(filepath.Join(snapshot, vol), path)
		if err != nil {
			return "", err
		}
		if _, err := os.Lstat(candidate); err == nil {
			return candidate, nil
		}
	}
	// No volume directory holds it: fall back to the backup root, which is what
	// a backup made of one volume with no wrapper looks like (and what tests use).
	return safe.Join(snapshot, path)
}

// volumes lists the volume directories inside a backup, newest layout first.
func volumes(snapshot string) []string {
	entries, err := os.ReadDir(snapshot)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out
}

// livePath turns a path inside a backup into the path it came from on the disk,
// dropping the volume directory Time Machine wraps each volume in.
func livePath(snapshot, inBackup string) string {
	rel := strings.TrimPrefix(strings.TrimPrefix(inBackup, snapshot), string(filepath.Separator))
	if rel == "" {
		return "/"
	}
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(parts) == 2 && looksLikeVolume(parts[0]) {
		return "/" + parts[1]
	}
	return "/" + rel
}

// looksLikeVolume reports whether a directory inside a backup is a volume
// wrapper rather than the start of a live path. Time Machine names it after the
// volume ("Macintosh HD", "Data"), and a live path always starts with a
// lowercase system directory or Users/Applications/Library/Volumes.
func looksLikeVolume(name string) bool {
	switch name {
	case "Users", "Applications", "Library", "System", "Volumes", "opt", "usr",
		"var", "private", "etc", "bin", "sbin", "tmp", "home", "Network", "cores":
		return false
	}
	return true
}

// samePath compares what tmutil answered about with what was asked, allowing for
// the /private prefix it resolves symlinked system directories to.
func samePath(answered, asked string) bool {
	a, b := filepath.Clean(answered), filepath.Clean(asked)
	return a == b || a == "/private"+b || "/private"+a == b
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
	s := redact.Secrets(strings.TrimSpace(string(b)))
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
func (b *Backend) Walk(ctx context.Context, _ backend.Destination, snapshot, path string, fn func(backend.File) error) error {
	root := snapshot
	if path != "" {
		var err error
		root, err = backupPath(snapshot, path)
		if err != nil {
			return err
		}
	}
	var unreadable int
	walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			unreadable++
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		return fn(backend.File{Path: filepath.Clean(livePath(snapshot, p)), Size: info.Size()})
	})
	if walkErr != nil {
		return walkErr
	}
	if unreadable > 0 {
		return fmt.Errorf("%d places inside the backup could not be read; the sample is not from the whole of it", unreadable)
	}
	return nil
}
