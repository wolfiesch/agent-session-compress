package main

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	btrfsSuperMagic = 0x9123683E
	// FIEMAP ioctl and flags from linux/fiemap.h.
	fsIocFiemap         = 0xC020660B
	fiemapFlagSync      = 0x1
	fiemapExtentLast    = 0x1
	fiemapExtentEncoded = 0x8
	fiemapHeaderSize    = 32
	fiemapExtentSize    = 56
	fiemapBatch         = 64
)

// btrfsBackend recompresses files with `btrfs filesystem defragment -czstd`,
// which the kernel applies in place, so a concurrent writer cannot lose data.
type btrfsBackend struct{}

func newBackend() Backend { return btrfsBackend{} }

func (btrfsBackend) Name() string { return "btrfs (defragment zstd)" }

func (btrfsBackend) Unsupported(root string) string {
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return root + ": " + err.Error()
	}
	if uint32(st.Type) != btrfsSuperMagic {
		return fmt.Sprintf("%s is not on btrfs (filesystem magic %#x); on Linux only btrfs supports transparent compression of existing files", root, uint32(st.Type))
	}
	if _, err := exec.LookPath("btrfs"); err != nil {
		return "the btrfs command is not installed (package btrfs-progs)"
	}
	return ""
}

// IsCompressed reports whether any extent of the file is stored encoded.
// FIEMAP_FLAG_SYNC flushes pending writes first, so a just-defragmented file
// reports its new extents.
func (btrfsBackend) IsCompressed(path string, _ fs.FileInfo) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, fiemapHeaderSize+fiemapBatch*fiemapExtentSize)
	var start uint64
	for {
		clear(buf)
		binary.LittleEndian.PutUint64(buf[0:], start)            // fm_start
		binary.LittleEndian.PutUint64(buf[8:], ^uint64(0)-start) // fm_length
		binary.LittleEndian.PutUint32(buf[16:], fiemapFlagSync)  // fm_flags
		binary.LittleEndian.PutUint32(buf[24:], fiemapBatch)     // fm_extent_count
		if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(&buf[0]))); errno != 0 {
			return false
		}
		mapped := binary.LittleEndian.Uint32(buf[20:]) // fm_mapped_extents
		if mapped == 0 {
			return false
		}
		for i := uint32(0); i < mapped; i++ {
			extent := buf[fiemapHeaderSize+i*fiemapExtentSize:]
			logical := binary.LittleEndian.Uint64(extent[0:])
			length := binary.LittleEndian.Uint64(extent[16:])
			flags := binary.LittleEndian.Uint32(extent[40:])
			if flags&fiemapExtentEncoded != 0 {
				return true
			}
			if flags&fiemapExtentLast != 0 {
				return false
			}
			start = logical + length
		}
	}
}

// Allocated is unknown: btrfs reports uncompressed sizes in st_blocks
// (`compsize` reads the real on-disk size).
func (btrfsBackend) Allocated(string, fs.FileInfo) int64 { return -1 }

func (btrfsBackend) OpenFiles([]string) map[string]bool { return nil }

func (btrfsBackend) Compress(candidates []*Candidate, report *Report) {
	for _, batch := range batches(candidates, 200) {
		args := []string{"filesystem", "defragment", "-czstd"}
		for _, c := range batch {
			args = append(args, c.Path)
		}
		if out, err := exec.Command("btrfs", args...).CombinedOutput(); err != nil {
			report.errorf("btrfs defragment: %v: %s", err, truncate(out))
		}
	}
}
