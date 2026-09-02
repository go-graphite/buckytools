package main

import (
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/go-graphite/buckytools/metrics"
	whisper "github.com/go-graphite/go-whisper"
)

// newSidecarMetric creates a compressed whisper file with an out-of-order
// sidecar: three points two seconds apart put the block watermark at base+4,
// then a point at base+1 lands in a hole the encoder cannot write in place.
func newSidecarMetric(t *testing.T) (path string, base int) {
	t.Helper()

	path = filepath.Join(t.TempDir(), "graphite", "some", "metric.wsp")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir: %s", err)
	}

	wsp, err := whisper.CreateWithOptions(
		path,
		whisper.MustParseRetentionDefs("1s:2h"),
		whisper.Sum, 0,
		&whisper.Options{
			Compressed: true, PointsPerBlock: 1200,
			IgnoreNowOnWrite: true, OutOfOrder: true,
		},
	)
	if err != nil {
		t.Fatalf("create: %s", err)
	}

	base = int(time.Now().Unix()) - 3600
	if err := wsp.UpdateMany([]*whisper.TimeSeriesPoint{
		{Time: base + 0, Value: 1},
		{Time: base + 2, Value: 2},
		{Time: base + 4, Value: 3},
	}); err != nil {
		t.Fatalf("update: %s", err)
	}
	if err := wsp.UpdateMany([]*whisper.TimeSeriesPoint{{Time: base + 1, Value: 7}}); err != nil {
		t.Fatalf("update out-of-order: %s", err)
	}
	if err := wsp.Close(); err != nil {
		t.Fatalf("close: %s", err)
	}

	if _, err := os.Stat(whisper.OutOfOrderSidecarPath(path)); err != nil {
		t.Fatalf("expected a sidecar to have been created: %s", err)
	}

	return path, base
}

// valueAt reads one interval straight out of the whisper file, with the sidecar
// moved aside so the read cannot be satisfied by the overlay.
func valueAt(t *testing.T, path string, interval int) float64 {
	t.Helper()

	if sidecar := whisper.OutOfOrderSidecarPath(path); fileExists(sidecar) {
		if err := os.Rename(sidecar, sidecar+".hidden"); err != nil {
			t.Fatalf("hide sidecar: %s", err)
		}
		defer os.Rename(sidecar+".hidden", sidecar)
	}

	wsp, err := whisper.Open(path)
	if err != nil {
		t.Fatalf("open: %s", err)
	}
	defer wsp.Close()

	ts, err := wsp.Fetch(interval-1, interval+1)
	if err != nil {
		t.Fatalf("fetch: %s", err)
	}
	for i, v := range ts.Values() {
		if ts.FromTime()+i*ts.Step() == interval {
			return v
		}
	}

	return math.NaN()
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestMergeOutOfOrder(t *testing.T) {
	path, base := newSidecarMetric(t)

	// The diverted point is not in the file itself yet.
	if got := valueAt(t, path, base+1); !math.IsNaN(got) {
		t.Fatalf("before merge: value at base+1 = %v; want NaN", got)
	}

	if err := mergeOutOfOrder(path); err != nil {
		t.Fatalf("mergeOutOfOrder: %s", err)
	}

	if fileExists(whisper.OutOfOrderSidecarPath(path)) {
		t.Error("sidecar still present after merge")
	}
	if got := valueAt(t, path, base+1); got != 7 {
		t.Errorf("after merge: value at base+1 = %v; want 7", got)
	}
	// Merging must not disturb the points that were already there.
	if got := valueAt(t, path, base+4); got != 3 {
		t.Errorf("after merge: value at base+4 = %v; want 3", got)
	}

	// No sidecar is a cheap no-op, not an error.
	if err := mergeOutOfOrder(path); err != nil {
		t.Errorf("mergeOutOfOrder without a sidecar: %s", err)
	}
}

func TestDeleteMetricRemovesSidecar(t *testing.T) {
	path, _ := newSidecarMetric(t)

	oldPrefix := Prefix
	Prefix = filepath.Dir(filepath.Dir(path))
	defer func() { Prefix = oldPrefix }()

	if err := deleteMetric(httptest.NewRecorder(), path, true); err != nil {
		t.Fatalf("deleteMetric: %s", err)
	}

	if fileExists(whisper.OutOfOrderSidecarPath(path)) {
		t.Error("sidecar survived the metric it belongs to")
	}
	// With the sidecar gone the now-empty directory can be swept too.
	if fileExists(filepath.Dir(path)) {
		t.Error("metric directory not cleaned up")
	}
}
