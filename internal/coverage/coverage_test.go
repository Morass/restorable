package coverage_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/morass/restorable/internal/backend"
	"github.com/morass/restorable/internal/backend/fake"
	"github.com/morass/restorable/internal/coverage"
)

// tree writes a fixture tree: paths mapped to contents, directories implied.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for p, content := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func protectors(bs ...backend.Backend) []*coverage.Protector {
	var out []*coverage.Protector
	for _, b := range bs {
		dests, _ := b.Destinations(context.Background())
		for _, d := range dests {
			out = append(out, &coverage.Protector{Dest: d, Backend: b})
		}
	}
	return out
}

func findingFor(rep coverage.Report, path string) (coverage.Finding, bool) {
	for _, f := range rep.Findings {
		if f.Path == path {
			return f, true
		}
	}
	return coverage.Finding{}, false
}

func TestUnclaimedDirectoryIsAHole(t *testing.T) {
	root := tree(t, map[string]string{
		"kept/a.txt":   "aaaa",
		"kept/b.txt":   "bbbb",
		"unkept/c.txt": "cccccccccc",
	})
	b := fake.New(backend.Restic, "repo", []string{filepath.Join(root, "kept")}, "snap", nil)

	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}}, protectors(b))
	if err != nil {
		t.Fatal(err)
	}
	f, ok := findingFor(rep, filepath.Join(root, "unkept"))
	if !ok {
		t.Fatalf("unkept was not reported; findings: %+v", rep.Findings)
	}
	if f.Kind != coverage.NoBackup {
		t.Errorf("kind = %q, want %q", f.Kind, coverage.NoBackup)
	}
	if f.Files != 1 || f.Bytes != 10 {
		t.Errorf("rollup = %d files / %d bytes, want 1/10", f.Files, f.Bytes)
	}
	if _, ok := findingFor(rep, filepath.Join(root, "kept")); ok {
		t.Error("a claimed directory must not be reported as a hole")
	}
	if rep.UnprotectedBytes != 10 || rep.UnprotectedFiles != 1 {
		t.Errorf("unprotected totals = %d/%d, want 10/1", rep.UnprotectedBytes, rep.UnprotectedFiles)
	}
	if rep.TotalFiles != 3 || rep.TotalBytes != 18 {
		t.Errorf("totals = %d files / %d bytes, want 3/18", rep.TotalFiles, rep.TotalBytes)
	}
	if rep.Verdict() {
		t.Error("Verdict says everything is protected when one file is not")
	}
}

func TestExclusionIsReportedWithItsReason(t *testing.T) {
	root := tree(t, map[string]string{
		"docs/a.txt":   "aaaa",
		"caches/b.bin": "bbbbbb",
	})
	b := fake.New(backend.TimeMachine, "disk", []string{"/"}, "snap", nil)
	b.Excludes[filepath.Join(root, "caches")] = "a cache directory, which Time Machine never backs up"

	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}}, protectors(b))
	if err != nil {
		t.Fatal(err)
	}
	f, ok := findingFor(rep, filepath.Join(root, "caches"))
	if !ok {
		t.Fatalf("the excluded directory was not reported; findings: %+v", rep.Findings)
	}
	if f.Kind != coverage.Excluded {
		t.Errorf("kind = %q, want %q", f.Kind, coverage.Excluded)
	}
	if !strings.Contains(f.Detail, "cache directory") || !strings.Contains(f.Detail, "timemachine") {
		t.Errorf("detail = %q, want the backend and the reason", f.Detail)
	}
}

func TestTwoDestinationsOneKeepsIt(t *testing.T) {
	root := tree(t, map[string]string{"work/a.txt": "aaaa"})
	tm := fake.New(backend.TimeMachine, "disk", []string{"/"}, "snap", nil)
	tm.Excludes[filepath.Join(root, "work")] = "excluded by hand"
	res := fake.New(backend.Restic, "repo", []string{root}, "snap", nil)

	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}}, protectors(tm, res))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unprotected()) != 0 {
		t.Errorf("a path one destination keeps is protected; got %+v", rep.Unprotected())
	}
}

func TestUnanswerableExclusionCountsAsProtected(t *testing.T) {
	root := tree(t, map[string]string{"work/a.txt": "aaaa"})
	res := fake.New(backend.Restic, "repo", []string{root}, "snap", nil)
	res.Unsupported = true // restic cannot say what it was told to exclude

	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}}, protectors(res))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unprotected()) != 0 {
		t.Errorf("an unanswerable exclusion must not invent a hole; got %+v", rep.Unprotected())
	}
}

func TestExclusionQuestionsAreAskedALevelAtATime(t *testing.T) {
	// Three levels of twenty directories each: 20 + 400 directories to ask about.
	// On a Mac every question costs a process, so the number of calls has to
	// follow the depth of the tree, not the number of directories in it.
	files := map[string]string{}
	for i := 0; i < 20; i++ {
		for j := 0; j < 20; j++ {
			files[filepath.Join(fmt.Sprintf("d%02d", i), fmt.Sprintf("s%02d", j), "f.txt")] = "x"
		}
	}
	root := tree(t, files)
	b := fake.New(backend.TimeMachine, "disk", []string{"/"}, "snap", nil)

	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}}, protectors(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unprotected()) != 0 {
		t.Fatalf("nothing should be unprotected here: %+v", rep.Unprotected())
	}
	if b.Calls.Paths < 420 {
		t.Fatalf("asked about %d paths, want every directory", b.Calls.Paths)
	}
	// 420 directories over three levels: a handful of calls, not one per directory.
	if b.Calls.Excluded > 8 {
		t.Errorf("%d calls for %d directories; questions must be asked a level at a time",
			b.Calls.Excluded, b.Calls.Paths)
	}
}

func TestAHugeLevelIsSplitIntoSeveralQuestions(t *testing.T) {
	// A directory with a thousand children must not become one enormous command
	// line: the questions are chunked.
	files := map[string]string{}
	for i := 0; i < 1000; i++ {
		files[filepath.Join(fmt.Sprintf("d%04d", i), "f.txt")] = "x"
	}
	root := tree(t, files)
	b := fake.New(backend.TimeMachine, "disk", []string{"/"}, "snap", nil)

	if _, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}}, protectors(b)); err != nil {
		t.Fatal(err)
	}
	if b.Calls.Excluded < 4 {
		t.Errorf("%d calls for 1000 directories; a level that big must be split", b.Calls.Excluded)
	}
	if b.Calls.Excluded > 20 {
		t.Errorf("%d calls for 1000 directories; the chunks are too small", b.Calls.Excluded)
	}
}

func TestSymlinksAreNotFollowed(t *testing.T) {
	root := tree(t, map[string]string{"real/a.txt": "aaaa"})
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	b := fake.New(backend.Restic, "repo", []string{filepath.Join(root, "real")}, "snap", nil)

	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}}, protectors(b))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findingFor(rep, filepath.Join(root, "link")); ok {
		t.Error("a symlink was walked; it is not data and it can loop")
	}
	if rep.TotalFiles != 1 {
		t.Errorf("total files = %d, want 1 (the symlink must not be counted twice)", rep.TotalFiles)
	}
}

func TestUnreadableDirectoryIsReportedNotSilentlyMissed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	root := tree(t, map[string]string{"secret/a.txt": "aaaa"})
	secret := filepath.Join(root, "secret")
	if err := os.Chmod(secret, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o755) })
	b := fake.New(backend.Restic, "repo", []string{root}, "snap", nil)

	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}}, protectors(b))
	if err != nil {
		t.Fatal(err)
	}
	f, ok := findingFor(rep, secret)
	if !ok {
		t.Fatalf("an unreadable directory must be named; findings: %+v", rep.Findings)
	}
	if f.Kind != coverage.Unreadable {
		t.Errorf("kind = %q, want %q", f.Kind, coverage.Unreadable)
	}
	if !strings.Contains(f.Detail, "permission") {
		t.Errorf("detail = %q, want it to mention permission", f.Detail)
	}
}

func TestMinBytesHidesSmallHolesButStillCountsThem(t *testing.T) {
	root := tree(t, map[string]string{
		"tiny/a.txt": "a",
		"big/b.bin":  strings.Repeat("b", 4096),
	})
	b := fake.New(backend.Restic, "repo", []string{"/nowhere"}, "snap", nil)

	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}, MinBytes: 1024}, protectors(b))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findingFor(rep, filepath.Join(root, "tiny")); ok {
		t.Error("a hole under --min-size must not be listed")
	}
	if rep.UnprotectedFiles != 2 {
		t.Errorf("unprotected files = %d, want 2: a hidden hole is still counted", rep.UnprotectedFiles)
	}
}

func TestDepthLimitStopsDescendingButKeepsCounting(t *testing.T) {
	root := tree(t, map[string]string{"a/b/c/d/e/f.txt": "xxxx"})
	b := fake.New(backend.Restic, "repo", []string{root}, "snap", nil)

	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}, MaxDepth: 2}, protectors(b))
	if err != nil {
		t.Fatal(err)
	}
	if rep.TotalFiles != 1 || rep.TotalBytes != 4 {
		t.Errorf("totals = %d/%d, want 1/4 even though the walk stopped early", rep.TotalFiles, rep.TotalBytes)
	}
}

func TestCheckFilesFindsAFileExcludedByHand(t *testing.T) {
	root := tree(t, map[string]string{"docs/keep.txt": "aaaa", "docs/skip.txt": "bbbb"})
	b := fake.New(backend.TimeMachine, "disk", []string{"/"}, "snap", nil)
	b.Excludes[filepath.Join(root, "docs", "skip.txt")] = "excluded by a sticky flag on the item itself (xattr)"

	without, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}}, protectors(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(without.Unprotected()) != 0 {
		t.Errorf("without --files a single excluded file is not looked for; got %+v", without.Unprotected())
	}

	b2 := fake.New(backend.TimeMachine, "disk", []string{"/"}, "snap", nil)
	b2.Excludes[filepath.Join(root, "docs", "skip.txt")] = "excluded by a sticky flag on the item itself (xattr)"
	with, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}, CheckFiles: true}, protectors(b2))
	if err != nil {
		t.Fatal(err)
	}
	f, ok := findingFor(with, filepath.Join(root, "docs", "skip.txt"))
	if !ok {
		t.Fatalf("--files must find the excluded file; findings: %+v", with.Findings)
	}
	if f.Kind != coverage.Excluded || f.Files != 1 {
		t.Errorf("finding = %+v, want one excluded file", f)
	}
	if !with.CheckedFiles {
		t.Error("the report must record that files were checked")
	}
}

func TestNoDestinationAtAllSaysSo(t *testing.T) {
	root := tree(t, map[string]string{"a.txt": "aaaa"})
	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, ok := findingFor(rep, root)
	if !ok {
		t.Fatalf("the root itself is the hole when there is no backup; findings: %+v", rep.Findings)
	}
	if !strings.Contains(f.Detail, "no backup destination could be read") {
		t.Errorf("detail = %q, want it to say no destination could be read", f.Detail)
	}
	if f.Files != 1 {
		t.Errorf("files = %d, want 1", f.Files)
	}
}

func TestUnreadableRootIsNotADirectoryError(t *testing.T) {
	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{"/nonexistent-path-for-a-test"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != coverage.Unreadable {
		t.Errorf("findings = %+v, want one unreadable root", rep.Findings)
	}
}

func TestDestinationsThatCouldNotBeReadDoNotProtectAnything(t *testing.T) {
	root := tree(t, map[string]string{"a.txt": "aaaa"})
	b := fake.New(backend.Restic, "repo", []string{root}, "snap", nil)
	b.Dest.State = backend.StateLocked // claims the root, but we could not read it

	rep, err := coverage.Run(context.Background(), coverage.Options{Roots: []string{root}}, protectors(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Unprotected()) == 0 {
		t.Error("a locked destination must not count as cover")
	}
}
