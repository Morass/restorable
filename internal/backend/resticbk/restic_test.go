package resticbk_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/morass/restorable/internal/backend"
	"github.com/morass/restorable/internal/backend/resticbk"
	"github.com/morass/restorable/internal/run"
)

// stubRestic installs a fake restic that records its arguments and environment
// and answers with the given script.
func stubRestic(t *testing.T, script string) (recorded func() string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	body := "#!/bin/sh\n{ echo \"ARGS: $*\"; env | sed 's/^/ENV: /'; } >> " + log + "\n" + script + "\n"
	path := filepath.Join(dir, "restic")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(run.Restic.EnvVar, path)
	return func() string {
		data, err := os.ReadFile(log)
		if err != nil {
			return ""
		}
		return string(data)
	}
}

const snapshotsJSON = `[
 {"time":"2026-09-18T10:00:00+02:00","paths":["/Users/x/Documents"],"hostname":"box","id":"aaaaaaaabbbbbbbb","short_id":"aaaaaaaa"},
 {"time":"2026-09-19T10:00:00+02:00","paths":["/Users/x/Documents","/Users/x/code"],"hostname":"box","id":"ccccccccdddddddd","short_id":"cccccccc"}
]`

func newBackend() *resticbk.Backend {
	return &resticbk.Backend{
		Repos:  []resticbk.Repo{{Name: "disk", Repo: "/Volumes/backup/restic", PasswordCommand: "echo hunter2"}},
		Runner: run.Runner{},
	}
}

func TestDestinationsReadSnapshotsAndTheirRoots(t *testing.T) {
	calls := stubRestic(t, `cat <<'JSON'
`+snapshotsJSON+`
JSON`)
	dests, err := newBackend().Destinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dests) != 1 {
		t.Fatalf("destinations = %d, want 1", len(dests))
	}
	d := dests[0]
	if d.State != backend.StateOK {
		t.Errorf("state = %q (%s), want ok", d.State, d.Err)
	}
	if d.Snapshots != 2 {
		t.Errorf("snapshots = %d, want 2", d.Snapshots)
	}
	want := time.Date(2026, 9, 19, 10, 0, 0, 0, time.FixedZone("", 2*3600))
	if !d.LastOK.Equal(want) {
		t.Errorf("last backup = %s, want %s (the newest snapshot)", d.LastOK, want)
	}
	if len(d.Roots) != 2 || d.Roots[0] != "/Users/x/Documents" || d.Roots[1] != "/Users/x/code" {
		t.Errorf("roots = %v, want the union of the snapshots' paths", d.Roots)
	}
	log := calls()
	if !strings.Contains(log, "RESTIC_REPOSITORY=/Volumes/backup/restic") {
		t.Error("the repository must be given to restic in its environment")
	}
	if !strings.Contains(log, "RESTIC_PASSWORD_COMMAND=echo hunter2") {
		t.Error("the user's own password command must be passed through")
	}
	if !strings.Contains(log, "--no-lock") {
		t.Error("reading a repository must not take a lock")
	}
}

func TestAWrongPassphraseReadsAsLockedNotBroken(t *testing.T) {
	stubRestic(t, `echo "Fatal: wrong password or no key found" >&2; exit 1`)
	dests, err := newBackend().Destinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dests[0].State != backend.StateLocked {
		t.Errorf("state = %q, want locked", dests[0].State)
	}
	if !strings.Contains(dests[0].Err, "passphrase") {
		t.Errorf("message = %q, want it to mention the passphrase", dests[0].Err)
	}
}

func TestAMissingRepositoryReadsAsNotThere(t *testing.T) {
	stubRestic(t, `echo "Fatal: unable to open config file: stat /Volumes/backup/restic/config: no such file or directory" >&2; exit 1`)
	dests, err := newBackend().Destinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dests[0].State != backend.StateUnreachable {
		t.Errorf("state = %q, want unreachable", dests[0].State)
	}
	if !strings.Contains(dests[0].Err, "not there") {
		t.Errorf("message = %q, want it to say the repository is not there", dests[0].Err)
	}
}

func TestUnreadableOutputIsAnError(t *testing.T) {
	stubRestic(t, `echo "not json at all"`)
	dests, err := newBackend().Destinations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dests[0].State != backend.StateError {
		t.Errorf("state = %q, want error", dests[0].State)
	}
}

func TestListSkipsTheSnapshotHeaderAndKeepsNodes(t *testing.T) {
	stubRestic(t, `cat <<'JSON'
{"time":"2026-09-19T10:00:00+02:00","short_id":"cccccccc","struct_type":"snapshot","message_type":"snapshot"}
{"name":"a.txt","type":"file","path":"/Users/x/Documents/a.txt","size":12,"struct_type":"node"}
{"name":"sub","type":"dir","path":"/Users/x/Documents/sub","struct_type":"node"}
JSON`)
	b := newBackend()
	dest := backend.Destination{Backend: backend.Restic, ID: "/Volumes/backup/restic"}
	files, err := b.List(context.Background(), dest, "cccccccc", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %+v, want the two nodes without the snapshot header", files)
	}
	if files[0].Path != "/Users/x/Documents/a.txt" || files[0].Size != 12 || files[0].Dir {
		t.Errorf("file = %+v", files[0])
	}
	if !files[1].Dir {
		t.Error("a directory node must be marked as one")
	}
}

func TestWalkStreamsEveryNode(t *testing.T) {
	stubRestic(t, `i=0; echo '{"struct_type":"snapshot","short_id":"cccccccc"}'; while [ $i -lt 500 ]; do echo "{\"name\":\"f$i\",\"type\":\"file\",\"path\":\"/x/f$i\",\"size\":3,\"struct_type\":\"node\"}"; i=$((i+1)); done`)
	b := newBackend()
	dest := backend.Destination{Backend: backend.Restic, ID: "/Volumes/backup/restic"}
	seen := 0
	err := b.Walk(context.Background(), dest, "cccccccc", "", func(f backend.File) error {
		seen++
		if f.Size != 3 {
			t.Fatalf("size = %d", f.Size)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 500 {
		t.Errorf("streamed %d nodes, want 500", seen)
	}
}

func TestRestoreAsksForEachFileAndFailsLoudly(t *testing.T) {
	calls := stubRestic(t, `exit 0`)
	b := newBackend()
	dest := backend.Destination{Backend: backend.Restic, ID: "/Volumes/backup/restic"}
	target := t.TempDir()
	if err := b.Restore(context.Background(), dest, "cccccccc", []string{"/x/a.txt", "/x/b.txt"}, target); err != nil {
		t.Fatal(err)
	}
	log := calls()
	if !strings.Contains(log, "--include /x/a.txt") || !strings.Contains(log, "--include /x/b.txt") {
		t.Errorf("restore did not ask for both files: %s", log)
	}
	if !strings.Contains(log, "--target "+target) {
		t.Errorf("restore did not use the target directory: %s", log)
	}

	stubRestic(t, `echo "Fatal: pack file not found" >&2; exit 1`)
	err := newBackend().Restore(context.Background(), dest, "cccccccc", []string{"/x/a.txt"}, target)
	if err == nil || !strings.Contains(err.Error(), "pack file not found") {
		t.Errorf("error = %v, want restic's own message", err)
	}
}

func TestRestoreRefusesToAskForNothing(t *testing.T) {
	stubRestic(t, `exit 0`)
	dest := backend.Destination{Backend: backend.Restic, ID: "/Volumes/backup/restic"}
	if err := newBackend().Restore(context.Background(), dest, "c", nil, t.TempDir()); err == nil {
		t.Error("restoring no files must be refused, not run as a whole-snapshot restore")
	}
}

func TestExclusionsCannotBeAnsweredByRestic(t *testing.T) {
	stubRestic(t, `exit 0`)
	_, err := newBackend().Excluded(context.Background(), backend.Destination{}, []string{"/x"})
	if err == nil {
		t.Fatal("restic cannot answer what it was told to exclude")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Errorf("error = %v, want the unsupported sentinel", err)
	}
}
