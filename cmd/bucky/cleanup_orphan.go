package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func init() {
	var remove bool
	c := NewCommand(func(c Command) int {
		if c.Flag.NArg() != 1 {
			log.Printf("Usage: bucky %s [-delete] <whisper-directory>", c.Name)
			return 2
		}
		count, err := cleanOrphanSidecars(c.Flag.Arg(0), remove, os.Stdout)
		if err != nil {
			log.Printf("Orphan cleanup stopped after %d files: %s", count, err)
			return 1
		}
		if remove {
			log.Printf("Removed %d orphan files", count)
		} else {
			log.Printf("Found %d orphan files (dry run)", count)
		}
		return 0
	}, "cleanup-orphan", "[-delete] <whisper-directory>", "Clean orphan Whisper sidecars.",
		`List orphan .ooo files recursively under a local
Whisper directory. Use -delete to remove them. Each candidate path is written
to STDOUT; the summary and errors are written to STDERR. No server is contacted.

Files are retained when the corresponding .wsp exists in the same directory.
Both metric.wsp.ooo and metric.ooo naming are supported, including hashed
auxiliary filenames. Only regular files are eligible; symlinks below the root
are skipped. All .lock and .ooo.lock files are left untouched.

Ensure metrics in this directory cannot be created or recreated during deletion;
the directory scan is not atomic with removal.`)
	c.Flag.BoolVar(&remove, "delete", false, "Remove orphan .ooo files.")
}

func cleanOrphanSidecars(root string, remove bool, output io.Writer) (int, error) {
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
		// Inventory each directory so hashed sidecars retain their metric owners.
		referenced := make(map[string]bool)
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasSuffix(name, ".wsp") {
				referenced[orphanSidecarName(name)] = true
				referenced[strings.TrimSuffix(name, ".wsp")+".ooo"] = true
			}
		}
		for _, entry := range entries {
			name := entry.Name()
			if referenced[name] || !strings.HasSuffix(name, ".ooo") || !entry.Type().IsRegular() {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				continue
			}
			path := filepath.Join(dir, name)
			if remove {
				if err := removeOrphanSidecar(path, info); err != nil {
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

// Match go-whisper's auxiliary filename fallback when appending exceeds NAME_MAX.
func orphanSidecarName(metricName string) string {
	if len(metricName)+len(".ooo") <= 255 {
		return metricName + ".ooo"
	}
	return fmt.Sprintf(".%x.ooo", sha256.Sum256([]byte(metricName)))
}

func removeOrphanSidecar(path string, expected os.FileInfo) error {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("lock sidecar %s: %w", path, err)
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(expected, opened) || !os.SameFile(opened, current) || !opened.Mode().IsRegular() {
		return fmt.Errorf("sidecar changed during cleanup: %s", path)
	}
	return os.Remove(path)
}
