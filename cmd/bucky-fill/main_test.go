package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	whisper "github.com/go-graphite/go-whisper"
)

func TestFillFileMovesOutOfOrderData(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.wsp")
	destination := filepath.Join(dir, "destination.wsp")
	wsp, err := whisper.CreateWithOptions(
		source,
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
	base := int(time.Now().Unix()) - 3600
	if err := wsp.UpdateMany([]*whisper.TimeSeriesPoint{
		{Time: base, Value: 1},
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

	oldDeleteSourceFiles := deleteSourceFiles
	deleteSourceFiles = true
	defer func() { deleteSourceFiles = oldDeleteSourceFiles }()
	if err := fillFile(source, destination); err != nil {
		t.Fatalf("fillFile: %s", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Errorf("source still exists: %v", err)
	}
	if _, err := os.Stat(whisper.OutOfOrderSidecarPath(source)); !os.IsNotExist(err) {
		t.Errorf("source sidecar still exists: %v", err)
	}
	if _, err := os.Stat(whisper.OutOfOrderSidecarPath(destination)); !os.IsNotExist(err) {
		t.Errorf("destination sidecar still exists: %v", err)
	}

	wsp, err = whisper.Open(destination)
	if err != nil {
		t.Fatalf("open destination: %s", err)
	}
	defer wsp.Close()
	series, err := wsp.Fetch(base, base+2)
	if err != nil {
		t.Fatalf("fetch destination: %s", err)
	}
	if got := series.Values()[0]; math.IsNaN(got) || got != 7 {
		t.Errorf("destination late value = %v; want 7", got)
	}
}
