// Package state keeps restorable's own small record: the receipts of past drills.
// It holds no file contents — only hashes, sizes and verdicts.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/morass/restorable/internal/drill"
)

// Dir is where receipts live, honouring XDG_STATE_HOME.
func Dir() (string, error) {
	if x := os.Getenv("RESTORABLE_STATE_DIR"); x != "" {
		return x, nil
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "restorable"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "restorable"), nil
}

func receiptsDir() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "receipts"), nil
}

// SaveReceipt writes one drill receipt and returns its path.
func SaveReceipt(r drill.Receipt) (string, error) {
	dir, err := receiptsDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s.json", r.RanAt.UTC().Format("20060102-150405"), safe(string(r.Backend)))
	path := filepath.Join(dir, name)
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// Receipts reads past drills, newest first.
func Receipts(limit int) ([]drill.Receipt, error) {
	dir, err := receiptsDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []drill.Receipt
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var r drill.Receipt
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].RanAt.After(out[j].RanAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// LastDrill returns when this machine last drilled a named destination. A
// destination with no identity has never been drilled, whatever the receipts say.
func LastDrill(destination string) (time.Time, bool) {
	if destination == "" {
		return time.Time{}, false
	}
	rs, err := Receipts(0)
	if err != nil {
		return time.Time{}, false
	}
	for _, r := range rs {
		if r.Destination == destination {
			return r.RanAt, true
		}
	}
	return time.Time{}, false
}

func safe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "backend"
	}
	return b.String()
}
