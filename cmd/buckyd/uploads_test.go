package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	whisper "github.com/go-graphite/go-whisper"
)

func TestHealMetricCleansUploads(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.wsp")
	rets := whisper.MustParseRetentionDefs("1s:2h")
	src, err := whisper.Create(source, rets, whisper.Sum, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Update(11, int(time.Now().Unix())-60); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	invalid := append([]byte(nil), data[:30]...)

	tests := []struct {
		name       string
		existing   bool
		data       []byte
		size       int
		wantStatus int
	}{
		{name: "backfill", existing: true, data: data, size: len(data), wantStatus: 200},
		{name: "compress", data: data, size: len(data), wantStatus: 200},
		{name: "truncated backfill", existing: true, data: data[:30], size: len(data), wantStatus: 400},
		{name: "truncated compression", data: data[:30], size: len(data), wantStatus: 400},
		{name: "invalid backfill", existing: true, data: invalid, size: len(invalid), wantStatus: 500},
		{name: "invalid compression", data: invalid, size: len(invalid), wantStatus: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldTmpDir, oldCompressed := tmpDir, compressed
			tmpDir, compressed = t.TempDir(), true
			t.Cleanup(func() { tmpDir, compressed = oldTmpDir, oldCompressed })
			keep := filepath.Join(tmpDir, "unrelated")
			if err := os.WriteFile(keep, nil, 0600); err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(t.TempDir(), "destination.wsp")
			if tt.existing {
				dst, err := whisper.CreateWithOptions(destination, rets, whisper.Sum, 0,
					&whisper.Options{Compressed: true, PointsPerBlock: 1200})
				if err != nil {
					t.Fatal(err)
				}
				if err := dst.Close(); err != nil {
					t.Fatal(err)
				}
			}
			stat, err := json.Marshal(map[string]any{"Name": "destination", "Size": tt.size})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest("POST", "/metrics/destination", bytes.NewReader(tt.data))
			r.Header.Set("Content-Type", "application/octet-stream")
			r.Header.Set("X-Metric-Stat", string(stat))
			w := httptest.NewRecorder()
			healMetric(w, r, destination)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", w.Code, tt.wantStatus, w.Body.String())
			}
			entries, err := os.ReadDir(tmpDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "unrelated" {
				t.Errorf("temporary files after request = %v, want only unrelated file", entries)
			}
		})
	}
}
