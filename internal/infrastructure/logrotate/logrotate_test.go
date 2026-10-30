package logrotate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func TestRotate_LeavesALogUnderTheLimitAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "box.log")
	write(t, path, "small\n")

	n, err := (&Rotator{Path: path, MaxBytes: 100, Keep: 3}).Rotate(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("Rotate = %d, %v; want 0, nil", n, err)
	}
	if got := read(t, path); got != "small\n" {
		t.Fatalf("log changed to %q", got)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("a generation was written for a log under the limit: %v", err)
	}
}

func TestRotate_IsANoOpWhenThereIsNoLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "box.log")

	n, err := (&Rotator{Path: path, MaxBytes: 1, Keep: 3}).Rotate(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("Rotate = %d, %v; want 0, nil", n, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("rotation created the log: %v", err)
	}
}

func TestRotate_MovesAnOversizedLogAsideAndEmptiesTheLiveFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "box.log")
	write(t, path, "0123456789")

	n, err := (&Rotator{Path: path, MaxBytes: 10, Keep: 3}).Rotate(context.Background())
	if err != nil || n != 10 {
		t.Fatalf("Rotate = %d, %v; want 10, nil", n, err)
	}
	if got := read(t, path+".1"); got != "0123456789" {
		t.Fatalf("box.log.1 = %q", got)
	}
	if got := read(t, path); got != "" {
		t.Fatalf("live log = %q, want empty", got)
	}
}

func TestRotate_ShiftsGenerationsAndDropsTheOldest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "box.log")
	write(t, path, "current-oversized")
	write(t, path+".1", "gen1")
	write(t, path+".2", "gen2")
	write(t, path+".3", "gen3")

	if _, err := (&Rotator{Path: path, MaxBytes: 5, Keep: 3}).Rotate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for suffix, want := range map[string]string{
		".1": "current-oversized", ".2": "gen1", ".3": "gen2",
	} {
		if got := read(t, path+suffix); got != want {
			t.Errorf("box.log%s = %q, want %q", suffix, got, want)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf("kept more than Keep generations: %v", err)
	}
}

// The reason for copy-then-truncate. launchd and systemd's append: open the
// log once per launch with O_APPEND and hand the descriptor to kiln as its
// stdout and stderr; rotation runs inside that very process. A rename would
// send the rest of the tick into box.log.1. Truncating in place keeps the
// descriptor pointed at the live file, and O_APPEND puts the next write at
// the new end — offset zero — rather than at the old offset behind a hole.
func TestRotate_AnAppendModeWriterKeepsWritingToTheLiveFileFromTheStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "box.log")
	held, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	if _, err := held.WriteString(strings.Repeat("x", 64)); err != nil {
		t.Fatal(err)
	}

	if _, err := (&Rotator{Path: path, MaxBytes: 32, Keep: 1}).Rotate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := held.WriteString("after\n"); err != nil {
		t.Fatal(err)
	}

	if got := read(t, path); got != "after\n" {
		t.Fatalf("live log = %q, want only what was written after rotation", got)
	}
	if got := read(t, path+".1"); got != strings.Repeat("x", 64) {
		t.Fatalf("box.log.1 = %q", got)
	}
}

func TestRotate_WithNoGenerationsKeptOnlyTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "box.log")
	write(t, path, "0123456789")

	if _, err := (&Rotator{Path: path, MaxBytes: 1, Keep: 0}).Rotate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); got != "" {
		t.Fatalf("live log = %q, want empty", got)
	}
	if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("Keep 0 still wrote a generation: %v", err)
	}
}

func TestNew_UsesTheBoxDefaults(t *testing.T) {
	r := New("/x/box.log")
	if r.MaxBytes != DefaultMaxBytes || r.Keep != DefaultKeep || r.Path != "/x/box.log" {
		t.Fatalf("New = %+v", r)
	}
}
