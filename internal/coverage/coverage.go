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

// shortError trims a tool's complaint to something a table can hold.
func shortError(err error) string {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(msg) > 120 {
		msg = msg[:120] + "…"
	}
	return msg
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
	// Unknown means a destination claims the path but could not be asked about it.
	Unknown Kind = "unknown"
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
	UnknownBytes     int64 `json:"unknown_bytes"`
	UnknownFiles     int   `json:"unknown_files"`

	Dirs     int           `json:"dirs_visited"`
	Duration time.Duration `json:"duration"`
	// CheckedFiles says whether individual files were asked about, or only
	// directories, because that changes what the report can promise.
	CheckedFiles bool `json:"checked_files"`
	// MinBytes is the size below which a hole was counted but not listed.
	MinBytes int64 `json:"min_bytes"`
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

// repositoryPaths collects the local directories the destinations themselves
// live in: a backup repository does not need backing up, and reporting it as a
// hole would be noise in every single run.
func repositoryPaths(protectors []*Protector) []string {
	var out []string
	for _, p := range protectors {
		id := p.Dest.ID
		if id == "" || strings.Contains(id, ":") && !filepath.IsAbs(id) {
			continue // a remote repository (sftp:, s3:, rest:) is not on this disk
		}
		if filepath.IsAbs(id) {
			out = append(out, filepath.Clean(id))
		}
	}
	return out
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
	rep := Report{GeneratedAt: start, Roots: opts.Roots, CheckedFiles: opts.CheckFiles, MinBytes: opts.MinBytes}
	for _, p := range protectors {
		rep.Destinations = append(rep.Destinations, p.Dest)
	}
	w := &walker{ctx: ctx, opts: opts, protectors: protectors, rep: &rep, repos: repositoryPaths(protectors)}

	home, _ := os.UserHomeDir()
	w.home = home

	roots := make([]pending, 0, len(opts.Roots))
	for _, root := range opts.Roots {
		info, err := os.Lstat(root)
		if err != nil {
			rep.Findings = append(rep.Findings, Finding{Path: root, Kind: Unreadable, Detail: readableError(err)})
			continue
		}
		if !info.IsDir() {
			return rep, fmt.Errorf("%s is not a directory", root)
		}
		if w.rootDev == 0 {
			w.rootDev, _ = deviceOf(info)
		}
		roots = append(roots, pending{path: root, depth: 0})
	}
	if err := w.walk(ctx, roots); err != nil {
		return rep, err
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
	ctx        context.Context
	opts       Options
	protectors []*Protector
	rep        *Report
	home       string
	rootDev    uint64
	repos      []string
}

// isRepository reports whether a path is a backup repository this tool reads.
func (w *walker) isRepository(path string) bool {
	clean := filepath.Clean(path)
	for _, r := range w.repos {
		if clean == r {
			return true
		}
	}
	return false
}

// protection is what a destination does with a path.
type protection int

const (
	protectedYes protection = iota
	protectedNo
	protectedExcluded
	// protectedUnknown means a destination claims the path but could not be
	// asked whether it keeps it. Reporting that as cover would be a guess.
	protectedUnknown
)

// claimsBelow reports whether any readable destination covers something strictly
// inside this directory.
func (w *walker) claimsBelow(path string) bool {
	prefix := strings.TrimSuffix(filepath.Clean(path), "/") + "/"
	for _, p := range w.protectors {
		if p.Dest.State != backend.StateOK || !p.Dest.Connected {
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

// statusOf decides whether a path is protected, and when it is not, why.
func (w *walker) statusOf(ctx context.Context, path string) (protection, string) {
	var claimed []*Protector
	for _, p := range w.protectors {
		// A destination that is not here right now covers nothing right now,
		// however recent the date it last recorded.
		if p.Dest.State != backend.StateOK || !p.Dest.Connected {
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
	var unknown []string
	for _, p := range claimed {
		ex, err := w.exclusion(ctx, p, path)
		if err != nil {
			// A question that could not be answered is not an answer: say so
			// rather than reporting cover nobody proved.
			unknown = append(unknown, p.Label()+": "+shortError(err))
			continue
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
	if len(unknown) > 0 {
		return protectedUnknown, "could not ask whether it is kept — " + strings.Join(unknown, "; ")
	}
	return protectedExcluded, strings.Join(reasons, "; ")
}

func (w *walker) noBackupDetail() string {
	live, away := 0, 0
	for _, p := range w.protectors {
		switch {
		case p.Dest.State == backend.StateOK && p.Dest.Connected:
			live++
		case p.Dest.State == backend.StateOK:
			away++
		}
	}
	switch {
	case live == 0 && away > 0:
		return "the backup destination is not connected, so nothing is being kept right now"
	case live == 0:
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

// chunkSize is how many paths one exclusion question carries. Big enough that a
// whole level usually costs one call, small enough to stay well inside the limit
// on the length of a command line.
const chunkSize = 256

// prefetch asks every protector about many paths at once, in chunks.
func (w *walker) prefetch(ctx context.Context, paths []string) {
	for _, p := range w.protectors {
		if p.Dest.State != backend.StateOK || !p.Dest.Connected {
			continue
		}
		var missing []string
		for _, path := range paths {
			if _, ok := p.excluded[path]; !ok && p.Dest.Claims(path) {
				missing = append(missing, path)
			}
		}
		for len(missing) > 0 {
			n := len(missing)
			if n > chunkSize {
				n = chunkSize
			}
			batch := missing[:n]
			missing = missing[n:]
			answers, err := p.Backend.Excluded(ctx, p.Dest, batch)
			if err != nil || len(answers) != len(batch) {
				continue // fall back to one question per path
			}
			for i, path := range batch {
				p.excluded[path] = answers[i]
			}
		}
	}
}

// pending is a directory waiting to be looked at.
type pending struct {
	path  string
	depth int
	// protected says a destination keeps this directory itself. A directory that
	// is walked only because something deeper is kept is not protected.
	protected bool
}

// walk visits the tree one depth at a time, so every exclusion question for a
// whole level is asked in a few calls instead of one call per directory. On a
// Mac each tmutil call costs about 60 ms, which is minutes of difference over a
// home directory.
func (w *walker) walk(ctx context.Context, level []pending) error {
	// The roots themselves have to be classified before anything is read.
	level = w.classify(ctx, level)

	for len(level) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		var children []pending
		for _, dir := range level {
			kids, err := w.read(ctx, dir)
			if err != nil {
				return err
			}
			children = append(children, kids...)
		}
		level = w.classify(ctx, children)
	}
	return nil
}

// classify asks about a whole level at once and returns the directories that are
// kept by a backup and still worth descending into. Everything else is counted
// here: a hole is rolled up and reported, and a protected directory at the depth
// limit is counted without being opened.
func (w *walker) classify(ctx context.Context, dirs []pending) []pending {
	if len(dirs) == 0 {
		return nil
	}
	paths := make([]string, 0, len(dirs))
	for _, d := range dirs {
		paths = append(paths, d.path)
	}
	w.prefetch(ctx, paths)

	var keep []pending
	for _, d := range dirs {
		state, detail := w.statusOf(ctx, d.path)
		switch {
		case state == protectedYes:
			if d.depth >= w.opts.MaxDepth {
				b, f := w.rollup(d.path)
				w.count(b, f)
				continue
			}
			d.protected = true
			keep = append(keep, d)
		case w.claimsBelow(d.path) && d.depth < w.opts.MaxDepth:
			// Nothing keeps this directory itself, but a destination keeps
			// something deeper: descend so the hole is reported where it is.
			keep = append(keep, d)
		case state == protectedUnknown:
			b, f := w.rollup(d.path)
			w.count(b, f)
			w.addFinding(Finding{Path: d.path, Kind: Unknown, Detail: detail, Bytes: b, Files: f}, false)
		default:
			b, f := w.rollup(d.path)
			w.count(b, f)
			kind := NoBackup
			if state == protectedExcluded {
				kind = Excluded
			}
			w.addFinding(Finding{Path: d.path, Kind: kind, Detail: detail, Bytes: b, Files: f}, true)
		}
	}
	return keep
}

// count adds to the totals of what was walked.
func (w *walker) count(bytes int64, files int) {
	w.rep.TotalBytes += bytes
	w.rep.TotalFiles += files
}

// read lists one directory: its files are counted here, and its subdirectories
// are handed back for the next level.
func (w *walker) read(ctx context.Context, dir pending) ([]pending, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if w.opts.Progress != nil {
		w.opts.Progress(dir.path)
	}
	w.rep.Dirs++

	entries, err := os.ReadDir(dir.path)
	if err != nil {
		w.addFinding(Finding{Path: dir.path, Kind: Unreadable, Detail: readableError(err)}, false)
		return nil, nil
	}

	var (
		bytes      int64
		files      int
		subdirs    []pending
		plainFiles []string
	)
	for _, e := range entries {
		full := filepath.Join(dir.path, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			// A symlink is not data, and following one walks in circles — but
			// staying quiet about it would hide what was not looked at.
			w.addFinding(Finding{Path: full, Kind: Skipped,
				Detail: "a symlink: what it points at is walked where it really lives, not here"}, false)
			continue
		case e.IsDir():
			switch {
			case !w.sameFilesystem(info):
				w.addFinding(Finding{Path: full, Kind: Skipped,
					Detail: "another volume (use --cross-filesystems to include it)"}, false)
			case w.isRepository(full):
				w.addFinding(Finding{Path: full, Kind: Skipped,
					Detail: "a backup repository, which does not need backing up itself"}, false)
			case w.skipLocation(full):
				w.addFinding(Finding{Path: full, Kind: Skipped,
					Detail: "a folder macOS guards behind a permission prompt (use --all to include it)"}, false)
			default:
				subdirs = append(subdirs, pending{path: full, depth: dir.depth + 1})
			}
		case info.Mode().IsRegular():
			bytes += info.Size()
			files++
			plainFiles = append(plainFiles, full)
		}
	}
	w.count(bytes, files)

	// A directory can be walked although nothing keeps it, because a backup keeps
	// something deeper. Its own loose files are then holes, and saying nothing
	// about them would leave them out of the verdict entirely.
	if !dir.protected && files > 0 {
		w.addFinding(Finding{
			Path: dir.path, Kind: NoBackup, Bytes: bytes, Files: files,
			Detail: "the files directly in it are kept by no backup (what is kept lives deeper)",
		}, true)
	}

	if w.opts.CheckFiles && len(plainFiles) > 0 {
		w.prefetch(ctx, plainFiles)
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
			counted := true
			switch state {
			case protectedExcluded:
				kind = Excluded
			case protectedUnknown:
				kind, counted = Unknown, false
			}
			w.addFinding(Finding{Path: f, Kind: kind, Detail: detail, Bytes: info.Size(), Files: 1}, counted)
		}
	}
	return subdirs, nil
}

// addFinding records a hole. counted says whether its bytes belong to the
// unprotected totals (a skipped or unreadable place is not a hole, it is a gap in
// what we know).
func (w *walker) addFinding(f Finding, counted bool) {
	switch {
	case counted && (f.Kind == NoBackup || f.Kind == Excluded):
		w.rep.UnprotectedBytes += f.Bytes
		w.rep.UnprotectedFiles += f.Files
	case f.Kind == Unknown:
		w.rep.UnknownBytes += f.Bytes
		w.rep.UnknownFiles += f.Files
	}
	if f.Kind == NoBackup || f.Kind == Excluded {
		if f.Bytes < w.opts.MinBytes {
			return
		}
	}
	w.rep.Findings = append(w.rep.Findings, f)
}

// rollupMaxDepth bounds a rollup: a bind mount can make a directory contain
// itself, and counting must not follow that for ever.
const rollupMaxDepth = 64

// rollup counts the regular files below a path without asking any questions.
func (w *walker) rollup(root string) (int64, int) {
	var bytes int64
	var files int
	rootDepth := strings.Count(filepath.Clean(root), string(filepath.Separator))
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if w.ctx != nil && w.ctx.Err() != nil {
			return w.ctx.Err()
		}
		if err != nil {
			if p != root {
				w.addFinding(Finding{Path: p, Kind: Unreadable, Detail: readableError(err)}, false)
			}
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
			if strings.Count(filepath.Clean(p), string(filepath.Separator))-rootDepth > rollupMaxDepth {
				w.addFinding(Finding{Path: p, Kind: Skipped,
					Detail: "deeper than this tool will count; a loop in the filesystem would never end"}, false)
				return fs.SkipDir
			}
			// The same folders are left alone here as in the walk proper:
			// reading them would make macOS ask the user for permission.
			if p != root && w.skipLocation(p) {
				w.addFinding(Finding{Path: p, Kind: Skipped,
					Detail: "a folder macOS guards behind a permission prompt (use --all to include it)"}, false)
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

// Unknowns returns the places a destination claims but could not be asked about.
func (r Report) Unknowns() []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Kind == Unknown {
			out = append(out, f)
		}
	}
	return out
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

// Verdict is the one-line answer: true when nothing is unprotected and nothing
// was left unanswered. A walk that could not tell has not said yes.
func (r Report) Verdict() bool { return r.UnprotectedFiles == 0 && r.UnknownFiles == 0 }
