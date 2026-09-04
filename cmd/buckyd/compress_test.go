package main

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	whisper "github.com/go-graphite/go-whisper"
)

// go-carbon can create a metric between healMetric's existence check and the
// compress, and CompressTo refuses to overwrite it.  The copy must then land as
// a backfill instead of failing the request with "file already exists".
func TestCompressMetricFillsFileThatAppeared(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.wsp")
	dstPath := filepath.Join(dir, "dst.wsp")
	rets := whisper.MustParseRetentionDefs("1s:2h")
	base := int(time.Now().Unix()) - 3600

	srcw, err := whisper.Create(srcPath, rets, whisper.Sum, 0)
	if err != nil {
		t.Fatalf("create source: %s", err)
	}
	if err := srcw.UpdateMany([]*whisper.TimeSeriesPoint{
		{Time: base, Value: 1},
		{Time: base + 1, Value: 2},
	}); err != nil {
		t.Fatalf("update source: %s", err)
	}
	if err := srcw.Close(); err != nil {
		t.Fatalf("close source: %s", err)
	}

	// go-carbon got there first.
	dstw, err := whisper.CreateWithOptions(dstPath, rets, whisper.Sum, 0,
		&whisper.Options{Compressed: true, PointsPerBlock: 1200})
	if err != nil {
		t.Fatalf("create destination: %s", err)
	}
	if err := dstw.Close(); err != nil {
		t.Fatalf("close destination: %s", err)
	}

	compressTook, fillTook, err := compressMetric(srcPath, dstPath)
	if err != nil {
		t.Fatalf("compressMetric: %s", err)
	}
	if compressTook != 0 || fillTook == 0 {
		t.Errorf("expected the backfill fallback to run, got compress=%s fill=%s", compressTook, fillTook)
	}

	dst, err := whisper.Open(dstPath)
	if err != nil {
		t.Fatalf("open destination: %s", err)
	}
	defer dst.Close()
	ts, err := dst.Fetch(base-1, base+2)
	if err != nil {
		t.Fatalf("fetch: %s", err)
	}
	var got []float64
	for _, v := range ts.Values() {
		if !math.IsNaN(v) {
			got = append(got, v)
		}
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("source points did not reach the destination: %v", got)
	}
}
