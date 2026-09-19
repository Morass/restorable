package drill_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/morass/restorable/internal/backend"
	"github.com/morass/restorable/internal/backend/fake"
	"github.com/morass/restorable/internal/drill"
)

// live writes files on the "live disk" and returns their paths.
func live(t *testing.T, files map[string]string) (string, map[string][]byte) {
	t.Helper()
	root := t.TempDir()
	content := map[string][]byte{}
	for name, data := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		content[p] = []byte(data)
	}
	return root, content
}

func newBackend(t *testing.T, root string, content map[string][]byte) (*fake.Backend, backend.Snapshot) {
	t.Helper()
	b := fake.New(backend.Restic, "repo", []string{root}, "snap1", content)
	b.Snaps[0].Time = time.Now().Add(-time.Hour)
	return b, b.Snaps[0]
}

func statusOf(rec drill.Receipt, path string) (drill.Status, string) {
	for _, f := range rec.Files {
		if f.Path == path {
			return f.Status, f.Detail
		}
	}
	return "", "missing from the receipt"
}

func TestMatchingBytesPass(t *testing.T) {
	root, content := live(t, map[string]string{"a.txt": "aaa", "b/c.txt": "ccc"})
	b, snap := newBackend(t, root, content)

	rec, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 10, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Pass {
		t.Fatalf("drill failed on a healthy backup: %+v", rec.Files)
	}
	if len(rec.Files) != 2 {
		t.Fatalf("sampled %d files, want 2", len(rec.Files))
	}
	for _, f := range rec.Files {
		if f.Status != drill.Match {
			t.Errorf("%s: status %q, want match (%s)", f.Path, f.Status, f.Detail)
		}
		if f.BackupSHA == "" || f.BackupSHA != f.LiveSHA {
			t.Errorf("%s: hashes not recorded or unequal (%q / %q)", f.Path, f.BackupSHA, f.LiveSHA)
		}
	}
}

func TestDifferentBytesInAnUneditedFileFail(t *testing.T) {
	root, content := live(t, map[string]string{"a.txt": "live bytes"})
	b, snap := newBackend(t, root, content)
	// The backup holds something else, and the live file is older than the snapshot.
	b.Contents["snap1"][filepath.Join(root, "a.txt")] = []byte("backup bytes")
	old := snap.Time.Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "a.txt"), old, old); err != nil {
		t.Fatal(err)
	}

	rec, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 5, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Pass {
		t.Fatal("a backup holding different bytes must fail the drill")
	}
	st, detail := statusOf(rec, filepath.Join(root, "a.txt"))
	if st != drill.Differs {
		t.Errorf("status = %q (%s), want differs", st, detail)
	}
}

func TestAFileEditedAfterTheSnapshotIsNotAFailure(t *testing.T) {
	root, content := live(t, map[string]string{"a.txt": "edited since"})
	b, snap := newBackend(t, root, content)
	b.Contents["snap1"][filepath.Join(root, "a.txt")] = []byte("older bytes")
	later := snap.Time.Add(10 * time.Minute)
	if err := os.Chtimes(filepath.Join(root, "a.txt"), later, later); err != nil {
		t.Fatal(err)
	}

	rec, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 5, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := statusOf(rec, filepath.Join(root, "a.txt")); st != drill.ChangedSince {
		t.Errorf("status = %q, want changed-since", st)
	}
	// Nothing was caught out, but nothing was proved either: that is not a pass.
	if !rec.Inconclusive {
		t.Error("a sample of only edited files is inconclusive, not a failure")
	}
	if rec.Pass {
		t.Error("a drill that compared nothing must not pass")
	}
}

func TestAFileNoLongerOnDiskIsNotAFailure(t *testing.T) {
	root, content := live(t, map[string]string{"gone.txt": "kept in the backup"})
	b, snap := newBackend(t, root, content)
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}

	rec, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 5, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := statusOf(rec, filepath.Join(root, "gone.txt")); st != drill.GoneLive {
		t.Errorf("status = %q, want gone-from-disk", st)
	}
	if !rec.Inconclusive {
		t.Error("a file only the backup has proves nothing either way: inconclusive")
	}
}

func TestAFileTheBackupWillNotHandBackFails(t *testing.T) {
	root, content := live(t, map[string]string{"a.txt": "aaa", "b.txt": "bbb"})
	b, snap := newBackend(t, root, content)
	b.SkipRestore = map[string]bool{filepath.Join(root, "b.txt"): true}

	rec, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 5, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Pass {
		t.Fatal("a file that does not come back must fail the drill")
	}
	if st, _ := statusOf(rec, filepath.Join(root, "b.txt")); st != drill.NotRestored {
		t.Errorf("status = %q, want not-restored", st)
	}
	if st, _ := statusOf(rec, filepath.Join(root, "a.txt")); st != drill.Match {
		t.Errorf("the other file should still match, got %q", st)
	}
}

func TestARestoreThatFailsOutrightFails(t *testing.T) {
	root, content := live(t, map[string]string{"a.txt": "aaa"})
	b, snap := newBackend(t, root, content)
	b.RestoreErr = errors.New("repository is damaged")

	rec, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 5, Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Pass {
		t.Fatal("a failed restore must fail the drill")
	}
	st, detail := statusOf(rec, filepath.Join(root, "a.txt"))
	if st != drill.NotRestored {
		t.Errorf("status = %q, want not-restored", st)
	}
	if detail == "" {
		t.Error("the receipt must carry why the restore failed")
	}
}

func TestTheSameSeedSamplesTheSameFiles(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 200; i++ {
		files[filepath.Join("d", string(rune('a'+i%26)), string(rune('0'+i/26))+".txt")] = "x"
	}
	root, content := live(t, files)
	b, snap := newBackend(t, root, content)

	first, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 8, Seed: 99})
	if err != nil {
		t.Fatal(err)
	}
	second, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 8, Seed: 99})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Files) != 8 {
		t.Fatalf("sampled %d files, want 8", len(first.Files))
	}
	for i := range first.Files {
		if first.Files[i].Path != second.Files[i].Path {
			t.Fatalf("the same seed sampled different files: %s vs %s", first.Files[i].Path, second.Files[i].Path)
		}
	}
	different, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 8, Seed: 100})
	if err != nil {
		t.Fatal(err)
	}
	same := 0
	for i := range first.Files {
		if first.Files[i].Path == different.Files[i].Path {
			same++
		}
	}
	if same == len(first.Files) {
		t.Error("a different seed sampled exactly the same files, so the seed does nothing")
	}
}

func TestBigFilesAreSkippedAndSaidSo(t *testing.T) {
	root, content := live(t, map[string]string{"big.bin": "0123456789"})
	b, snap := newBackend(t, root, content)

	rec, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 5, Seed: 1, MaxBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Files) != 0 {
		t.Fatalf("sampled %d files, want none under the size limit", len(rec.Files))
	}
	if rec.Pass {
		t.Error("a drill that could prove nothing must not pass")
	}
	if rec.Note == "" {
		t.Error("the receipt must say why nothing was sampled")
	}
}

func TestRestoredCopiesAreRemovedUnlessKept(t *testing.T) {
	root, content := live(t, map[string]string{"a.txt": "aaa"})
	b, snap := newBackend(t, root, content)

	target := filepath.Join(t.TempDir(), "kept")
	rec, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 5, Seed: 1, Target: target, Keep: true})
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Pass {
		t.Fatalf("drill failed: %+v", rec.Files)
	}
	// The restore lands in a directory the drill made inside the target, never
	// straight into a directory the user named.
	matches, err := filepath.Glob(filepath.Join(target, "restorable-drill-*", root[1:], "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("--keep must leave the restored copy under its own directory; found %v", matches)
	}

	// Without a target, the temporary directory must not survive the drill.
	before := tempDirs(t)
	if _, err := drill.Run(context.Background(), b, b.Dest, snap, drill.Options{Count: 5, Seed: 2}); err != nil {
		t.Fatal(err)
	}
	after := tempDirs(t)
	if after > before {
		t.Errorf("a drill left %d temporary directories behind", after-before)
	}
}

func tempDirs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "restorable-drill-") {
			n++
		}
	}
	return n
}

func TestABackendThatCannotListCannotBeDrilled(t *testing.T) {
	root, content := live(t, map[string]string{"a.txt": "aaa"})
	b, snap := newBackend(t, root, content)
	_, err := drill.Run(context.Background(), noWalker{b}, b.Dest, snap, drill.Options{Count: 3})
	if err == nil {
		t.Fatal("a backend that cannot list a snapshot must be refused, not silently pass")
	}
}

// noWalker hides the Walk method of a backend.
type noWalker struct{ b *fake.Backend }

func (n noWalker) Kind() backend.Kind { return n.b.Kind() }
func (n noWalker) Installed() bool    { return true }
func (n noWalker) Destinations(ctx context.Context) ([]backend.Destination, error) {
	return n.b.Destinations(ctx)
}
func (n noWalker) Snapshots(ctx context.Context, d backend.Destination) ([]backend.Snapshot, error) {
	return n.b.Snapshots(ctx, d)
}
func (n noWalker) Excluded(ctx context.Context, d backend.Destination, p []string) ([]backend.Exclusion, error) {
	return n.b.Excluded(ctx, d, p)
}
func (n noWalker) List(ctx context.Context, d backend.Destination, s, p string) ([]backend.File, error) {
	return n.b.List(ctx, d, s, p)
}
func (n noWalker) Restore(ctx context.Context, d backend.Destination, s string, f []string, t string) error {
	return n.b.Restore(ctx, d, s, f, t)
}

func TestAScopeThatTheSnapshotSpellsDifferentlyStillWorks(t *testing.T) {
	// The snapshot holds /private/tmp/..., the caller asks about /tmp/...:
	// the same directory on a Mac, spelled the way it was typed.
	root := t.TempDir()
	content := map[string][]byte{
		"/private" + root + "/a.txt": []byte("aaa"),
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := fake.New(backend.Restic, "repo", []string{"/private" + root}, "snap1", content)
	b.Snaps[0].Time = time.Now().Add(-time.Hour)

	rec, err := drill.Run(context.Background(), b, b.Dest, b.Snaps[0], drill.Options{Count: 5, Seed: 1, Path: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Files) != 1 {
		t.Fatalf("sampled %d files, want the one the snapshot holds (%+v)", len(rec.Files), rec)
	}
	if rec.Note != "" && !strings.Contains(rec.Note, "kept") {
		t.Errorf("note = %q, want no complaint: the other spelling was found", rec.Note)
	}
}

func TestAScopeAboveWhatTheSnapshotHoldsSamplesThoseRoots(t *testing.T) {
	// The snapshot holds ~/Documents and ~/code; the drill is asked for ~. A
	// listing of a path above the snapshot's own roots does not walk into them,
	// so the scope has to become the roots themselves.
	root := t.TempDir()
	docs := filepath.Join(root, "Documents")
	code := filepath.Join(root, "code")
	content := map[string][]byte{}
	for _, p := range []string{filepath.Join(docs, "a.txt"), filepath.Join(code, "b.go")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("xx"), 0o644); err != nil {
			t.Fatal(err)
		}
		content[p] = []byte("xx")
	}
	b := fake.New(backend.Restic, "repo", []string{docs, code}, "snap1", content)
	b.Snaps[0].Time = time.Now().Add(-time.Hour)

	rec, err := drill.Run(context.Background(), b, b.Dest, b.Snaps[0], drill.Options{Count: 10, Seed: 4, Path: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Files) != 2 {
		t.Fatalf("sampled %d files, want both roots walked: %+v", len(rec.Files), rec)
	}
	if rec.Note != "" {
		t.Errorf("note = %q, want none: the scope was resolved, not abandoned", rec.Note)
	}
	if !rec.Pass {
		t.Errorf("drill failed: %+v", rec.Files)
	}
}

func TestAScopeWithNothingInCommonSamplesTheWholeSnapshot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	if err := os.WriteFile(file, []byte("xx"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := fake.New(backend.Restic, "repo", []string{root}, "snap1", map[string][]byte{file: []byte("xx")})
	b.Snaps[0].Time = time.Now().Add(-time.Hour)

	rec, err := drill.Run(context.Background(), b, b.Dest, b.Snaps[0],
		drill.Options{Count: 5, Seed: 4, Path: "/somewhere/else/entirely"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Files) != 1 {
		t.Fatalf("sampled %d files, want the fallback to the whole snapshot", len(rec.Files))
	}
	if rec.Note == "" {
		t.Error("falling back to the whole snapshot must be said out loud")
	}
}
