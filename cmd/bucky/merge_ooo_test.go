package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	whisper "github.com/go-graphite/go-whisper"
)

func createOOOMergeMetric(t *testing.T, path string) int {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	wsp, err := whisper.CreateWithOptions(path, whisper.MustParseRetentionDefs("1s:2h"), whisper.Sum, 0,
		&whisper.Options{Compressed: true, OutOfOrder: true, IgnoreNowOnWrite: true, PointsPerBlock: 1200})
	if err != nil {
		t.Fatal(err)
	}
	defer wsp.Close()
	base := int(time.Now().Unix()) - 3600
	if err := wsp.UpdateMany([]*whisper.TimeSeriesPoint{
		{Time: base, Value: 1}, {Time: base + 2, Value: 2}, {Time: base + 4, Value: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if err := wsp.UpdateMany([]*whisper.TimeSeriesPoint{{Time: base + 1, Value: 7}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(whisper.OutOfOrderSidecarPath(path)); err != nil {
		t.Fatalf("expected OOO sidecar: %v", err)
	}
	return base
}

func checkOOOMergeValues(t *testing.T, path string, base int) {
	t.Helper()
	if _, err := os.Stat(whisper.OutOfOrderSidecarPath(path)); !os.IsNotExist(err) {
		t.Fatalf("sidecar remains after merge: %v", err)
	}
	wsp, err := whisper.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer wsp.Close()
	series, err := wsp.Fetch(base-1, base+5)
	if err != nil {
		t.Fatal(err)
	}
	for offset, want := range []float64{1, 7, 2, math.NaN(), 3} {
		index := (base + offset - series.FromTime()) / series.Step()
		if index < 0 || index >= len(series.Values()) {
			t.Fatalf("missing timestamp %d", base+offset)
		}
		got := series.Values()[index]
		if got != want && !(math.IsNaN(got) && math.IsNaN(want)) {
			t.Errorf("value at base+%d = %v, want %v", offset, got, want)
		}
	}
}

func TestMergeOOOSidecars(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(root, "a.wsp"), filepath.Join(root, "nested", "b.wsp")}
	bases := make([]int, len(paths))
	for i, path := range paths {
		bases[i] = createOOOMergeMetric(t, path)
	}
	// An invalid metric without a sidecar must never be opened.
	for _, name := range []string{"no-sidecar.wsp", "orphan.wsp.ooo", "other.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("unchanged"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	count, err := mergeOOOSidecars(root, 0, 500, &output)
	if err != nil || count != 2 {
		t.Fatalf("count=%d, err=%v", count, err)
	}
	if want := strings.Join(paths, "\n") + "\n"; output.String() != want {
		t.Fatalf("output=%q, want %q", output.String(), want)
	}
	for i, path := range paths {
		checkOOOMergeValues(t, path, bases[i])
		// Persistent locks survive the rewrite and all held locks are released.
		for _, target := range []string{path, oooMergeLockPath(path), oooMergeLockPath(whisper.OutOfOrderSidecarPath(path))} {
			file, err := os.Open(target)
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Errorf("lock left held on %s: %v", target, err)
			}
			file.Close()
		}
	}
	for _, name := range []string{"no-sidecar.wsp", "orphan.wsp.ooo", "other.txt"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || string(data) != "unchanged" {
			t.Errorf("%s changed: %q, err=%v", name, data, err)
		}
	}
	count, err = mergeOOOSidecars(root, 0, 500, io.Discard)
	if count != 0 || err != nil {
		t.Fatalf("second run: count=%d, err=%v", count, err)
	}
}

func TestMergeOOOSkipsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.wsp")
	createOOOMergeMetric(t, outside)
	local := filepath.Join(root, "local.wsp")
	createOOOMergeMetric(t, local)
	if err := os.Remove(whisper.OutOfOrderSidecarPath(local)); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		filepath.Join(root, "linked.wsp"):       outside,
		filepath.Join(root, "linked.wsp.ooo"):   whisper.OutOfOrderSidecarPath(outside),
		whisper.OutOfOrderSidecarPath(local):    whisper.OutOfOrderSidecarPath(outside),
		filepath.Join(root, "linked-directory"): filepath.Dir(outside),
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	count, err := mergeOOOSidecars(root, 0, 500, io.Discard)
	if count != 0 || err != nil {
		t.Fatalf("count=%d, err=%v", count, err)
	}
	after, err := os.ReadFile(outside)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("external metric changed: %v", err)
	}
	if _, err := os.Stat(whisper.OutOfOrderSidecarPath(outside)); err != nil {
		t.Fatalf("external sidecar removed: %v", err)
	}
}

func TestMergeOOOMaxRunWhileLocked(t *testing.T) {
	for _, target := range []string{"metric", "metric-lock", "sidecar", "sidecar-lock"} {
		t.Run(target, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "metric.wsp")
			base := createOOOMergeMetric(t, path)
			sidecar := whisper.OutOfOrderSidecarPath(path)
			lockPath := map[string]string{
				"metric": path, "metric-lock": oooMergeLockPath(path),
				"sidecar": sidecar, "sidecar-lock": oooMergeLockPath(sidecar),
			}[target]
			file, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0666)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			count, err := mergeOOOSidecars(root, 30*time.Millisecond, 500, io.Discard)
			if count != 0 || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("count=%d, err=%v", count, err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("max-run did not interrupt lock wait: %s", elapsed)
			}
			if _, err := os.Stat(sidecar); err != nil {
				t.Fatalf("unmerged sidecar removed: %v", err)
			}
			file.Close()
			// A canceled lock wait must release every lock it already acquired.
			count, err = mergeOOOSidecars(root, time.Second, 500, io.Discard)
			if count != 1 || err != nil {
				t.Fatalf("retry: count=%d, err=%v", count, err)
			}
			checkOOOMergeValues(t, path, base)
		})
	}
}

func TestMergeOOOSpeed(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.wsp", "b.wsp", "c.wsp"} {
		createOOOMergeMetric(t, filepath.Join(root, name))
	}
	start := time.Now()
	count, err := mergeOOOSidecars(root, 0, 10, io.Discard)
	if count != 3 || err != nil {
		t.Fatalf("count=%d, err=%v", count, err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("three merges at speed=10 finished too quickly: %s", elapsed)
	}
}

func TestMergeOOOMaxRunWhileThrottled(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "a.wsp")
	base := createOOOMergeMetric(t, first)
	second := filepath.Join(root, "b.wsp")
	createOOOMergeMetric(t, second)
	start := time.Now()
	count, err := mergeOOOSidecars(root, 100*time.Millisecond, 1, io.Discard)
	if count != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("count=%d, err=%v", count, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("max-run did not interrupt rate limit wait: %s", elapsed)
	}
	checkOOOMergeValues(t, first, base)
	if _, err := os.Stat(whisper.OutOfOrderSidecarPath(second)); err != nil {
		t.Fatalf("second sidecar removed: %v", err)
	}
}

func TestMergeOOOExpiredMaxRun(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "metric.wsp")
	createOOOMergeMetric(t, path)
	count, err := mergeOOOSidecars(root, time.Nanosecond, 500, io.Discard)
	if count != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("count=%d, err=%v", count, err)
	}
	if _, err := os.Stat(whisper.OutOfOrderSidecarPath(path)); err != nil {
		t.Fatalf("sidecar removed after deadline: %v", err)
	}
}

func TestMergeOOORejectsInvalidInput(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		root   string
		maxRun time.Duration
		speed  int
	}{
		{name: "negative max-run", root: root, maxRun: -time.Second, speed: 500},
		{name: "zero speed", root: root},
		{name: "negative speed", root: root, speed: -1},
		{name: "missing root", root: filepath.Join(root, "missing"), speed: 500},
		{name: "file root", root: file, speed: 500},
	} {
		t.Run(tt.name, func(t *testing.T) {
			count, err := mergeOOOSidecars(tt.root, tt.maxRun, tt.speed, io.Discard)
			if count != 0 || err == nil {
				t.Fatalf("count=%d, err=%v", count, err)
			}
		})
	}
}

func TestMergeOOOFailurePreservesSidecar(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "metric.wsp")
	createOOOMergeMetric(t, path)
	sidecar := whisper.OutOfOrderSidecarPath(path)
	if err := os.WriteFile(sidecar, []byte("invalid"), 0644); err != nil {
		t.Fatal(err)
	}
	// A failure must not stop later metrics from merging.
	good := filepath.Join(root, "next.wsp")
	goodBase := createOOOMergeMetric(t, good)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	count, err := mergeOOOSidecars(root, 0, 500, io.Discard)
	if count != 1 || !errors.Is(err, errOOOMergeFailed) {
		t.Fatalf("count=%d, err=%v", count, err)
	}
	checkOOOMergeValues(t, good, goodBase)
	after, readErr := os.ReadFile(path)
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("metric changed after failed merge: %v", readErr)
	}
	if data, err := os.ReadFile(sidecar); err != nil || string(data) != "invalid" {
		t.Fatalf("invalid sidecar changed: %q, err=%v", data, err)
	}
}

func TestMergeOOOCommandDefaults(t *testing.T) {
	for _, c := range commands {
		if c.Name == "merge-ooo" {
			for name, want := range map[string]string{"max-run": "0s", "speed": "500"} {
				flag := c.Flag.Lookup(name)
				if flag == nil || flag.DefValue != want {
					t.Errorf("%s flag=%v, want default %s", name, flag, want)
				}
			}
			return
		}
	}
	t.Fatal("merge-ooo command is not registered")
}
