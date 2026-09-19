package tmbk_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/morass/restorable/internal/backend"
	"github.com/morass/restorable/internal/backend/tmbk"
	"github.com/morass/restorable/internal/run"
)

// stubTmutil installs a fake tmutil whose script answers per subcommand, and
// returns a reader of what it was asked.
func stubTmutil(t *testing.T, script string) func() string {
	t.Helper()
	if _, err := os.Stat("/usr/bin/plutil"); err != nil {
		t.Skip("plutil is only on macOS")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	body := "#!/bin/sh\necho \"ARGS: $*\" >> " + log + "\n" + script + "\n"
	path := filepath.Join(dir, "tmutil")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(run.Tmutil.EnvVar, path)
	return func() string {
		data, _ := os.ReadFile(log)
		return string(data)
	}
}

const twoDestinations = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Destinations</key>
  <array>
    <dict>
      <key>Name</key><string>Backup Disk</string>
      <key>Kind</key><string>Local</string>
      <key>ID</key><string>1E2D3C4B-5A69-4788-9ABC-DEF012345678</string>
      <key>MountPoint</key><string>MOUNTPOINT</string>
    </dict>
    <dict>
      <key>Name</key><string>Time Capsule</string>
      <key>Kind</key><string>Network</string>
      <key>ID</key><string>AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE</string>
    </dict>
  </array>
</dict>
</plist>`

func TestNoDestinationIsSaidPlainlyAndIsNotAnError(t *testing.T) {
	stubTmutil(t, `case "$1" in
destinationinfo) echo '<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict></dict></plist>';;
*) exit 1;;
esac`)
	b := &tmbk.Backend{}
	dests, err := b.Destinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dests) != 1 {
		t.Fatalf("destinations = %d, want one row saying there is none", len(dests))
	}
	if !dests[0].NotConfigured {
		t.Error("a Mac with no destination must be marked as not configured, not as a failure")
	}
	if !strings.Contains(dests[0].Err, "no Time Machine destination") {
		t.Errorf("message = %q", dests[0].Err)
	}
}

func TestDestinationsAreReadWithTheirNamesAndCoverEverything(t *testing.T) {
	mount := t.TempDir()
	info := strings.ReplaceAll(twoDestinations, "MOUNTPOINT", mount)
	stubTmutil(t, `case "$1" in
destinationinfo) cat <<'PLIST'
`+info+`
PLIST
;;
listbackups) echo "`+mount+`/Backups.backupdb/box/2026-09-18-101500"; echo "`+mount+`/Backups.backupdb/box/2026-09-19-091500";;
listlocalsnapshotdates) echo "Snapshot dates for all disks:"; echo "2026-09-19-120000";;
*) exit 1;;
esac`)
	b := &tmbk.Backend{Prefs: filepath.Join(t.TempDir(), "absent.plist")}
	dests, err := b.Destinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dests) != 2 {
		t.Fatalf("destinations = %d, want 2", len(dests))
	}
	if dests[0].Label != "Backup Disk" {
		t.Errorf("label = %q", dests[0].Label)
	}
	if len(dests[0].Roots) != 1 || dests[0].Roots[0] != "/" {
		t.Errorf("roots = %v, want / because Time Machine covers what it is not told to skip", dests[0].Roots)
	}
	if dests[0].LastOKSource != "the mounted destination" {
		t.Errorf("source = %q, want the mounted destination for a mounted disk", dests[0].LastOKSource)
	}
	want := time.Date(2026, 9, 19, 9, 15, 0, 0, time.Local)
	if !dests[0].LastOK.Equal(want) {
		t.Errorf("last backup = %s, want the newest backup %s", dests[0].LastOK, want)
	}
	if dests[0].Snapshots != 2 {
		t.Errorf("snapshots = %d, want 2", dests[0].Snapshots)
	}
	// The second destination is not mounted, so the local snapshot is the only
	// date available and the report must say so rather than claim a backup.
	if dests[1].LastOKSource == "" || !strings.Contains(dests[1].LastOKSource, "local snapshot") {
		t.Errorf("second source = %q, want it to admit it is a local snapshot", dests[1].LastOKSource)
	}
}

func TestThePreferencesDateIsUsedWhenTheDiskIsNotMounted(t *testing.T) {
	prefs := filepath.Join(t.TempDir(), "com.apple.TimeMachine.plist")
	body := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
 <key>Destinations</key><array><dict>
   <key>DestinationID</key><string>AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE</string>
   <key>SnapshotDates</key><array><string>2026-09-17-101500</string></array>
   <key>BACKUP_COMPLETED_DATE</key><string>2026-09-18-221500</string>
 </dict></array>
</dict></plist>`
	if err := os.WriteFile(prefs, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stubTmutil(t, `case "$1" in
destinationinfo) cat <<'PLIST'
`+strings.ReplaceAll(twoDestinations, "MOUNTPOINT", "/Volumes/gone")+`
PLIST
;;
listbackups) echo "tmutil: Unable to locate machine directory for host." >&2; exit 1;;
listlocalsnapshotdates) echo "Snapshot dates for all disks:";;
*) exit 1;;
esac`)
	b := &tmbk.Backend{Prefs: prefs}
	dests, err := b.Destinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var capsule backend.Destination
	for _, d := range dests {
		if strings.HasPrefix(d.ID, "AAAA") {
			capsule = d
		}
	}
	if capsule.LastOKSource != "Time Machine's preferences" {
		t.Fatalf("source = %q, want the preferences", capsule.LastOKSource)
	}
	want := time.Date(2026, 9, 18, 22, 15, 0, 0, time.UTC)
	if !capsule.LastOK.Equal(want) {
		t.Errorf("last backup = %s, want %s", capsule.LastOK, want)
	}
}

func TestAnUnmountedDestinationWithNoDateAtAllSaysUnknown(t *testing.T) {
	stubTmutil(t, `case "$1" in
destinationinfo) cat <<'PLIST'
`+strings.ReplaceAll(twoDestinations, "MOUNTPOINT", "/Volumes/gone")+`
PLIST
;;
*) exit 1;;
esac`)
	b := &tmbk.Backend{Prefs: filepath.Join(t.TempDir(), "absent.plist")}
	dests, err := b.Destinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dests {
		if d.State != backend.StateUnreachable {
			t.Errorf("%s: state = %q, want unreachable when no date can be read", d.Label, d.State)
		}
		if !strings.Contains(d.Err, "unknown") {
			t.Errorf("%s: message = %q, want it to admit the last backup is unknown", d.Label, d.Err)
		}
	}
}

func TestExclusionsAreAskedInOneCallAndKeptInOrder(t *testing.T) {
	calls := stubTmutil(t, `case "$1" in
isexcluded) shift; [ "$1" = "--" ] && shift; for p in "$@"; do case "$p" in
  */Caches) echo "[Excluded]  $p";;
  *) echo "[Included]  $p";;
esac; done;;
*) exit 1;;
esac`)
	b := &tmbk.Backend{}
	paths := []string{"/Users/x/Documents", "/Users/x/Library/Caches", "/Users/x/code"}
	ex, err := b.Excluded(context.Background(), backend.Destination{}, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(ex) != 3 {
		t.Fatalf("answers = %d, want 3", len(ex))
	}
	if ex[0].Excluded || !ex[1].Excluded || ex[2].Excluded {
		t.Errorf("answers = %+v, want only the cache excluded and in order", ex)
	}
	if ex[1].Reason == "" {
		t.Error("an exclusion must come with a reason")
	}
	if !strings.Contains(ex[1].Reason, "cache") {
		t.Errorf("reason = %q, want the standard cache exclusion named", ex[1].Reason)
	}
	if n := strings.Count(calls(), "ARGS: isexcluded"); n != 1 {
		t.Errorf("tmutil was called %d times for 3 paths; questions must be batched", n)
	}
}

func TestAnAnswerForTheWrongNumberOfPathsIsAnError(t *testing.T) {
	stubTmutil(t, `case "$1" in
isexcluded) echo "[Included]  one";;
*) exit 1;;
esac`)
	b := &tmbk.Backend{}
	_, err := b.Excluded(context.Background(), backend.Destination{}, []string{"/a", "/b"})
	if err == nil {
		t.Fatal("a short answer must be an error, not a silent 'included'")
	}
}

func TestAnUnexpectedAnswerIsAnError(t *testing.T) {
	stubTmutil(t, `case "$1" in
isexcluded) echo "tmutil: something went wrong";;
*) exit 1;;
esac`)
	b := &tmbk.Backend{}
	if _, err := b.Excluded(context.Background(), backend.Destination{}, []string{"/a"}); err == nil {
		t.Fatal("an answer that is neither Included nor Excluded must be an error")
	}
}

func TestRestoreCopiesOutOfTheBackupAndNeverIntoIt(t *testing.T) {
	backupRoot := t.TempDir()
	// A Time Machine backup is a directory tree holding the live layout.
	src := filepath.Join(backupRoot, "Users", "x", "Documents")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("backed up"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &tmbk.Backend{}
	target := t.TempDir()
	err := b.Restore(context.Background(), backend.Destination{}, backupRoot, []string{"/Users/x/Documents/a.txt"}, target)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "Users", "x", "Documents", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "backed up" {
		t.Errorf("restored %q", got)
	}
	// Nothing new inside the backup.
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("the backup now holds %d entries; a restore must never write into it", len(entries))
	}
}

func TestWalkListsTheLivePathsABackupHolds(t *testing.T) {
	backupRoot := t.TempDir()
	dir := filepath.Join(backupRoot, "Users", "x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("xx"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b := &tmbk.Backend{}
	var seen []string
	err := b.Walk(context.Background(), backend.Destination{}, backupRoot, "", func(f backend.File) error {
		seen = append(seen, f.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("walked %v, want two files", seen)
	}
	for _, p := range seen {
		if !strings.HasPrefix(p, "/Users/x/") {
			t.Errorf("walked %q, want the live path, not the path inside the backup", p)
		}
	}
}

func TestARestoreWillNotWriteThroughASymlink(t *testing.T) {
	backupRoot := t.TempDir()
	src := filepath.Join(backupRoot, "Users", "x")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("from the backup"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	victim := filepath.Join(t.TempDir(), "important.conf")
	if err := os.WriteFile(victim, []byte("do not touch"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Something has already put a symlink where the restore wants to write.
	dst := filepath.Join(target, "Users", "x")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dst, "a.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	b := &tmbk.Backend{}
	err := b.Restore(context.Background(), backend.Destination{}, backupRoot, []string{"/Users/x/a.txt"}, target)
	if err == nil {
		t.Error("writing through a symlink must be refused")
	}
	got, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "do not touch" {
		t.Errorf("the file behind the symlink was overwritten with %q", got)
	}
}

func TestAPathFromABackupCannotEscapeTheTarget(t *testing.T) {
	backupRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(backupRoot, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupRoot, "etc", "hosts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	outside := filepath.Dir(target)

	b := &tmbk.Backend{}
	// A snapshot entry that tries to climb out of the target.
	_ = b.Restore(context.Background(), backend.Destination{}, backupRoot,
		[]string{"/etc/../../../../../../etc/hosts", "../../etc/hosts"}, target)

	escaped, err := filepath.Glob(filepath.Join(outside, "etc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(escaped) != 0 {
		t.Errorf("a restore wrote outside its target: %v", escaped)
	}
}

func TestTheVolumeDirectoryIsNotPartOfTheLivePath(t *testing.T) {
	// A Time Machine backup wraps each volume in a directory of its own:
	// <backup>/<Macintosh HD>/Users/x/a.txt is /Users/x/a.txt on the disk.
	backupRoot := t.TempDir()
	inside := filepath.Join(backupRoot, "Macintosh HD", "Users", "x")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inside, "a.txt"), []byte("backed up"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := &tmbk.Backend{}
	var seen []string
	if err := b.Walk(context.Background(), backend.Destination{}, backupRoot, "", func(f backend.File) error {
		seen = append(seen, f.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "/Users/x/a.txt" {
		t.Fatalf("walked %v, want the live path without the volume directory", seen)
	}

	// And a restore of that live path finds it again inside the volume.
	target := t.TempDir()
	if err := b.Restore(context.Background(), backend.Destination{}, backupRoot, seen, target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "Users", "x", "a.txt"))
	if err != nil {
		t.Fatalf("the file was not restored: %v", err)
	}
	if string(got) != "backed up" {
		t.Errorf("restored %q", got)
	}
}

func TestANameWithANewlineIsNotAskedAbout(t *testing.T) {
	stubTmutil(t, `echo "[Included]  x"`)
	b := &tmbk.Backend{}
	_, err := b.Excluded(context.Background(), backend.Destination{}, []string{"/Users/x/a\nb"})
	if err == nil {
		t.Fatal("a path holding a newline cannot be told apart in tmutil's answer")
	}
	if !strings.Contains(err.Error(), "newline") {
		t.Errorf("error = %v, want it to name the reason", err)
	}
}

func TestAnAnswerAboutTheWrongPathIsRefused(t *testing.T) {
	// A forged extra line, or an answer that does not name the path asked about,
	// must not be matched to a path by position.
	stubTmutil(t, `case "$1" in
isexcluded) echo "[Included]  /somewhere/else"; echo "[Excluded]  /another/place";;
*) exit 1;;
esac`)
	b := &tmbk.Backend{}
	_, err := b.Excluded(context.Background(), backend.Destination{}, []string{"/Users/x", "/Users/y"})
	if err == nil {
		t.Fatal("answers naming other paths must be refused")
	}
}

func TestAFailedExclusionQuestionIsAnError(t *testing.T) {
	stubTmutil(t, `case "$1" in
isexcluded) echo "tmutil: broken" >&2; exit 1;;
*) exit 1;;
esac`)
	b := &tmbk.Backend{}
	if _, err := b.Excluded(context.Background(), backend.Destination{}, []string{"/Users/x"}); err == nil {
		t.Fatal("a non-zero exit must not read as 'not excluded'")
	}
}
