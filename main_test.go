package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const day = 24 * time.Hour

// writeTranscript writes a compressible JSONL file last modified age ago.
func writeTranscript(t *testing.T, path string, age time.Duration) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	for i := range 4000 {
		fmt.Fprintf(&buf, `{"type":"message","role":"assistant","index":%d,"text":"%s"}`+"\n", i, strings.Repeat("lorem ipsum ", 8))
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-age).Truncate(time.Second)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

type fingerprint struct {
	sum   [sha256.Size]byte
	mtime time.Time
}

func fingerprintOf(t *testing.T, path string) fingerprint {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint{sha256.Sum256(data), info.ModTime()}
}

func runJSON(t *testing.T, args ...string) (Report, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append(args, "--json"), &stdout, &stderr)
	var report Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decoding report (exit %d, stderr %q): %v", code, stderr.String(), err)
	}
	return report, code
}

// sessionRoot returns a temp directory, or skips when its filesystem has no
// transparent compression. Set ASC_TEST_DIR to test on another filesystem.
func sessionRoot(t *testing.T, requireSupport bool) string {
	t.Helper()
	base := os.Getenv("ASC_TEST_DIR")
	if base == "" {
		base = t.TempDir()
	} else {
		var err error
		if base, err = os.MkdirTemp(base, "asc-test-"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(base) })
	}
	root, _ := filepath.EvalSymlinks(base)
	if requireSupport {
		if reason := newBackend().Unsupported(root); reason != "" {
			t.Skip(reason)
		}
	}
	return root
}

func TestApplyCompressesIdleFilesKeepingBytesAndMtime(t *testing.T) {
	root := sessionRoot(t, true)
	old := writeTranscript(t, filepath.Join(root, "a", "old.jsonl"), 30*day)
	recent := writeTranscript(t, filepath.Join(root, "a", "recent.jsonl"), day)
	beforeOld, beforeRecent := fingerprintOf(t, old), fingerprintOf(t, recent)

	report, code := runJSON(t, "--path", root, "--apply")

	if code != 0 || report.Compressed != 1 || report.SkippedRecent != 1 {
		t.Fatalf("exit %d, report %+v", code, report)
	}
	if report.AllocatedBefore != nil && *report.AllocatedAfter >= *report.AllocatedBefore {
		t.Fatalf("on-disk size did not shrink: %d -> %d", *report.AllocatedBefore, *report.AllocatedAfter)
	}
	if got := fingerprintOf(t, old); got != beforeOld {
		t.Fatalf("compressed file changed: %+v -> %+v", beforeOld, got)
	}
	if got := fingerprintOf(t, recent); got != beforeRecent {
		t.Fatal("recent file was touched")
	}
	if again, _ := runJSON(t, "--path", root); again.AlreadyCompressed != 1 || again.Candidates != 0 {
		t.Fatalf("second run should find the file compressed: %+v", again)
	}
}

func TestDryRunLeavesFilesUntouched(t *testing.T) {
	root := sessionRoot(t, true)
	old := writeTranscript(t, filepath.Join(root, "old.jsonl"), 30*day)
	before := fingerprintOf(t, old)

	report, code := runJSON(t, "--path", root)

	if code != 0 || report.Candidates != 1 || report.Compressed != 0 {
		t.Fatalf("exit %d, report %+v", code, report)
	}
	if fingerprintOf(t, old) != before || newBackend().IsCompressed(old, mustStat(t, old)) {
		t.Fatal("dry run modified the file")
	}
}

func TestSkipsSmallAndPrecompressedFiles(t *testing.T) {
	root := sessionRoot(t, true)
	small := filepath.Join(root, "small.jsonl")
	if err := os.WriteFile(small, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	archive := writeTranscript(t, filepath.Join(root, "old.jsonl.gz"), 30*day)
	old := time.Now().Add(-30 * day)
	_ = os.Chtimes(small, old, old)

	report, _ := runJSON(t, "--path", root)

	if report.FilesScanned != 2 || report.Candidates != 0 {
		t.Fatalf("expected both files skipped: %+v (%s)", report, archive)
	}
}

func TestSkipsFilesHeldOpen(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("open-file check applies to the APFS backend only")
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	root := sessionRoot(t, true)
	held := writeTranscript(t, filepath.Join(root, "held.jsonl"), 30*day)
	f, err := os.OpenFile(held, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	report, _ := runJSON(t, "--path", root, "--apply")

	if report.SkippedOpen != 1 || report.Compressed != 0 {
		t.Fatalf("report %+v", report)
	}
}

func TestUnsupportedFilesystemFailsVisibly(t *testing.T) {
	root := sessionRoot(t, false)
	if newBackend().Unsupported(root) == "" {
		t.Skip("temp dir supports compression")
	}
	writeTranscript(t, filepath.Join(root, "old.jsonl"), 30*day)
	report, code := runJSON(t, "--path", root)
	if code != 1 || len(report.Errors) == 0 || report.FilesScanned != 0 {
		t.Fatalf("exit %d, report %+v", code, report)
	}
}

func TestHarnessNamesMixWithFlags(t *testing.T) {
	codexHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(codexHome, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	var stdout, stderr bytes.Buffer
	run([]string{"--idle-days", "3", "codex", "--json"}, &stdout, &stderr)
	var report Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("%v; stderr %q", err, stderr.String())
	}
	want, _ := filepath.EvalSymlinks(filepath.Join(codexHome, "sessions"))
	if len(report.Roots) != 1 || report.Roots[0] != want {
		t.Fatalf("roots %v, want [%s]", report.Roots, want)
	}
}

func TestUnknownHarnessIsRejected(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"nope"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "unknown harness") {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
