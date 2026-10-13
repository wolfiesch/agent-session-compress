package main

import (
	"bufio"
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// ufCompressed is the st_flags bit macOS sets on a transparently compressed file.
const ufCompressed = 0x20

// apfsBackend uses afsctool (LZFSE) when installed, else the built-in
// `ditto --hfsCompression` into a scratch copy that is verified and renamed
// over the original.
type apfsBackend struct{ afsctool string }

func newBackend() Backend {
	path, _ := exec.LookPath("afsctool")
	return &apfsBackend{afsctool: path}
}

func (b *apfsBackend) Name() string {
	if b.afsctool != "" {
		return "apfs (afsctool lzfse)"
	}
	return "apfs (ditto)"
}

func (b *apfsBackend) Unsupported(root string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return root + ": " + err.Error()
	}
	name := cString(st.Fstypename[:])
	if name != "apfs" && name != "hfs" {
		return root + " is on " + name + "; only APFS and HFS+ support transparent compression"
	}
	return ""
}

func cString(raw []int8) string {
	buf := make([]byte, 0, len(raw))
	for _, c := range raw {
		if c == 0 {
			break
		}
		buf = append(buf, byte(c))
	}
	return string(buf)
}

func (b *apfsBackend) IsCompressed(_ string, info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && st.Flags&ufCompressed != 0
}

func (b *apfsBackend) Allocated(_ string, info fs.FileInfo) int64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return -1
}

// OpenFiles lists open paths with lsof: rewriting a file from user space
// would drop bytes a concurrent writer appends.
func (b *apfsBackend) OpenFiles(roots []string) map[string]bool {
	open := map[string]bool{}
	// -n -P skip DNS and port-name lookups, which otherwise take seconds.
	out, _ := exec.Command("lsof", "-Fn", "-w", "-n", "-P").Output()
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "n") {
			continue
		}
		for _, root := range roots {
			if strings.HasPrefix(line[1:], root+string(filepath.Separator)) {
				open[line[1:]] = true
				break
			}
		}
	}
	return open
}

func (b *apfsBackend) Compress(candidates []*Candidate, report *Report) {
	if b.afsctool != "" {
		for _, batch := range batches(candidates, 200) {
			args := []string{"-c", "-T", "LZFSE", "-j4"}
			for _, c := range batch {
				args = append(args, c.Path)
			}
			if out, err := exec.Command(b.afsctool, args...).CombinedOutput(); err != nil {
				report.errorf("afsctool: %v: %s", err, truncate(out))
			}
		}
		return
	}
	byDir := map[string][]*Candidate{}
	var dirs []string
	for _, c := range candidates {
		dir := filepath.Dir(c.Path)
		if _, seen := byDir[dir]; !seen {
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], c)
	}
	for _, dir := range dirs {
		for _, batch := range batches(byDir[dir], 200) {
			dittoBatch(dir, batch, report)
		}
	}
}

func dittoBatch(dir string, batch []*Candidate, report *Report) {
	scratch := filepath.Join(dir, scratchDir)
	_ = os.RemoveAll(scratch)
	defer os.RemoveAll(scratch)
	if err := os.Mkdir(scratch, 0o700); err != nil {
		report.errorf("%s: %v", dir, err)
		return
	}
	args := []string{"--hfsCompression"}
	for _, c := range batch {
		args = append(args, c.Path)
	}
	if out, err := exec.Command("ditto", append(args, scratch)...).CombinedOutput(); err != nil {
		report.errorf("%s: ditto: %v: %s", dir, err, truncate(out))
		return
	}
	for _, c := range batch {
		copyPath := filepath.Join(scratch, filepath.Base(c.Path))
		current, err := os.Lstat(c.Path)
		if err != nil || current.Size() != c.Size || !current.ModTime().Equal(c.ModTime) {
			continue // written since selection; leave it
		}
		copied, err := os.Lstat(copyPath)
		if err != nil {
			report.errorf("%s: %v", copyPath, err)
			continue
		}
		sum, err := fileSum(copyPath)
		if err != nil || copied.Size() != c.Size || sum != c.sum {
			report.errorf("%s: compressed copy does not match the original", c.Path)
			continue
		}
		if st, ok := copied.Sys().(*syscall.Stat_t); ok && c.Allocated >= 0 && st.Blocks*512 >= c.Allocated {
			continue // did not shrink; keep the original
		}
		_ = os.Chmod(copyPath, current.Mode().Perm())
		if err := os.Chtimes(copyPath, current.ModTime(), current.ModTime()); err != nil {
			report.errorf("%s: %v", copyPath, err)
			continue
		}
		if err := os.Rename(copyPath, c.Path); err != nil {
			report.errorf("%s: %v", c.Path, err)
		}
	}
}
