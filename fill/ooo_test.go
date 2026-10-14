package fill

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"testing"
	"time"

	whisper "github.com/go-graphite/go-whisper"
)

func TestCompressedFillClosesSidecar(t *testing.T) {
	// Lost descriptors must be released by fill, without help from finalizers.
	gcPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(gcPercent)

	dir := t.TempDir()
	source := filepath.Join(dir, "source.wsp")
	destination := filepath.Join(dir, "destination.wsp")
	base := int(time.Now().Unix()) - 3600
	rets := whisper.MustParseRetentionDefs("1s:2h")
	src, err := whisper.Create(source, rets, whisper.Sum, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Update(9, base+3); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	dst, err := whisper.CreateWithOptions(destination, rets, whisper.Sum, 0, &whisper.Options{
		Compressed: true, OutOfOrder: true, FLock: true,
		IgnoreNowOnWrite: true, PointsPerBlock: 1200,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.UpdateMany([]*whisper.TimeSeriesPoint{
		{Time: base, Value: 1}, {Time: base + 2, Value: 2}, {Time: base + 4, Value: 3},
	}); err != nil {
		t.Fatal(err)
	}
	if err := dst.UpdateMany([]*whisper.TimeSeriesPoint{{Time: base + 1, Value: 7}}); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	sidecar := whisper.OutOfOrderSidecarPath(destination)
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatal(err)
	}

	if err := All(source, destination); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{source, destination, sidecar, destination + ".lock", sidecar + ".lock"} {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Errorf("fill left %s locked: %v", filepath.Base(path), err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}

	// Read without the overlay to verify the late point reached the main file.
	if err := os.Rename(sidecar, sidecar+".saved"); err != nil {
		t.Fatal(err)
	}
	dst, err = whisper.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	series, err := dst.Fetch(base-1, base+5)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float64{1, 7, 2, 9, 3} {
		index := (base + i - series.FromTime()) / series.Step()
		if index < 0 || index >= len(series.Values()) {
			t.Fatalf("missing timestamp %d", base+i)
		}
		if got := series.Values()[index]; got != want {
			t.Errorf("value at base+%d = %v, want %v", i, got, want)
		}
	}
}
