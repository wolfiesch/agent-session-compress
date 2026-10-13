// Command agent-session-compress shrinks idle coding-agent session history
// with transparent filesystem compression.
//
// Session transcripts and tool logs (JSONL, JSON, plain-text logs) compress
// well, and the filesystem inflates them on read. Every file keeps its path,
// bytes, and mtime, so the agent that wrote it can still list, resume, and
// search it. Appending to a compressed file stores it uncompressed again.
package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

var version = "dev"

const (
	// scratchDir holds copies a backend writes beside the originals; a
	// leftover belongs to a crashed run and is never scanned.
	scratchDir = ".agent-session-compress.tmp"
	// minSize skips files that fit in one filesystem block: they cannot shrink.
	minSize = 4096
)

// skipSuffixes are formats whose bytes are already compressed, plus lock and
// database files that agents rewrite in place.
var skipSuffixes = []string{
	".gz", ".zst", ".xz", ".bz2", ".zip", ".7z",
	".png", ".jpg", ".jpeg", ".webp", ".gif", ".mp4", ".pdf",
	".lock", ".sqlite", ".sqlite-wal", ".sqlite-shm", ".db", ".db-wal", ".db-shm",
}

// Harness is a coding agent whose session history lives in known directories.
type Harness struct {
	Name, Title string
	Dirs        func() []string
}

func home(parts ...string) string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{dir}, parts...)...)
}

// envOr returns $env joined with sub, or the default home-relative path.
func envOr(env string, def []string, sub ...string) string {
	if base := os.Getenv(env); base != "" {
		return filepath.Join(append([]string{base}, sub...)...)
	}
	return home(append(def, sub...)...)
}

var harnesses = []Harness{
	{"claude", "Claude Code", func() []string { return []string{envOr("CLAUDE_CONFIG_DIR", []string{".claude"}, "projects")} }},
	{"codex", "OpenAI Codex CLI", func() []string {
		return []string{envOr("CODEX_HOME", []string{".codex"}, "sessions"), envOr("CODEX_HOME", []string{".codex"}, "archived_sessions")}
	}},
	{"gemini", "Gemini CLI", func() []string { return []string{home(".gemini", "tmp")} }},
	{"opencode", "opencode", func() []string {
		return []string{envOr("XDG_DATA_HOME", []string{".local", "share"}, "opencode", "storage")}
	}},
	{"omp", "Oh My Pi", func() []string { return []string{home(".omp", "agent", "sessions")} }},
	{"pi", "Pi", func() []string { return []string{envOr("PI_CODING_AGENT_DIR", []string{".pi", "agent"}, "sessions")} }},
}

func findHarness(name string) (Harness, bool) {
	for _, h := range harnesses {
		if h.Name == name {
			return h, true
		}
	}
	return Harness{}, false
}

// Candidate is a file selected for compression.
type Candidate struct {
	Path      string
	Size      int64
	ModTime   time.Time
	Allocated int64 // bytes on disk before compression; -1 when unknown
	sum       [sha256.Size]byte
}

// Report summarizes one run. Allocation fields are nil when the backend
// cannot measure on-disk size.
type Report struct {
	Backend           string   `json:"backend"`
	Apply             bool     `json:"apply"`
	Roots             []string `json:"roots"`
	FilesScanned      int      `json:"files_scanned"`
	SkippedRecent     int      `json:"skipped_recent"`
	SkippedOpen       int      `json:"skipped_open"`
	AlreadyCompressed int      `json:"already_compressed"`
	Candidates        int      `json:"candidates"`
	CandidateBytes    int64    `json:"candidate_bytes"`
	AllocatedBefore   *int64   `json:"allocated_before"`
	Compressed        int      `json:"compressed"`
	LeftUncompressed  int      `json:"left_uncompressed"`
	AllocatedAfter    *int64   `json:"allocated_after"`
	Errors            []string `json:"errors"`
}

func (r *Report) errorf(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

// Backend compresses files with one filesystem's transparent compression.
type Backend interface {
	Name() string
	// Unsupported returns why root cannot be compressed, or "".
	Unsupported(root string) string
	IsCompressed(path string, info fs.FileInfo) bool
	// Allocated returns the bytes path occupies on disk, or -1 when unknown.
	Allocated(path string, info fs.FileInfo) int64
	// OpenFiles returns paths under roots that a process holds open, or nil
	// when a concurrent writer cannot race this backend.
	OpenFiles(roots []string) map[string]bool
	Compress(candidates []*Candidate, report *Report)
}

func fileSum(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func batches[T any](items []T, size int) [][]T {
	var out [][]T
	for start := 0; start < len(items); start += size {
		out = append(out, items[start:min(start+size, len(items))])
	}
	return out
}

func skipName(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range skipSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func truncate(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func scan(roots []string, idle time.Duration, backend Backend, report *Report) []*Candidate {
	cutoff := time.Now().Add(-idle)
	busy := backend.OpenFiles(roots)
	var candidates []*Candidate
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if path != root {
					return nil // unreadable entry; keep walking
				}
				return err
			}
			if entry.IsDir() {
				if entry.Name() == scratchDir {
					return filepath.SkipDir
				}
				return nil
			}
			if !entry.Type().IsRegular() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			report.FilesScanned++
			if info.Size() <= minSize || skipName(entry.Name()) {
				return nil
			}
			if info.ModTime().After(cutoff) {
				report.SkippedRecent++
				return nil
			}
			if busy[path] {
				report.SkippedOpen++
				return nil
			}
			if backend.IsCompressed(path, info) {
				report.AlreadyCompressed++
				return nil
			}
			candidates = append(candidates, &Candidate{
				Path: path, Size: info.Size(), ModTime: info.ModTime(), Allocated: backend.Allocated(path, info),
			})
			return nil
		})
		if err != nil {
			report.errorf("%s: %v", root, err)
		}
	}
	report.Candidates = len(candidates)
	before, known := int64(0), len(candidates) > 0
	for _, c := range candidates {
		report.CandidateBytes += c.Size
		if c.Allocated < 0 {
			known = false
		}
		before += c.Allocated
	}
	if known {
		report.AllocatedBefore = &before
	}
	return candidates
}

// compress runs the backend, then confirms each file still holds its
// original bytes and restores its mtime.
func compress(candidates []*Candidate, backend Backend, report *Report) {
	live := candidates[:0:0]
	for _, c := range candidates {
		sum, err := fileSum(c.Path)
		if err != nil {
			report.errorf("%s: %v", c.Path, err)
			continue
		}
		c.sum = sum
		live = append(live, c)
	}
	backend.Compress(live, report)
	after, known := int64(0), len(live) > 0
	for _, c := range live {
		info, err := os.Lstat(c.Path)
		if err != nil {
			report.errorf("%s: %v", c.Path, err)
			known = false
			continue
		}
		sum, err := fileSum(c.Path)
		if err != nil || info.Size() != c.Size || sum != c.sum {
			if info.ModTime().Equal(c.ModTime) {
				report.errorf("%s: content changed during compression", c.Path)
			}
			known = false // an agent wrote to it meanwhile; leave it be
			continue
		}
		if !info.ModTime().Equal(c.ModTime) {
			if err := os.Chtimes(c.Path, time.Time{}, c.ModTime); err != nil {
				report.errorf("%s: restoring mtime: %v", c.Path, err)
			}
			if info, err = os.Lstat(c.Path); err != nil {
				report.errorf("%s: %v", c.Path, err)
				continue
			}
		}
		if backend.IsCompressed(c.Path, info) {
			report.Compressed++
		} else {
			report.LeftUncompressed++
		}
		allocated := backend.Allocated(c.Path, info)
		if allocated < 0 {
			known = false
		}
		after += allocated
	}
	if known {
		report.AllocatedAfter = &after
	}
}

func human(n *int64) string {
	if n == nil {
		return "?"
	}
	value := float64(*n)
	for _, unit := range []string{"B", "KB", "MB", "GB"} {
		if value < 1000 {
			if unit == "B" {
				return fmt.Sprintf("%d B", *n)
			}
			return fmt.Sprintf("%.1f %s", value, unit)
		}
		value /= 1000
	}
	return fmt.Sprintf("%.1f TB", value)
}

func resolveRoots(names []string, paths []string) []string {
	var roots []string
	for _, name := range names {
		h, _ := findHarness(name)
		for _, dir := range h.Dirs() {
			if info, err := os.Stat(dir); err == nil && info.IsDir() {
				roots = append(roots, dir)
			}
		}
	}
	roots = append(roots, paths...)
	var out []string
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		if !slices.Contains(out, abs) {
			out = append(out, abs)
		}
	}
	return out
}

type pathList []string

func (p *pathList) String() string     { return strings.Join(*p, ",") }
func (p *pathList) Set(v string) error { *p = append(*p, v); return nil }

func usage(flags *flag.FlagSet) func() {
	return func() {
		out := flags.Output()
		fmt.Fprintf(out, "Usage: agent-session-compress [HARNESS...] [--path DIR] [--idle-days N] [--apply] [--json]\n\n")
		fmt.Fprintf(out, "Shrink idle coding-agent session history with transparent filesystem compression.\n")
		fmt.Fprintf(out, "Dry run unless --apply. Without HARNESS or --path, every harness found is used.\n\nHarnesses:\n")
		for _, h := range harnesses {
			fmt.Fprintf(out, "  %-9s %s\n", h.Name, h.Title)
		}
		fmt.Fprintf(out, "\nOptions:\n")
		flags.PrintDefaults()
	}
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("agent-session-compress", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var paths pathList
	flags.Var(&paths, "path", "extra directory to compress (repeatable)")
	idleDays := flags.Float64("idle-days", 7, "only touch files unmodified for this many days")
	apply := flags.Bool("apply", false, "compress files (default: dry run)")
	list := flags.Bool("list", false, "show each harness's session directories and exit")
	asJSON := flags.Bool("json", false, "print the report as JSON")
	showVersion := flags.Bool("version", false, "print the version and exit")
	flags.Usage = usage(flags)

	// Accept harness names before, between, or after flags.
	var names []string
	for {
		if err := flags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0
			}
			return 2
		}
		if flags.NArg() == 0 {
			break
		}
		names = append(names, flags.Arg(0))
		args = flags.Args()[1:]
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agent-session-compress: "+format+"\n", a...)
		return 2
	}

	if *showVersion {
		fmt.Fprintln(stdout, "agent-session-compress", version)
		return 0
	}
	if *list {
		for _, h := range harnesses {
			for _, dir := range h.Dirs() {
				state := "missing"
				if info, err := os.Stat(dir); err == nil && info.IsDir() {
					state = "found"
				}
				fmt.Fprintf(stdout, "%-9s %-7s %s  (%s)\n", h.Name, state, dir, h.Title)
			}
		}
		return 0
	}
	for _, name := range names {
		if _, ok := findHarness(name); !ok {
			var known []string
			for _, h := range harnesses {
				known = append(known, h.Name)
			}
			return fail("unknown harness %q; choose from %s", name, strings.Join(known, ", "))
		}
	}
	for _, path := range paths {
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return fail("--path is not a directory: %s", path)
		}
	}
	if *idleDays < 0 {
		return fail("--idle-days must not be negative")
	}
	if len(names) == 0 && len(paths) == 0 {
		for _, h := range harnesses {
			names = append(names, h.Name)
		}
	}
	roots := resolveRoots(names, paths)
	if len(roots) == 0 {
		return fail("no session directories found; pass --path DIR or see --list")
	}

	backend := newBackend()
	report := &Report{Backend: backend.Name(), Apply: *apply, Roots: roots, Errors: []string{}}
	for _, root := range roots {
		if reason := backend.Unsupported(root); reason != "" {
			report.Errors = append(report.Errors, reason)
		}
	}
	if len(report.Errors) == 0 {
		idle := time.Duration(*idleDays * float64(24*time.Hour))
		candidates := scan(roots, idle, backend, report)
		if *apply && len(candidates) > 0 {
			compress(candidates, backend, report)
		}
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		mode := "dry run"
		if *apply {
			mode = "apply"
		}
		fmt.Fprintf(stdout, "backend: %s (%s)\n", report.Backend, mode)
		for _, root := range roots {
			fmt.Fprintf(stdout, "  %s\n", root)
		}
		if report.FilesScanned > 0 {
			size := human(&report.CandidateBytes)
			if report.AllocatedBefore != nil {
				size += ", " + human(report.AllocatedBefore) + " on disk"
			}
			fmt.Fprintf(stdout,
				"scanned %d files: %d to compress (%s), %d already compressed, %d modified within %g days, %d open\n",
				report.FilesScanned, report.Candidates, size,
				report.AlreadyCompressed, report.SkippedRecent, *idleDays, report.SkippedOpen)
		}
		switch {
		case *apply && report.Candidates > 0:
			fmt.Fprintf(stdout, "compressed %d files, %d left as-is", report.Compressed, report.LeftUncompressed)
			if report.AllocatedBefore != nil && report.AllocatedAfter != nil {
				fmt.Fprintf(stdout, "; on disk %s -> %s", human(report.AllocatedBefore), human(report.AllocatedAfter))
			}
			fmt.Fprintln(stdout)
		case report.Candidates > 0:
			fmt.Fprintln(stdout, "dry run: pass --apply to compress")
		}
		for _, e := range report.Errors {
			fmt.Fprintf(stderr, "error: %s\n", e)
		}
	}
	if len(report.Errors) > 0 {
		return 1
	}
	return 0
}

// unsupportedBackend reports every root as unsupported.
type unsupportedBackend struct{}

func (unsupportedBackend) Name() string { return "unsupported" }
func (unsupportedBackend) Unsupported(string) string {
	return "no transparent compression backend for " + runtime.GOOS
}
func (unsupportedBackend) IsCompressed(string, fs.FileInfo) bool { return false }
func (unsupportedBackend) Allocated(string, fs.FileInfo) int64   { return -1 }
func (unsupportedBackend) OpenFiles([]string) map[string]bool    { return nil }
func (unsupportedBackend) Compress([]*Candidate, *Report)        {}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}
