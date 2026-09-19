// Package coverage answers the first question: what on this machine is protected
// by nothing. It walks the roots once, asks every backup destination whether it
// claims and keeps each directory, and reports the largest holes.
package coverage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/morass/restorable/internal/backend"
)

// Protector is one destination seen as a yes/no answer about paths.
type Protector struct {
	Dest    backend.Destination
	Backend backend.Backend

	excluded map[string]backend.Exclusion
}

// Label names the protector in a report.
func (p *Protector) Label() string {
	if p.Dest.Label != "" {
		return string(p.Dest.Backend) + " (" + p.Dest.Label + ")"
	}
	return string(p.Dest.Backend)
}

// Kind is the class of finding.
type Kind string

const (
	// NoBackup means no destination claims the path at all.
	NoBackup Kind = "no-backup"
	// Excluded means a destination claims the path but keeps it out.
	Excluded Kind = "excluded"
	// Unreadable means restorable could not look inside.
	Unreadable Kind = "unreadable"
	// Skipped means restorable did not look on purpose.
	Skipped Kind = "skipped"
)

// Finding is one hole, or one place the walk stopped.
type Finding struct {
	Path   string `json:"path"`
	Kind   Kind   `json:"kind"`
	Detail string `json:"detail,omitempty"`
	Bytes  int64  `json:"bytes"`
	Files  int    `json:"files"`
}

// Report is the whole answer.
type Report struct {
	GeneratedAt  time.Time             `json:"generated_at"`
	Roots        []string              `json:"roots"`
	Destinations []backend.Destination `json:"destinations"`
	Findings     []Finding             `json:"findings"`

	TotalBytes       int64 `json:"total_bytes"`
	TotalFiles       int   `json:"total_files"`
	UnprotectedBytes int64 `json:"unprotected_bytes"`
	UnprotectedFiles int   `json:"unprotected_files"`

	Dirs     int           `json:"dirs_visited"`
	Duration time.Duration `json:"duration"`
	// CheckedFiles says whether individual files were asked about, or only
	// directories, because that changes what the report can promise.
	CheckedFiles bool `json:"checked_files"`
}

// Options control the walk.
type Options struct {
	Roots    []string
	MaxDepth int   // how deep to look for holes inside protected trees; 0 = 8
	MinBytes int64 // holes smaller than this are counted but not listed
	// CheckFiles asks about individual files as well as directories. Slower, and
	// only useful when someone excluded single files by hand.
	CheckFiles bool
	// AllLocations includes the folders macOS guards behind a permission prompt.
	AllLocations bool
	// CrossFilesystems follows the walk onto other mounted volumes.
	CrossFilesystems bool
	// Progress, when set, is called with each directory as it is entered.
	Progress func(path string)
}

// DefaultMaxDepth is how deep the walk looks for holes inside a protected tree.
const DefaultMaxDepth = 8

// protectedLocations are the folders that make macOS ask the user for permission.
// Walking them from a terminal raises a dialog, so they are skipped by default
// and named in the report rather than silently missed.
var protectedLocations = []string{
	"Library/Photos", "Pictures/Photos Library.photoslibrary", "Library/Mail",
	"Library/Messages", "Library/Containers/com.apple.mail",
	"Library/Application Support/AddressBook", "Library/Calendars",
	"Library/Suggestions", "Library/Safari", "Library/CoreFollowUp",
	"Library/Accounts", "Library/HomeKit", "Library/IdentityServices",
	"Library/Metadata/CoreSpotlight", "Library/Sharing", "Library/Trial",
}

// Run walks the roots and reports the holes.
func Run(ctx context.Context, opts Options, protectors []*Protector) (Report, error) {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultMaxDepth
	}
	for _, p := range protectors {
		if p.excluded == nil {
			p.excluded = map[string]backend.Exclusion{}
		}
	}
	start := time.Now()
	rep := Report{GeneratedAt: start, Roots: opts.Roots, CheckedFiles: opts.CheckFiles}
	for _, p := range protectors {
		rep.Destinations = append(rep.Destinations, p.Dest)
	}
	w := &walker{opts: opts, protectors: protectors, rep: &rep}

	home, _ := os.UserHomeDir()
	w.home = home

	for _, root := range opts.Roots {
		info, err := os.Lstat(root)
		if err != nil {
			rep.Findings = append(rep.Findings, Finding{Path: root, Kind: Unreadable, Detail: readableError(err)})
			continue
		}
		if !info.IsDir() {
			return rep, fmt.Errorf("%s is not a directory", root)
		}
		w.rootDev, _ = deviceOf(info)
		b, f, err := w.visit(ctx, root, 0)
		if err != nil {
			return rep, err
		}
		rep.TotalBytes += b
		rep.TotalFiles += f
	}
	sort.SliceStable(rep.Findings, func(i, j int) bool {
		if rep.Findings[i].Bytes != rep.Findings[j].Bytes {
			return rep.Findings[i].Bytes > rep.Findings[j].Bytes
		}
		return rep.Findings[i].Path < rep.Findings[j].Path
	})
	rep.Duration = time.Since(start)
	return rep, nil
}

type walker struct {
	opts       Options
	protectors []*Protector
	rep        *Report
	home       string
	rootDev    uint64
}

// visit decides what to do with one directory: walk it when a backup keeps it,
// walk it anyway when a backup keeps something inside it, and otherwise report it
// as a hole and count what it holds.
func (w *walker) visit(ctx context.Context, path string, depth int) (int64, int, error) {
	state, detail := w.statusOf(ctx, path)
	if state == protectedYes {
		if depth >= w.opts.MaxDepth {
			b, f := w.rollup(path)
			return b, f, nil
		}
		return w.dir(ctx, path, depth)
	}
	// Nothing keeps this directory itself, but a destination may keep something
	// deeper: descend so the hole is reported where it really is.
	if w.claimsBelow(path) && depth < w.opts.MaxDepth {
		return w.dir(ctx, path, depth)
	}
	b, f := w.rollup(path)
	kind := NoBackup
	if state == protectedExcluded {
		kind = Excluded
	}
	w.addFinding(Finding{Path: path, Kind: kind, Detail: detail, Bytes: b, Files: f}, true)
	return b, f, nil
}

// claimsBelow reports whether any readable destination covers something strictly
// inside this directory.
func (w *walker) claimsBelow(path string) bool {
	prefix := strings.TrimSuffix(filepath.Clean(path), "/") + "/"
	for _, p := range w.protectors {
		if p.Dest.State != backend.StateOK {
			continue
		}
		for _, r := range p.Dest.Roots {
			if strings.HasPrefix(filepath.Clean(r)+"/", prefix) && filepath.Clean(r) != filepath.Clean(path) {
				return true
			}
		}
	}
	return false
}

type protection int

const (
	protectedYes protection = iota
	protectedNo
	protectedExcluded
)

// statusOf decides whether a path is protected, and when it is not, why.
func (w *walker) statusOf(ctx context.Context, path string) (protection, string) {
	var claimed []*Protector
	for _, p := range w.protectors {
		if p.Dest.State != backend.StateOK {
			continue
		}
		if p.Dest.Claims(path) {
			claimed = append(claimed, p)
		}
	}
	if len(claimed) == 0 {
		return protectedNo, w.noBackupDetail()
	}
	var reasons []string
	for _, p := range claimed {
		ex, err := w.exclusion(ctx, p, path)
		if err != nil {
			// An exclusion question that cannot be answered is reported as
			// protected: claiming otherwise would invent a hole.
			return protectedYes, ""
		}
		if !ex.Excluded {
			return protectedYes, ""
		}
		reason := ex.Reason
		if reason == "" {
			reason = "excluded"
		}
		reasons = append(reasons, p.Label()+": "+reason)
	}
	return protectedExcluded, strings.Join(reasons, "; ")
}

func (w *walker) noBackupDetail() string {
	live := 0
	for _, p := range w.protectors {
		if p.Dest.State == backend.StateOK {
			live++
		}
	}
	if live == 0 {
		return "no backup destination could be read on this machine"
	}
	return "no backup destination covers this path"
}

// exclusion asks one protector about one path, remembering the answer.
func (w *walker) exclusion(ctx context.Context, p *Protector, path string) (backend.Exclusion, error) {
	if ex, ok := p.excluded[path]; ok {
		return ex, nil
	}
	answers, err := p.Backend.Excluded(ctx, p.Dest, []string{path})
	if err != nil {
		if errors.Is(err, backend.ErrUnsupported) {
			ex := backend.Exclusion{}
			p.excluded[path] = ex
			return ex, nil
		}
		return backend.Exclusion{}, err
	}
	if len(answers) != 1 {
		return backend.Exclusion{}, fmt.Errorf("%s answered about %d paths, asked about 1", p.Label(), len(answers))
	}
	p.excluded[path] = answers[0]
	return answers[0], nil
}

// prefetch asks every protector about a whole directory's children in one call.
func (w *walker) prefetch(ctx context.Context, paths []string) {
	for _, p := range w.protectors {
		if p.Dest.State != backend.StateOK {
			continue
		}
		var missing []string
		for _, path := range paths {
			if _, ok := p.excluded[path]; !ok && p.Dest.Claims(path) {
				missing = append(missing, path)
			}
		}
		if len(missing) == 0 {
			continue
		}
		answers, err := p.Backend.Excluded(ctx, p.Dest, missing)
		if err != nil || len(answers) != len(missing) {
			continue // fall back to one question per path
		}
		for i, path := range missing {
			p.excluded[path] = answers[i]
		}
	}
}

// dir walks one protected directory, counting what it holds and reporting the
// holes inside it. It returns the bytes and files below path.
func (w *walker) dir(ctx context.Context, path string, depth int) (int64, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if w.opts.Progress != nil {
		w.opts.Progress(path)
	}
	w.rep.Dirs++

	entries, err := os.ReadDir(path)
	if err != nil {
		w.addFinding(Finding{Path: path, Kind: Unreadable, Detail: readableError(err)}, false)
		return 0, 0, nil
	}

	var bytes int64
	var files int
	var subdirs []string
	var plainFiles []string

	for _, e := range entries {
		full := filepath.Join(path, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			continue // a symlink is not data, and following one walks in circles
		case e.IsDir():
			if !w.sameFilesystem(info) {
				w.addFinding(Finding{Path: full, Kind: Skipped,
					Detail: "another volume (use --cross-filesystems to include it)"}, false)
				continue
			}
			subdirs = append(subdirs, full)
		case info.Mode().IsRegular():
			bytes += info.Size()
			files++
			plainFiles = append(plainFiles, full)
		}
	}

	ask := append([]string{}, subdirs...)
	if w.opts.CheckFiles {
		ask = append(ask, plainFiles...)
	}
	w.prefetch(ctx, ask)

	if w.opts.CheckFiles {
		for _, f := range plainFiles {
			state, detail := w.statusOf(ctx, f)
			if state == protectedYes {
				continue
			}
			info, err := os.Lstat(f)
			if err != nil {
				continue
			}
			kind := NoBackup
			if state == protectedExcluded {
				kind = Excluded
			}
			w.addFinding(Finding{Path: f, Kind: kind, Detail: detail, Bytes: info.Size(), Files: 1}, true)
		}
	}

	for _, sub := range subdirs {
		if w.skipLocation(sub) {
			w.addFinding(Finding{Path: sub, Kind: Skipped,
				Detail: "a folder macOS guards behind a permission prompt (use --all to include it)"}, false)
			continue
		}
		b, f, err := w.visit(ctx, sub, depth+1)
		if err != nil {
			return bytes, files, err
		}
		bytes += b
		files += f
	}

	return bytes, files, nil
}

// addFinding records a hole. counted says whether its bytes belong to the
// unprotected totals (a skipped or unreadable place is not a hole, it is a gap in
// what we know).
func (w *walker) addFinding(f Finding, counted bool) {
	if counted && (f.Kind == NoBackup || f.Kind == Excluded) {
		w.rep.UnprotectedBytes += f.Bytes
		w.rep.UnprotectedFiles += f.Files
	}
	if f.Kind == NoBackup || f.Kind == Excluded {
		if f.Bytes < w.opts.MinBytes {
			return
		}
	}
	w.rep.Findings = append(w.rep.Findings, f)
}

// rollup counts the regular files below a path without asking any questions.
func (w *walker) rollup(root string) (int64, int) {
	var bytes int64
	var files int
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		case d.IsDir():
			if p != root && !w.sameFilesystem(info) {
				return fs.SkipDir
			}
			w.rep.Dirs++
			return nil
		case info.Mode().IsRegular():
			bytes += info.Size()
			files++
		}
		return nil
	})
	return bytes, files
}

func (w *walker) sameFilesystem(info os.FileInfo) bool {
	if w.opts.CrossFilesystems || w.rootDev == 0 {
		return true
	}
	dev, ok := deviceOf(info)
	if !ok {
		return true
	}
	return dev == w.rootDev
}

func (w *walker) skipLocation(path string) bool {
	if w.opts.AllLocations || w.home == "" {
		return false
	}
	rel, err := filepath.Rel(w.home, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	for _, loc := range protectedLocations {
		if rel == loc {
			return true
		}
	}
	return false
}

func deviceOf(info os.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}

// readableError turns a filesystem error into something a user can act on.
func readableError(err error) string {
	switch {
	case errors.Is(err, os.ErrPermission):
		return "no permission to read it (Full Disk Access would be needed)"
	case errors.Is(err, os.ErrNotExist):
		return "it is not there any more"
	}
	msg := err.Error()
	if pe, ok := err.(*os.PathError); ok {
		msg = pe.Err.Error()
	}
	return msg
}

// Unprotected returns only the findings that are real holes.
func (r Report) Unprotected() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Kind == NoBackup || f.Kind == Excluded {
			out = append(out, f)
		}
	}
	return out
}

// Verdict is the one-line answer: true when nothing is unprotected.
func (r Report) Verdict() bool { return r.UnprotectedFiles == 0 }
