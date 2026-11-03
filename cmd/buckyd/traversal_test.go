package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	. "github.com/go-graphite/buckytools/metrics"
)

func TestServeMetricsRejectsInvalidPaths(t *testing.T) {
	oldPrefix, oldSecret := Prefix, authJWTSecretKey
	authJWTSecretKey = nil
	t.Cleanup(func() { Prefix, authJWTSecretKey = oldPrefix, oldSecret })

	paths := []string{
		"/metrics/",
		"/metrics/../target",
		"/metrics/namespace/../../target",
		"/metrics//target",
		"/metrics/namespace/target",
		"/metrics/%2e%2e%2ftarget",
		"/metrics/namespace%2F..%2Ftarget",
		"/metrics/..%5Ctarget",
		"/metrics/namespace%5ctarget",
		"/metrics/namespace%00target",
	}
	for _, method := range []string{http.MethodHead, http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		for _, path := range paths {
			t.Run(method+" "+path, func(t *testing.T) {
				Prefix = t.TempDir()
				filename := filepath.Join(Prefix, "target.wsp")
				original := []byte("existing metric data")
				if err := os.WriteFile(filename, original, 0600); err != nil {
					t.Fatal(err)
				}
				payload := bytes.Repeat([]byte("x"), 64)
				r := httptest.NewRequest(method, path, bytes.NewReader(payload))
				r.Header.Set("Content-Type", "application/octet-stream")
				r.Header.Set("X-Metric-Stat", fmt.Sprintf(`{"Size":%d}`, len(payload)))
				w := httptest.NewRecorder()
				serveMetrics(w, r)
				if w.Code != http.StatusBadRequest {
					t.Errorf("status = %d, want 400: %s", w.Code, w.Body.String())
				}
				if data, err := os.ReadFile(filename); err != nil || !bytes.Equal(data, original) {
					t.Errorf("existing metric changed: data = %q, error = %v", data, err)
				}
				entries, err := os.ReadDir(Prefix)
				if err != nil || len(entries) != 1 {
					t.Errorf("unexpected storage contents: entries = %v, error = %v", entries, err)
				}
			})
		}
	}
}

func TestServeMetricsValidPaths(t *testing.T) {
	oldPrefix, oldSecret := Prefix, authJWTSecretKey
	authJWTSecretKey = nil
	t.Cleanup(func() { Prefix, authJWTSecretKey = oldPrefix, oldSecret })

	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprintf("relative=%t", relative), func(t *testing.T) {
			Prefix = t.TempDir()
			if relative {
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				Prefix, err = filepath.Rel(cwd, Prefix)
				if err != nil {
					t.Fatal(err)
				}
			}
			filename := filepath.Join(Prefix, "namespace", "metric-with_underscores:42.wsp")
			if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
				t.Fatal(err)
			}
			data := []byte("existing metric data")
			if err := os.WriteFile(filename, data, 0600); err != nil {
				t.Fatal(err)
			}
			for _, method := range []string{http.MethodHead, http.MethodGet} {
				r := httptest.NewRequest(method, "/metrics/namespace.metric-with_underscores:42", nil)
				w := httptest.NewRecorder()
				serveMetrics(w, r)
				if w.Code != http.StatusOK {
					t.Fatalf("%s status = %d, want 200: %s", method, w.Code, w.Body.String())
				}
				if method == http.MethodGet && !bytes.Equal(w.Body.Bytes(), data) {
					t.Errorf("GET body = %q, want %q", w.Body.Bytes(), data)
				}
			}
		})
	}
}
