package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCleanOrphanLocks(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "metrics")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	longMetric := strings.Repeat("m", 251) + ".wsp"
	longLock := fmt.Sprintf(".%x.lock", sha256.Sum256([]byte(longMetric)))
	longSidecar := fmt.Sprintf(".%x.ooo", sha256.Sum256([]byte(longMetric)))
	longSidecarMetric := strings.Repeat("n", 246) + ".wsp"
	longSidecarLock := fmt.Sprintf(".%x.lock", sha256.Sum256([]byte(longSidecarMetric+".ooo")))
	orphanHash := fmt.Sprintf(".%x.lock", sha256.Sum256([]byte("deleted long metric")))
	keep := []string{
		"live.wsp", "live.wsp.lock", "live.wsp.ooo.lock",
		"sidecar-only.wsp.ooo", "sidecar-only.wsp.lock", "sidecar-only.wsp.ooo.lock",
		"unrelated.lock", "nonempty.wsp.lock",
		longMetric, longLock, longSidecar + ".lock",
		longSidecarMetric, longSidecarMetric + ".lock", longSidecarLock,
	}
	remove := []string{"gone.wsp.lock", "gone.wsp.ooo.lock", orphanHash}
	for _, name := range append(append([]string{}, keep...), remove...) {
		var data []byte
		if name == "nonempty.wsp.lock" {
			data = []byte("not a Whisper lock")
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	outsideLock := filepath.Join(outside, "outside.wsp.lock")
	if err := os.WriteFile(outsideLock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{"symlink.wsp.lock": outsideLock, "other-directory": outside} {
		if err := os.Symlink(target, filepath.Join(dir, link)); err != nil {
			t.Fatal(err)
		}
		keep = append(keep, link)
	}
	// A sidecar may outlive a long-named metric too.
	hashedSidecar := fmt.Sprintf(".%x.ooo", sha256.Sum256([]byte("deleted metric with sidecar")))
	for _, name := range []string{hashedSidecar, hashedSidecar + ".lock", strings.TrimSuffix(hashedSidecar, ".ooo") + ".lock"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
		keep = append(keep, name)
	}

	for _, deleting := range []bool{false, true} {
		var output bytes.Buffer
		count, err := cleanOrphanLocks(root, deleting, &output)
		if err != nil {
			t.Fatal(err)
		}
		if count != len(remove) {
			t.Fatalf("delete=%t: count=%d, want %d; output=%s", deleting, count, len(remove), output.String())
		}
		for _, name := range remove {
			path := filepath.Join(dir, name)
			if !strings.Contains(output.String(), path+"\n") {
				t.Errorf("missing candidate %s", name)
			}
			_, err := os.Lstat(path)
			if deleting && !os.IsNotExist(err) || !deleting && err != nil {
				t.Errorf("delete=%t: candidate %s: %v", deleting, name, err)
			}
		}
		for _, name := range keep {
			if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
				t.Errorf("preserved file %s: %v", name, err)
			}
		}
		if _, err := os.Stat(outsideLock); err != nil {
			t.Errorf("symlink target changed: %v", err)
		}
	}
	if count, err := cleanOrphanLocks(root, true, io.Discard); err != nil || count != 0 {
		t.Fatalf("repeat cleanup = %d, %v", count, err)
	}
}

func TestCleanOrphanLocksRefusesHeldLock(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "busy.wsp.lock")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if count, err := cleanOrphanLocks(root, true, io.Discard); err == nil || count != 0 {
		t.Fatalf("held lock cleanup = %d, %v; want an error", count, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveOfflineLockRefusesReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replaced.wsp.lock")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeOfflineLock(path, info); err == nil {
		t.Fatal("removed a replacement lock file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
