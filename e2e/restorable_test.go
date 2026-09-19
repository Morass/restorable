package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackendsListsWhatCanBeRead(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.writeFile("work/a.txt", "hello")
	w.restic(filepath.Join(w.Home, "work"))

	out := w.mustRun(0, "backends").Stdout
	contains(t, out, "restic")
	contains(t, out, "readable")
	contains(t, out, "no Time Machine destination is configured")
	contains(t, out, "covers")
}

func TestDoctorIsQuietWhenFreshAndLoudWhenStale(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.writeFile("work/a.txt", "hello")
	w.restic(filepath.Join(w.Home, "work"))

	fresh := w.mustRun(0, "doctor")
	contains(t, fresh.Stdout, "readable and fresher")
	contains(t, fresh.Stdout, "run `restorable drill`")

	stale := w.mustRun(1, "doctor", "--stale-after", "1ms")
	contains(t, stale.Stdout, "What is wrong")
	contains(t, stale.Stdout, "the last backup is")
}

func TestDoctorJSONCarriesTheSameAnswer(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.writeFile("work/a.txt", "hello")
	w.restic(filepath.Join(w.Home, "work"))

	type health struct {
		Destination struct {
			Backend      string `json:"backend"`
			State        string `json:"state"`
			LastOKSource string `json:"last_ok_source"`
			Snapshots    int    `json:"snapshots"`
		} `json:"destination"`
		Problem string `json:"problem"`
		NotUsed bool   `json:"not_used"`
	}
	type doctor struct {
		Destinations []health `json:"destinations"`
		Problems     int      `json:"problems"`
	}
	rep := decode[doctor](t, w.mustRun(0, "doctor", "--json").Stdout)
	if rep.Problems != 0 {
		t.Errorf("problems = %d, want 0", rep.Problems)
	}
	var sawRestic, sawUnused bool
	for _, h := range rep.Destinations {
		if h.Destination.Backend == "restic" {
			sawRestic = true
			if h.Destination.State != "ok" || h.Destination.Snapshots != 1 {
				t.Errorf("restic destination = %+v", h.Destination)
			}
			if h.Destination.LastOKSource != "restic snapshots" {
				t.Errorf("source = %q, want it named", h.Destination.LastOKSource)
			}
		}
		if h.Destination.Backend == "timemachine" && h.NotUsed {
			sawUnused = true
		}
	}
	if !sawRestic {
		t.Error("the restic destination is missing from the JSON")
	}
	if !sawUnused {
		t.Error("Time Machine should be reported as not used, not as broken")
	}
}

func TestCoverageFindsTheDirectoryNoBackupKeeps(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.writeFile("data/work/kept.txt", "hello")
	w.writeFile("data/elsewhere/lost.txt", strings.Repeat("x", 4096))
	w.restic(filepath.Join(w.Home, "data", "work"))
	data := filepath.Join(w.Home, "data")

	out := w.mustRun(0, "coverage", "--min-size", "1KB", data).Stdout
	contains(t, out, "protected by nothing")
	contains(t, out, "elsewhere")
	contains(t, out, "no backup")
	absent(t, out, "/work  ") // the backed-up directory is not a hole

	// --strict turns the same finding into an exit code for cron.
	strict := w.mustRun(1, "coverage", "--strict", "--min-size", "1KB", data)
	contains(t, strict.Stdout, "elsewhere")

	// And the whole answer is available as JSON.
	type finding struct {
		Path   string `json:"path"`
		Kind   string `json:"kind"`
		Bytes  int64  `json:"bytes"`
		Files  int    `json:"files"`
		Detail string `json:"detail"`
	}
	type report struct {
		Findings         []finding `json:"findings"`
		TotalFiles       int       `json:"total_files"`
		UnprotectedFiles int       `json:"unprotected_files"`
	}
	rep := decode[report](t, w.mustRun(0, "coverage", "--json", data).Stdout)
	if rep.UnprotectedFiles != 1 {
		t.Errorf("unprotected files = %d, want 1", rep.UnprotectedFiles)
	}
	if rep.TotalFiles < 2 {
		t.Errorf("total files = %d, want at least the two we wrote", rep.TotalFiles)
	}
	var found bool
	for _, f := range rep.Findings {
		if strings.HasSuffix(f.Path, "elsewhere") && f.Kind == "no-backup" {
			found = true
			if f.Files != 1 || f.Bytes != 4096 {
				t.Errorf("finding = %+v, want 1 file of 4096 bytes", f)
			}
		}
	}
	if !found {
		t.Errorf("the hole is missing from the JSON: %+v", rep.Findings)
	}
}

func TestTheBackupRepositoryItselfIsNotAHole(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.writeFile("data/work/a.txt", "hello")
	repo := w.restic(filepath.Join(w.Home, "data", "work"))
	// The repository lives inside the walked tree, as it does when someone backs
	// up their home directory to an external disk mounted under it.
	out := w.mustRun(0, "coverage", w.Home).Stdout
	if strings.Contains(out, filepath.Base(repo)+"  ") && strings.Contains(out, "no backup") {
		t.Errorf("the repository was reported as a hole:\n%s", out)
	}
	contains(t, out, "Skipped on purpose")
}

func TestCoverageExplainsATimeMachineExclusion(t *testing.T) {
	w := newWorld(t)
	w.writeFile("data/work/keep.txt", "hello")
	w.writeFile("data/Library/Caches/big.bin", strings.Repeat("c", 8192))
	// A Mac with Time Machine configured and its disk connected, excluding
	// caches the way every Mac does.
	w.stubTool("RESTORABLE_TMUTIL", "tmutil", `case "$1" in
destinationinfo) cat <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Destinations</key><array><dict>
<key>Name</key><string>Backup Disk</string><key>Kind</key><string>Local</string>
<key>ID</key><string>1111-2222</string><key>MountPoint</key><string>/Volumes/Backup Disk</string></dict></array></dict></plist>
PLIST
;;
isexcluded) shift; [ "$1" = "--" ] && shift; for p in "$@"; do case "$p" in
  */Caches) echo "[Excluded]  $p";;
  *) echo "[Included]  $p";;
esac; done;;
listlocalsnapshotdates) echo "Snapshot dates for all disks:"; echo "2099-01-01-120000";;
listbackups) echo "/Volumes/Backup Disk/Backups.backupdb/box/2099-01-01-120000";;
*) exit 1;;
esac`)

	out := w.mustRun(0, "coverage", "--min-size", "1KB", filepath.Join(w.Home, "data")).Stdout
	contains(t, out, "excluded")
	contains(t, out, "Caches")
	contains(t, out, "cache directory")
	absent(t, out, "work")
}

func TestDrillProvesTheBytesComeBackAndCatchesWhenTheyDoNot(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.writeFile("work/a.txt", "the original bytes")
	w.writeFile("work/b.txt", "more original bytes")
	w.restic(filepath.Join(w.Home, "work"))

	pass := w.mustRun(0, "drill", "--count", "5", "--seed", "1")
	contains(t, pass.Stdout, "came back byte for byte")
	contains(t, pass.Stdout, "receipt:")

	// The receipt is on disk, private, and history finds it.
	receipts := filepath.Join(w.Env["RESTORABLE_STATE_DIR"], "receipts")
	entries, err := os.ReadDir(receipts)
	if err != nil || len(entries) != 1 {
		t.Fatalf("receipts = %v (%v), want exactly one", entries, err)
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("receipt mode = %04o, want 0600", perm)
	}
	contains(t, w.mustRun(0, "history").Stdout, "pass")

	// Now the live file disagrees with the backup and has an older timestamp:
	// the backup holds bytes that are not the file's, which is a failure.
	p := w.writeFile("work/a.txt", "tampered bytes!!!!")
	old := filepath.Join(w.Home, "work", "a.txt")
	if err := os.Chtimes(old, timeLongAgo(), timeLongAgo()); err != nil {
		t.Fatal(err)
	}
	_ = p
	fail := w.mustRun(1, "drill", "--count", "5", "--seed", "1")
	contains(t, fail.Stdout, "DIFFERS")
	contains(t, fail.Stdout, "has not been edited since")
	contains(t, w.mustRun(0, "history").Stdout, "FAIL")
}

func TestDrillKeepsTheRestoredCopiesOnRequest(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.writeFile("work/a.txt", "hello")
	w.restic(filepath.Join(w.Home, "work"))

	target := filepath.Join(w.Home, "drilled")
	out := w.mustRun(0, "drill", "--count", "1", "--seed", "3", "--keep", "--target", target).Stdout
	contains(t, out, "kept in")
	found := false
	_ = filepath.Walk(target, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && strings.HasSuffix(p, "a.txt") {
			found = true
		}
		return nil
	})
	if !found {
		t.Errorf("--keep left nothing under %s", target)
	}
}

func TestADamagedRepositoryFailsTheDrill(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.writeFile("work/a.txt", strings.Repeat("payload ", 512))
	repo := w.restic(filepath.Join(w.Home, "work"))

	// Delete the pack files: the snapshot still lists the file, but the data is
	// gone — exactly the failure a drill exists to find.
	packs := filepath.Join(repo, "data")
	if err := os.RemoveAll(packs); err != nil {
		t.Fatal(err)
	}
	out := w.mustRun(1, "drill", "--count", "3", "--seed", "1")
	if !strings.Contains(out.Stdout, "NOT RESTORED") && !strings.Contains(out.Stdout, "failed") {
		t.Errorf("a repository with no data must fail the drill:\n%s\n%s", out.Stdout, out.Stderr)
	}
}

func TestALockedRepositoryIsReportedAsLocked(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.writeFile("work/a.txt", "hello")
	w.restic(filepath.Join(w.Home, "work"))
	w.Env["RESTIC_PASSWORD"] = "the-wrong-one"

	res := w.mustRun(1, "doctor")
	contains(t, res.Stdout, "locked")
	contains(t, res.Stdout, "passphrase")
	absent(t, res.Stdout, "the-wrong-one")
}

func TestAMissingRepositoryIsReportedAsNotThere(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.Env["RESTIC_REPOSITORY"] = filepath.Join(w.Home, "no-such-repo")
	w.Env["RESTIC_PASSWORD"] = "x"

	res := w.mustRun(1, "doctor")
	contains(t, res.Stdout, "not there")
	contains(t, res.Stdout, "mount the disk")
}

func TestDrillWithNothingToDrillSaysSo(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	res := w.mustRun(1, "drill")
	contains(t, res.Stderr, "no destination could be read")
}

func TestHelpCoversEveryCommand(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	top := w.mustRun(0, "help").Stdout
	for _, cmd := range []string{"coverage", "doctor", "drill", "history", "backends", "config"} {
		contains(t, top, cmd)
		body := w.mustRun(0, "help", cmd).Stdout
		contains(t, body, "restorable "+cmd)
		if !strings.Contains(body, "Example") && !strings.Contains(body, "Flags") {
			t.Errorf("help for %q has neither examples nor flags:\n%s", cmd, body)
		}
	}
	// A command's own --help works too, and an unknown command is a usage error.
	contains(t, w.mustRun(0, "drill", "--help").Stderr+w.mustRun(0, "drill", "--help").Stdout, "restorable drill")
	unknown := w.run("nonsense")
	if unknown.Code != 2 {
		t.Errorf("an unknown command exited %d, want 2", unknown.Code)
	}
	contains(t, w.mustRun(0, "version").Stdout, "restorable")
}

func TestConfigWritesAPrivateExampleThatLoads(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	path := strings.TrimSpace(w.mustRun(0, "config", "--path").Stdout)
	if !strings.HasPrefix(path, w.Home) {
		t.Fatalf("configuration path %q is outside the sandbox", path)
	}
	w.mustRun(0, "config", "--write")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("configuration mode = %04o, want 0600: it names a password command", perm)
	}
	// Writing twice must not overwrite what the user has edited.
	if res := w.run("config", "--write"); res.Code == 0 {
		t.Error("a second --write must refuse rather than overwrite")
	}
	// The example is a valid configuration: the tool still runs with it.
	contains(t, w.mustRun(0, "config").Stdout, "restic")
	// And a world-readable configuration is refused.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	res := w.run("doctor")
	if res.Code != 3 {
		t.Errorf("a world-readable configuration exited %d, want 3", res.Code)
	}
	contains(t, res.Stderr, "chmod 600")
}

func TestMachineAccessCanBeSwitchedOffCompletely(t *testing.T) {
	w := newWorld(t)
	w.Env["RESTORABLE_NO_MACHINE"] = "1"
	res := w.run("backends")
	if res.Code != 0 {
		t.Fatalf("backends exited %d with machine access off:\n%s%s", res.Code, res.Stdout, res.Stderr)
	}
	contains(t, res.Stdout, "No backup system was found")
}

// timeLongAgo is older than any snapshot a test makes, so a difference in bytes
// cannot be explained away as an edit.
func timeLongAgo() time.Time { return time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC) }

func TestAMachineWithNoBackupAtAllIsToldWhatToDo(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	delete(w.Env, "RESTIC_REPOSITORY")

	res := w.mustRun(1, "doctor")
	contains(t, res.Stdout, "nothing on this machine is backing anything up")
	contains(t, res.Stdout, "restorable config --write")
	absent(t, res.Stdout, "✗ :") // no empty name before the colon

	backends := w.mustRun(0, "backends")
	absent(t, backends.Stdout, "borg") // borg is not read yet; do not promise it
}

func TestAHangingToolDoesNotHangTheTool(t *testing.T) {
	w := newWorld(t)
	// A tmutil that never answers: the deadline must end it, not the user.
	w.stubTool("RESTORABLE_TMUTIL", "tmutil", "sleep 600")
	delete(w.Env, "RESTIC_REPOSITORY")
	w.Env["RESTORABLE_TIMEOUT"] = "1s"

	done := make(chan result, 1)
	go func() { done <- w.run("backends") }()
	select {
	case res := <-done:
		if res.Code == 0 && strings.Contains(res.Stdout, "readable") {
			t.Errorf("a tool that never answers must not read as a healthy backup:\n%s", res.Stdout)
		}
	case <-time.After(70 * time.Second):
		t.Fatal("the tool waited for a hanging tmutil instead of giving up")
	}
}

func TestARepositoryUrlWithCredentialsIsNeverEchoed(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	// A repository location that carries credentials, as several restic
	// backends allow. Nothing restorable prints or writes may hold the secret.
	// The repository answers through a stub, so the test needs no network.
	w.Env["RESTIC_REPOSITORY"] = "rest:https://alice:hunter2@backup.example.org/repo"
	w.Env["RESTIC_PASSWORD"] = "correct-horse-battery"
	w.Env["RESTORABLE_WIDTH"] = "200"
	w.stubTool("RESTORABLE_RESTIC", "restic", `echo '[{"time":"2026-09-19T10:00:00+02:00","paths":["`+w.Home+`"],"hostname":"'$(hostname)'","id":"aaaa","short_id":"aaaa"}]'`)

	for _, args := range [][]string{{"doctor"}, {"doctor", "--json"}, {"backends"}, {"coverage", w.Home}} {
		res := w.run(args...)
		for _, secret := range []string{"hunter2", "correct-horse-battery"} {
			if strings.Contains(res.Stdout+res.Stderr, secret) {
				t.Errorf("%v printed a secret:\n%s%s", args, res.Stdout, res.Stderr)
			}
		}
		if !strings.Contains(res.Stdout, "backup.example.org") && args[0] != "coverage" {
			t.Errorf("%v hid the whole repository; the user must still recognise it:\n%s", args, res.Stdout)
		}
	}
}

func TestADrillNeverRestoresOverLiveFiles(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.writeFile("work/a.txt", "the live bytes")
	w.restic(filepath.Join(w.Home, "work"))

	// Someone points --target at their own home, the worst thing they could do.
	res := w.run("drill", "--count", "3", "--seed", "1", "--target", w.Home)
	if res.Code != 0 {
		t.Fatalf("drill exited %d:\n%s%s", res.Code, res.Stdout, res.Stderr)
	}
	live, err := os.ReadFile(filepath.Join(w.Home, "work", "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(live) != "the live bytes" {
		t.Errorf("the live file was written over during a drill: %q", live)
	}
	// And the restore went into a directory of its own, which was cleaned up.
	entries, err := filepath.Glob(filepath.Join(w.Home, "restorable-drill-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a drill without --keep left %v behind", entries)
	}
}

func TestASecondConfigWriteDoesNotFollowASymlink(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	path := strings.TrimSpace(w.mustRun(0, "config", "--path").Stdout)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(w.Home, "somewhere-else.json")
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if res := w.run("config", "--write"); res.Code == 0 {
		t.Error("writing through a symlink must be refused")
	}
	if _, err := os.Stat(victim); err == nil {
		t.Error("config --write created the file the symlink pointed at")
	}
}

func TestAQuestionThatCannotBeAnsweredIsShownAsUnknown(t *testing.T) {
	w := newWorld(t)
	w.writeFile("data/work/a.txt", "hello")
	// A Time Machine whose exclusion question fails: neither cover nor a hole.
	w.stubTool("RESTORABLE_TMUTIL", "tmutil", `case "$1" in
destinationinfo) cat <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Destinations</key><array><dict>
<key>Name</key><string>Backup Disk</string><key>Kind</key><string>Local</string>
<key>ID</key><string>1111</string><key>MountPoint</key><string>/Volumes/Backup Disk</string></dict></array></dict></plist>
PLIST
;;
listbackups) echo "/Volumes/Backup Disk/Backups.backupdb/box/2099-01-01-120000";;
isexcluded) echo "tmutil: something went wrong" >&2; exit 1;;
*) exit 1;;
esac`)
	delete(w.Env, "RESTIC_REPOSITORY")

	res := w.mustRun(0, "coverage", filepath.Join(w.Home, "data"))
	contains(t, res.Stdout, "Could not tell")
	contains(t, res.Stdout, "could not ask")
	absent(t, res.Stdout, "protected by nothing")
}

func TestADrillSamplesFilesDeepInTheTreeNotJustTheTop(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	// One file at the top and many below it: a listing that does not recurse
	// would only ever sample the top one.
	w.writeFile("data/work/top.txt", "top")
	for i := 0; i < 20; i++ {
		w.writeFile(fmt.Sprintf("data/work/deep/sub%02d/file.txt", i), fmt.Sprintf("deep %d", i))
	}
	w.restic(filepath.Join(w.Home, "data", "work"))

	res := w.mustRun(0, "drill", "--count", "12", "--seed", "11", "--path", filepath.Join(w.Home, "data", "work"))
	if strings.Count(res.Stdout, "deep/sub") < 5 {
		t.Errorf("the sample did not reach the files below the top:\n%s", res.Stdout)
	}
	contains(t, res.Stdout, "came back byte for byte")
}

func TestADrillRefusesToWriteInsideTheRepositoryItIsReading(t *testing.T) {
	w := newWorld(t)
	w.noTimeMachine()
	w.writeFile("data/work/a.txt", "hello")
	repo := w.restic(filepath.Join(w.Home, "data", "work"))

	res := w.run("drill", "--count", "2", "--target", filepath.Join(repo, "restored"))
	if res.Code == 0 {
		t.Errorf("a drill wrote into the repository it was reading:\n%s", res.Stdout)
	}
	contains(t, res.Stderr, "pick somewhere else")
}

func TestStrictCoverageFailsWhenSomethingCouldNotBeJudged(t *testing.T) {
	w := newWorld(t)
	w.writeFile("data/work/a.txt", "hello")
	w.stubTool("RESTORABLE_TMUTIL", "tmutil", `case "$1" in
destinationinfo) cat <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Destinations</key><array><dict>
<key>Name</key><string>Backup Disk</string><key>Kind</key><string>Local</string>
<key>ID</key><string>1111</string><key>MountPoint</key><string>/Volumes/Backup Disk</string></dict></array></dict></plist>
PLIST
;;
listbackups) echo "/Volumes/Backup Disk/Backups.backupdb/box/2099-01-01-120000";;
isexcluded) echo "tmutil: broken" >&2; exit 1;;
*) exit 1;;
esac`)
	delete(w.Env, "RESTIC_REPOSITORY")

	res := w.run("coverage", "--strict", filepath.Join(w.Home, "data"))
	if res.Code != 1 {
		t.Errorf("--strict exited %d; an answer nobody could give is not a clean bill of health:\n%s", res.Code, res.Stdout)
	}
	contains(t, res.Stdout, "could not be judged")
}
