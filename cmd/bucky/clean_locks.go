package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	whisper "github.com/go-graphite/go-whisper"
)

func init() {
	var remove bool
	c := NewCommand(func(c Command) int {
		if c.Flag.NArg() != 1 {
			log.Printf("Usage: bucky clean-locks [-delete] <whisper-directory>")
			return 2
		}
		count, err := cleanOrphanLocks(c.Flag.Arg(0), remove, os.Stdout)
		if err != nil {
			log.Printf("Lock cleanup stopped after %d files: %s", count, err)
			return 1
		}
		if remove {
			log.Printf("Removed %d orphan lock files", count)
		} else {
			log.Printf("Found %d orphan lock files (dry run). Stop all storage processes before using -delete.", count)
		}
		return 0
	}, "clean-locks", "[-delete] <whisper-directory>", "Clean orphan Whisper lock files offline.",
		`List orphan lock files under a local Whisper directory. No server is contacted.

Use -delete only after stopping every process accessing this storage, including
go-carbon, buckyd, and local maintenance tools. An unlocked file alone does not
prove it is safe to remove: another process may already have opened its inode.

Only empty, regular Whisper lock files without a corresponding metric or sidecar
are eligible. Locks for existing metrics are retained even without a sidecar.
Hashed auxiliary filenames are supported. Symlinks below the root are skipped.`)
	c.Flag.BoolVar(&remove, "delete", false, "Remove orphan locks; requires all storage processes to be stopped.")
}

func cleanOrphanLocks(root string, remove bool, output io.Writer) (int, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s is not a directory", root)
	}
	count := 0
	var walk func(string) error
	walk = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		// Scope the inventory to each directory rather than the entire metric
		// tree. Include sidecars whose main file is already absent.
		referenced := make(map[string]bool)
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasSuffix(name, ".wsp") || strings.HasSuffix(name, ".ooo") {
				referenced[whisper.PathLockPath(name)] = true
			}
			if strings.HasSuffix(name, ".wsp") {
				sidecar := whisper.OutOfOrderSidecarPath(name)
				referenced[whisper.PathLockPath(sidecar)] = true
			}
			if strings.HasSuffix(name, ".ooo") {
				parent := strings.TrimSuffix(name, ".ooo")
				if isWhisperLockName(parent + ".lock") {
					referenced[whisper.PathLockPath(parent)] = true
				}
			}
		}
		for _, entry := range entries {
			name := entry.Name()
			if referenced[name] || !isWhisperLockName(name) || !entry.Type().IsRegular() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() != 0 {
				continue
			}
			path := filepath.Join(dir, name)
			if remove {
				if err := removeOfflineLock(path, info); err != nil {
					return err
				}
			}
			count++
			if _, err := fmt.Fprintln(output, path); err != nil {
				return err
			}
		}
		for _, entry := range entries {
			if entry.IsDir() {
				if err := walk(filepath.Join(dir, entry.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err = walk(root)
	return count, err
}

func isWhisperLockName(name string) bool {
	if strings.HasSuffix(name, ".wsp.lock") || strings.HasSuffix(name, ".wsp.ooo.lock") {
		return true
	}
	if !strings.HasSuffix(name, ".lock") {
		return false
	}
	base := strings.TrimSuffix(name, ".lock")
	if len(base) != 65 || base[0] != '.' || base != strings.ToLower(base) {
		return false
	}
	_, err := hex.DecodeString(base[1:])
	return err == nil
}

func removeOfflineLock(path string, expected os.FileInfo) error {
	file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock %s: %w; stop all storage processes before cleanup", path, err)
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(expected, opened) || !os.SameFile(opened, current) || opened.Size() != 0 {
		return fmt.Errorf("lock file changed during cleanup: %s", path)
	}
	return os.Remove(path)
}
