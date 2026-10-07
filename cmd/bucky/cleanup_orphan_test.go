package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

func TestCleanupOrphan(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "metrics", "nested")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	longMetric := strings.Repeat("m", 251) + ".wsp"
	longHash := fmt.Sprintf(".%x", sha256.Sum256([]byte(longMetric)))
	longSidecarMetric := strings.Repeat("n", 246) + ".wsp"
	longSidecarLock := fmt.Sprintf(".%x.lock", sha256.Sum256([]byte(longSidecarMetric+".ooo")))
	orphanHash := fmt.Sprintf(".%x", sha256.Sum256([]byte("deleted long metric")))
	keep := []string{
		"live.wsp", "live.wsp.ooo", "live.wsp.ooo.lock", "live.wsp.lock",
		"live.ooo", "live.ooo.lock", "live.lock",
		"no-sidecar.wsp", "no-sidecar.wsp.lock", "no-sidecar.wsp.ooo.lock",
		"no-sidecar.lock", "no-sidecar.ooo.lock",
		"other.txt", "other.wsp.ooo.tmp", "other.wsp.lock.backup",
		longMetric, longHash + ".ooo", longHash + ".ooo.lock", longHash + ".lock",
		longSidecarMetric, longSidecarMetric + ".ooo", longSidecarMetric + ".lock", longSidecarLock,
		"gone.wsp.ooo.lock", "gone.wsp.lock", "gone.ooo.lock", "gone.lock",
		"lock-only.wsp.lock", "sidecar-lock-only.wsp.ooo.lock", "nonempty.wsp.lock",
		orphanHash + ".ooo.lock", orphanHash + ".lock",
		"parent.wsp.ooo.lock", "parent.wsp.lock",
	}
	remove := []string{
		"gone.wsp.ooo", "gone.ooo", "empty.wsp.ooo", orphanHash + ".ooo",
		// A metric with this name in the parent directory is a different owner.
		"parent.wsp.ooo",
	}
	for _, name := range append(append([]string{}, keep...), remove...) {
		data := []byte("data")
		if name == "empty.wsp.ooo" || strings.HasSuffix(name, ".lock") && name != "nonempty.wsp.lock" {
			data = nil
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0400); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "parent.wsp"), []byte("metric"), 0600); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "external.wsp.ooo")
	if err := os.WriteFile(outsideFile, []byte("external data"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"symlink.wsp.ooo":  outsideFile,
		"symlink.wsp.lock": outsideFile,
		"other-directory":  outside,
		"broken.wsp":       filepath.Join(outside, "missing.wsp"),
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		keep = append(keep, name)
	}
	// A .wsp directory entry is conservatively retained even for a broken symlink.
	for _, name := range []string{"broken.wsp.ooo", "broken.wsp.lock"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("data"), 0600); err != nil {
			t.Fatal(err)
		}
		keep = append(keep, name)
	}
	directoryName := "directory.wsp.ooo"
	if err := os.Mkdir(filepath.Join(dir, directoryName), 0755); err != nil {
		t.Fatal(err)
	}
	keep = append(keep, directoryName)

	for _, deleting := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete=%t", deleting), func(t *testing.T) {
			var output bytes.Buffer
			count, err := cleanOrphanSidecars(root, deleting, &output)
			if err != nil {
				t.Fatal(err)
			}
			if count != len(remove) {
				t.Fatalf("count=%d, want %d; output=%s", count, len(remove), output.String())
			}
			wantPaths := make([]string, 0, len(remove))
			for _, name := range remove {
				path := filepath.Join(dir, name)
				wantPaths = append(wantPaths, path)
				_, err := os.Lstat(path)
				if deleting && !os.IsNotExist(err) || !deleting && err != nil {
					t.Errorf("candidate %s: %v", name, err)
				}
			}
			sort.Strings(wantPaths)
			if want := strings.Join(wantPaths, "\n") + "\n"; output.String() != want {
				t.Errorf("output=%q, want %q", output.String(), want)
			}
			for _, name := range keep {
				if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
					t.Errorf("preserved file %s: %v", name, err)
				}
			}
			if data, err := os.ReadFile(outsideFile); err != nil || string(data) != "external data" {
				t.Errorf("symlink target changed: %q, %v", data, err)
			}
		})
	}
	if count, err := cleanOrphanSidecars(root, true, io.Discard); err != nil || count != 0 {
		t.Fatalf("repeat cleanup = %d, %v", count, err)
	}
}

func TestCleanupOrphanRefusesHeldFile(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{name: "busy.wsp.ooo", wantErr: true},
		{name: "busy.wsp.ooo.lock"},
		{name: "busy.wsp.lock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tt.name)
			file, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			if count, err := cleanOrphanSidecars(root, true, io.Discard); (err != nil) != tt.wantErr || count != 0 {
				t.Fatalf("held file cleanup = %d, %v; want error=%t", count, err, tt.wantErr)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCleanupOrphanInvalidRoot(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, filepath.Join(root, "missing")} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if count, err := cleanOrphanSidecars(path, true, io.Discard); err == nil || count != 0 {
				t.Fatalf("invalid root cleanup = %d, %v; want an error", count, err)
			}
		})
	}
}

func TestRemoveOrphanSidecarRefusesReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replaced.wsp.ooo")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeOrphanSidecar(path, info); err == nil {
		t.Fatal("removed a replacement file")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "replacement" {
		t.Fatalf("replacement changed: %q, %v", data, err)
	}
}
