package logging_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nlink-jp/bigquery-mcp/internal/logging"
)

func TestRotateOnStartup_ShiftsGenerations(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "server.log")

	// Seed: current + .1, .2, .3 with unique contents.
	mustWrite(t, base, "current")
	mustWrite(t, base+".1", "rot-1")
	mustWrite(t, base+".2", "rot-2")
	mustWrite(t, base+".3", "rot-3")

	if err := logging.RotateOnStartup(base, 5); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Errorf("base path should not exist after rotation, got err=%v", err)
	}
	if got := readFile(t, base+".1"); got != "current" {
		t.Errorf(".1 content: %q, want %q", got, "current")
	}
	if got := readFile(t, base+".2"); got != "rot-1" {
		t.Errorf(".2 content: %q, want %q", got, "rot-1")
	}
	if got := readFile(t, base+".4"); got != "rot-3" {
		t.Errorf(".4 content: %q, want %q", got, "rot-3")
	}
}

func TestRotateOnStartup_DropsOldestAtCap(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "server.log")
	keep := 3

	// Seed every slot.
	mustWrite(t, base, "current")
	mustWrite(t, base+".1", "rot-1")
	mustWrite(t, base+".2", "rot-2")
	mustWrite(t, base+".3", "rot-3")

	if err := logging.RotateOnStartup(base, keep); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	// Expect: base gone, .1=current, .2=rot-1, .3=rot-2, no .4
	if got := readFile(t, base+".3"); got != "rot-2" {
		t.Errorf(".3 content: %q, want %q", got, "rot-2")
	}
	if _, err := os.Stat(base + ".4"); !os.IsNotExist(err) {
		t.Errorf(".4 should be removed at cap, got err=%v", err)
	}
}

func TestRotateOnStartup_MissingCurrentOK(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "server.log")
	// Only old generations exist; current never created.
	mustWrite(t, base+".1", "rot-1")

	if err := logging.RotateOnStartup(base, 5); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	// .1 should have shifted to .2; no error.
	if got := readFile(t, base+".2"); got != "rot-1" {
		t.Errorf(".2 content: %q, want %q", got, "rot-1")
	}
}

func TestSetup_StderrOnlyWhenEmpty(t *testing.T) {
	logger, fh, err := logging.Setup("info", "")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if fh != nil {
		t.Errorf("expected nil file handle for stderr-only mode")
	}
	logger.Info("smoke")
}

func TestSetup_OpensAndWritesToFile(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "app.log")

	logger, fh, err := logging.Setup("debug", logFile)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer fh.Close()

	logger.Info("hello", "k", "v")

	data := readFile(t, logFile)
	if !strings.Contains(data, "hello") || !strings.Contains(data, "k=v") {
		t.Errorf("log file missing expected text: %q", data)
	}
}

func TestSetup_RotatesPriorRun(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "app.log")
	mustWrite(t, logFile, "from previous run\n")

	logger, fh, err := logging.Setup("info", logFile)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer fh.Close()

	logger.Info("new run")

	if got := readFile(t, logFile+".1"); !strings.HasPrefix(got, "from previous run") {
		t.Errorf("prior contents should have moved to .1: %q", got)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// Compare without the trailing newline that slog adds.
	return strings.TrimRight(string(b), "\n")
}

var _ = fmt.Sprintf // tests above use fmt indirectly via t.Errorf
