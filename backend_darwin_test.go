package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDittoFallbackWithoutAfsctool(t *testing.T) {
	root := sessionRoot(t, true)
	old := writeTranscript(t, filepath.Join(root, "old.jsonl"), 30*day)
	before := fingerprintOf(t, old)
	backend := &apfsBackend{} // no afsctool: use ditto
	report := &Report{}

	compress(scan([]string{root}, 7*day, backend, report), backend, report)

	if report.Compressed != 1 || len(report.Errors) > 0 {
		t.Fatalf("report %+v", report)
	}
	if fingerprintOf(t, old) != before {
		t.Fatal("ditto changed bytes or mtime")
	}
	if _, err := os.Stat(filepath.Join(root, scratchDir)); !os.IsNotExist(err) {
		t.Fatal("scratch directory left behind")
	}
}
