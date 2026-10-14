package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-graphite/buckytools/lock"
	whisper "github.com/go-graphite/go-whisper"
)

func init() {
	var maxRun time.Duration
	var speed int
	c := NewCommand(func(c Command) int {
		if c.Flag.NArg() != 1 || maxRun < 0 || speed <= 0 {
			log.Printf("Usage: bucky merge-ooo [-max-run duration] [-speed merges-per-second] <whisper-directory>; max-run must be non-negative and speed must be positive")
			return 2
		}
		count, err := mergeOOOSidecars(c.Flag.Arg(0), maxRun, speed, os.Stdout)
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("Merged %d sidecars; max-run reached", count)
			return 0
		}
		if err != nil {
			log.Printf("OOO merge stopped after %d sidecars: %s", count, err)
			return 1
		}
		log.Printf("Merged %d sidecars", count)
		return 0
	}, "merge-ooo", "[-max-run duration] [-speed merges-per-second] <whisper-directory>",
		"Merge local OOO sidecars into compressed Whisper files.",
		`Recursively merge .wsp.ooo sidecars into their compressed .wsp files.
Each merged .wsp path is written to STDOUT; the summary and errors are written
to STDERR. Orphans, symlinks below the root, and non-regular files are skipped.
No server is contacted.

The speed limit defaults to 500 merges per second. A max-run of 0s (the default)
allows the scan to finish. The duration includes scanning, rate limiting, and
waiting for locks. An ongoing merge finishes before stopping.

Exclusive locks are held on the metric, sidecar, and their persistent .lock
files until the rewrite finishes. Lock files are retained. Concurrent writers
must use persistent path locks across file replacement; stop writers that only
lock database inodes before running this command.`)
	c.Flag.DurationVar(&maxRun, "max-run", 0, "Maximum run duration; 0s means unlimited. An ongoing merge finishes before stopping.")
	c.Flag.IntVar(&speed, "speed", 500, "Maximum number of sidecar merges per second (must be positive).")
}

func mergeOOOSidecars(root string, maxRun time.Duration, speed int, output io.Writer) (int, error) {
	if maxRun < 0 || speed <= 0 {
		return 0, fmt.Errorf("max-run must be non-negative and speed must be positive")
	}
	ctx := context.Background()
	if maxRun > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, maxRun)
		defer cancel()
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	root, err = filepath.EvalSymlinks(root)
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
	interval := time.Second / time.Duration(speed)
	if time.Second%time.Duration(speed) != 0 {
		interval++
	}
	count := 0
	var nextMerge time.Time
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".wsp") {
			return nil
		}
		sidecar, err := os.Lstat(whisper.OutOfOrderSidecarPath(path))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !sidecar.Mode().IsRegular() {
			return nil
		}
		if err := waitForOOOMerge(ctx, time.Until(nextMerge)); err != nil {
			return err
		}
		err = mergeOOOFile(ctx, path)
		if os.IsNotExist(err) {
			// A writer or another maintenance process removed the candidate.
			return nil
		}
		if err != nil {
			return fmt.Errorf("merge %s: %w", path, err)
		}
		count++
		nextMerge = time.Now().Add(interval)
		if _, err := fmt.Fprintln(output, path); err != nil {
			return err
		}
		return ctx.Err()
	})
	return count, err
}

func mergeOOOFile(ctx context.Context, path string) error {
	sidecar := whisper.OutOfOrderSidecarPath(path)
	// Hold our own descriptors: go-whisper closes its handles during a rewrite.
	// Match the writer's lock order: metric path, metric inode, sidecar path, inode.
	for _, target := range []struct {
		path   string
		create bool
	}{
		{oooMergeLockPath(path), true}, {path, false},
		{oooMergeLockPath(sidecar), true}, {sidecar, false},
	} {
		file, err := lockOOOMergeFile(ctx, target.path, target.create)
		if err != nil {
			return err
		}
		defer file.Close()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// The external locks cover both files, including the sidecar after go-whisper
	// closes it. Taking go-whisper's locks again would deadlock on our descriptors.
	flags := os.O_RDWR | syscall.O_NOFOLLOW
	wsp, err := whisper.OpenWithOptions(path, &whisper.Options{OpenFileFlag: &flags})
	if err != nil {
		return err
	}
	return errors.Join(wsp.MergeOutOfOrder(), wsp.Close())
}

func oooMergeLockPath(path string) string {
	name := filepath.Base(path)
	if len(name)+len(".lock") <= 255 {
		return path + ".lock"
	}
	return filepath.Join(filepath.Dir(path), fmt.Sprintf(".%x.lock", sha256.Sum256([]byte(name))))
}

func lockOOOMergeFile(ctx context.Context, path string, create bool) (file *os.File, err error) {
	flags := os.O_RDWR | syscall.O_NOFOLLOW
	if create {
		flags |= os.O_CREATE
	}
	file, err = os.OpenFile(path, flags, 0666)
	if err != nil {
		return nil, err
	}
	defer func(openedFile *os.File) {
		if err != nil {
			openedFile.Close()
		}
	}(file)
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	for {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		err = lock.TryExclusive(file)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !lock.IsResourceUnavailable(err) {
			return nil, err
		}
		if err = waitForOOOMerge(ctx, 10*time.Millisecond); err != nil {
			return nil, err
		}
	}
	current, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(opened, current) {
		return nil, fmt.Errorf("file changed while locking: %s", path)
	}
	return file, nil
}

func waitForOOOMerge(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
