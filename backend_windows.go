package main

import (
	"io/fs"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetCompressedFileSize = kernel32.NewProc("GetCompressedFileSizeW")
	procGetVolumeInformation  = kernel32.NewProc("GetVolumeInformationW")
)

// ntfsBackend applies Windows Overlay Filter compression with the built-in
// `compact /exe:lzx`. The kernel compresses in place, so a concurrent writer
// cannot lose data; a write decompresses the file.
type ntfsBackend struct{}

func newBackend() Backend { return ntfsBackend{} }

func (ntfsBackend) Name() string { return "ntfs (compact lzx)" }

func (ntfsBackend) Unsupported(root string) string {
	volume := filepath.VolumeName(root) + `\`
	volumePtr, err := syscall.UTF16PtrFromString(volume)
	if err != nil {
		return root + ": " + err.Error()
	}
	var name [32]uint16
	ok, _, callErr := procGetVolumeInformation.Call(uintptr(unsafe.Pointer(volumePtr)), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)))
	if ok == 0 {
		return "cannot read the filesystem of " + volume + ": " + callErr.Error()
	}
	if fsName := syscall.UTF16ToString(name[:]); fsName != "NTFS" {
		return volume + " is " + fsName + "; only NTFS supports compact /exe"
	}
	return ""
}

func (ntfsBackend) Allocated(path string, _ fs.FileInfo) int64 {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return -1
	}
	var high uint32
	low, _, callErr := procGetCompressedFileSize.Call(uintptr(unsafe.Pointer(pathPtr)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xFFFFFFFF && callErr != syscall.Errno(0) {
		return -1
	}
	return int64(high)<<32 | int64(uint32(low))
}

func (b ntfsBackend) IsCompressed(path string, info fs.FileInfo) bool {
	allocated := b.Allocated(path, info)
	return allocated >= 0 && allocated < info.Size()
}

func (ntfsBackend) OpenFiles([]string) map[string]bool { return nil }

func (ntfsBackend) Compress(candidates []*Candidate, report *Report) {
	for _, batch := range batches(candidates, 50) { // stay under the command-line length limit
		args := []string{"/c", "/exe:lzx", "/q"}
		for _, c := range batch {
			args = append(args, c.Path)
		}
		if out, err := exec.Command("compact", args...).CombinedOutput(); err != nil {
			report.errorf("compact: %v: %s", err, truncate(out))
		}
	}
}
