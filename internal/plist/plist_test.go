package plist_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/morass/restorable/internal/plist"
	"github.com/morass/restorable/internal/run"
)

const sample = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PreferencesVersion</key><integer>6</integer>
  <key>AutoBackup</key><true/>
  <key>Ratio</key><real>0.5</real>
  <key>Blob</key><data>aGVsbG8=</data>
  <key>Destinations</key>
  <array>
    <dict>
      <key>DestinationID</key><string>AAAA-BBBB</string>
      <key>BACKUP_COMPLETED_DATE</key><date>2026-09-18T22:15:00Z</date>
      <key>SnapshotDates</key>
      <array>
        <date>2026-09-17T10:15:00Z</date>
        <date>2026-09-18T22:00:00Z</date>
      </array>
    </dict>
  </array>
</dict>
</plist>`

func TestParseReadsEveryTypeIncludingDates(t *testing.T) {
	v, err := plist.Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	d, ok := plist.AsDict(v)
	if !ok {
		t.Fatalf("root is %T, want a dictionary", v)
	}
	if d["PreferencesVersion"] != int64(6) {
		t.Errorf("integer = %#v", d["PreferencesVersion"])
	}
	if d["AutoBackup"] != true {
		t.Errorf("bool = %#v", d["AutoBackup"])
	}
	if d["Ratio"] != 0.5 {
		t.Errorf("real = %#v", d["Ratio"])
	}
	if raw, ok := d["Blob"].([]byte); !ok || string(raw) != "hello" {
		t.Errorf("data = %#v, want the decoded bytes", d["Blob"])
	}
	dests := plist.Dicts(v, "Destinations")
	if len(dests) != 1 {
		t.Fatalf("destinations = %d, want 1", len(dests))
	}
	if got := plist.Str(dests[0], "DestinationID"); got != "AAAA-BBBB" {
		t.Errorf("id = %q", got)
	}
	when, ok := plist.Time(dests[0], "BACKUP_COMPLETED_DATE")
	if !ok {
		t.Fatal("a plist date must be read as a date")
	}
	if want := time.Date(2026, 9, 18, 22, 15, 0, 0, time.UTC); !when.Equal(want) {
		t.Errorf("date = %s, want %s", when, want)
	}
	dates := plist.Times(dests[0], "SnapshotDates")
	if len(dates) != 2 {
		t.Fatalf("snapshot dates = %d, want 2", len(dates))
	}
}

// TestABinaryPlistWithDatesIsRead is the regression test for the trap that
// plutil's JSON conversion refuses any plist holding a date — which is every
// real Time Machine preferences file.
func TestABinaryPlistWithDatesIsRead(t *testing.T) {
	if _, err := os.Stat("/usr/bin/plutil"); err != nil {
		t.Skip("plutil is only on macOS")
	}
	dir := t.TempDir()
	xmlPath := filepath.Join(dir, "in.plist")
	if err := os.WriteFile(xmlPath, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	binPath := filepath.Join(dir, "binary.plist")
	if err := os.WriteFile(binPath, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	// Turn it into a real binary plist, the shape macOS actually stores.
	res, err := run.Runner{}.Run(context.Background(), run.Plutil, "-convert", "binary1", binPath)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("could not make a binary plist: %v %s", err, res.Stderr)
	}
	head, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(head[:6]) != "bplist" {
		t.Fatalf("the fixture is not a binary plist: %q", head[:6])
	}
	// The JSON conversion this package used to rely on must indeed refuse it,
	// so this test proves the fix and not merely that the parser works.
	jsonRes, err := run.Runner{}.Run(context.Background(), run.Plutil, "-convert", "json", "-o", "-", binPath)
	if err != nil {
		t.Fatal(err)
	}
	if jsonRes.ExitCode == 0 {
		t.Fatal("plutil now converts dates to JSON; this test needs rewriting")
	}

	v, err := plist.FromFile(context.Background(), run.Runner{}, binPath)
	if err != nil {
		t.Fatalf("a binary plist with dates must still be read: %v", err)
	}
	dests := plist.Dicts(v, "Destinations")
	if len(dests) != 1 {
		t.Fatalf("destinations = %d, want 1", len(dests))
	}
	if _, ok := plist.Time(dests[0], "BACKUP_COMPLETED_DATE"); !ok {
		t.Error("the completed date must survive the round trip")
	}
}

func TestTheMachinesOwnTimeMachinePreferencesCanBeRead(t *testing.T) {
	const path = "/Library/Preferences/com.apple.TimeMachine.plist"
	if _, err := os.Stat(path); err != nil {
		t.Skip("no Time Machine preferences on this machine")
	}
	v, err := plist.FromFile(context.Background(), run.Runner{}, path)
	if err != nil {
		t.Skipf("preferences not readable by this user: %v", err)
	}
	if _, ok := plist.AsDict(v); !ok {
		t.Errorf("the real preferences parsed as %T, want a dictionary", v)
	}
}

func TestAnEmptyOrBrokenPlistFailsClearly(t *testing.T) {
	if _, err := plist.Parse([]byte("")); err == nil {
		t.Error("empty input must be an error")
	}
	if _, err := plist.Parse([]byte("<plist><dict><key>a</key><date>not a date</date></dict></plist>")); err == nil {
		t.Error("a date that is not a date must be an error")
	}
	v, err := plist.Parse([]byte(`<plist version="1.0"><dict></dict></plist>`))
	if err != nil {
		t.Fatalf("an empty dictionary is valid: %v", err)
	}
	if d, ok := plist.AsDict(v); !ok || len(d) != 0 {
		t.Errorf("value = %#v, want an empty dictionary", v)
	}
}

func TestUnknownElementsAreSkippedNotFatal(t *testing.T) {
	v, err := plist.Parse([]byte(`<plist version="1.0"><dict>
	  <key>Known</key><string>yes</string>
	  <key>Odd</key><uid>7</uid>
	</dict></plist>`))
	if err != nil {
		t.Fatalf("an unknown element must not fail the file: %v", err)
	}
	if plist.Str(v, "Known") != "yes" {
		t.Errorf("the known key was lost: %#v", v)
	}
}
