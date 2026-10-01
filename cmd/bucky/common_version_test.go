package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-graphite/buckytools/metrics"
)

func TestDeleteMetricVersionUsesConditionalTokenAndAuth(t *testing.T) {
	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("jwt-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldFile, oldClient, oldToken := APITokenFile, httpClient, apiTokenContent.token
	defer func() {
		APITokenFile, httpClient, apiTokenContent.token = oldFile, oldClient, oldToken
		apiTokenContent.once = sync.Once{}
	}()
	APITokenFile, apiTokenContent.token, apiTokenContent.once = tokenFile, "", sync.Once{}
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method=%s", r.Method)
		}
		if got := r.URL.Query().Get("version"); got != "1:2:3" {
			t.Errorf("version=%q", got)
		}
		if got := r.Header.Get(buckydAuthHeader); got != "jwt-token" {
			t.Errorf("auth=%q", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()
	if err := DeleteMetricVersion(s.Listener.Addr().String(), "metric.name", "1:2:3"); err != nil {
		t.Fatal(err)
	}
}

func runSyncForTest(ms *metricSyncer, job *syncJob) {
	ms.stat.nodes[job.SrcServer] = &syncPerNodeStat{}
	ms.stat.nodes[job.DstServer] = &syncPerNodeStat{}
	jobs := make(chan *syncJob, 1)
	jobs <- job
	close(jobs)
	throttles := map[string]chan struct{}{job.SrcServer: make(chan struct{}, 1)}
	var wg sync.WaitGroup
	wg.Add(1)
	go ms.sync(jobs, throttles, &wg)
	wg.Wait()
}

func TestSyncSuccessfulMoveUsesGETVersionForDelete(t *testing.T) {
	oldClient := httpClient
	defer func() { httpClient = oldClient }()
	httpClient = nil
	deleteVersion := ""
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			b, _ := json.Marshal(&metrics.MetricData{Name: "old", Size: 1, StorageVersion: "get-v"})
			w.Header().Set("X-Metric-Stat", string(b))
			_, _ = w.Write([]byte("x"))
			return
		}
		if r.Method == http.MethodDelete {
			deleteVersion = r.URL.Query().Get("version")
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer src.Close()
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer dst.Close()
	ms := newMetricSyncer(&metricSyncerFlags{delete: true})
	runSyncForTest(ms, &syncJob{SrcServer: src.Listener.Addr().String(), DstServer: dst.Listener.Addr().String(), OldName: "old", NewName: "new"})
	if deleteVersion != "get-v" {
		t.Fatalf("delete version=%q", deleteVersion)
	}
}

func TestSyncOffloadHeadsSourceBeforeCopyAndDeletesVersion(t *testing.T) {
	oldClient := httpClient
	defer func() { httpClient = oldClient }()
	httpClient = nil
	head, deleted := false, ""
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			head = true
			b, _ := json.Marshal(&metrics.MetricData{StorageVersion: "head-v"})
			w.Header().Set("X-Metric-Stat", string(b))
			return
		}
		if r.Method == http.MethodDelete {
			deleted = r.URL.Query().Get("version")
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer src.Close()
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !head {
			t.Error("copy before source HEAD")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer dst.Close()
	ms := newMetricSyncer(&metricSyncerFlags{delete: true, offloadFetch: true})
	runSyncForTest(ms, &syncJob{SrcServer: src.Listener.Addr().String(), DstServer: dst.Listener.Addr().String(), OldName: "old", NewName: "new"})
	if !head || deleted != "head-v" {
		t.Fatalf("head=%t deleted=%q", head, deleted)
	}
}

func TestSyncDeleteConflictLeavesJobRetryable(t *testing.T) {
	oldClient := httpClient
	defer func() { httpClient = oldClient }()
	httpClient = nil
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			b, _ := json.Marshal(&metrics.MetricData{Size: 1, StorageVersion: "v"})
			w.Header().Set("X-Metric-Stat", string(b))
			_, _ = w.Write([]byte("x"))
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusConflict)
		}
	}))
	defer src.Close()
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
	defer dst.Close()
	state := t.TempDir() + "/state"
	ms := newMetricSyncer(&metricSyncerFlags{delete: true, jobStateFile: state})
	ms.jobStateLogger = createLogger(state, 0)
	runSyncForTest(ms, &syncJob{SrcServer: src.Listener.Addr().String(), DstServer: dst.Listener.Addr().String(), OldName: "old", NewName: "new"})
	b, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Fatalf("conflicted job was marked complete: %s", b)
	}
}

func TestSyncDoesNotDeleteWhenDestinationPostFails(t *testing.T) {
	oldClient := httpClient
	defer func() { httpClient = oldClient }()
	httpClient = nil
	var deletes int32
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			b, _ := json.Marshal(&metrics.MetricData{Name: "old", Size: 1, StorageVersion: "v1"})
			w.Header().Set("X-Metric-Stat", string(b))
			_, _ = w.Write([]byte("x"))
		case http.MethodDelete:
			atomic.AddInt32(&deletes, 1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer src.Close()
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer dst.Close()
	ms := newMetricSyncer(&metricSyncerFlags{delete: true})
	ms.stat.nodes[src.Listener.Addr().String()] = &syncPerNodeStat{}
	ms.stat.nodes[dst.Listener.Addr().String()] = &syncPerNodeStat{}
	jobs := make(chan *syncJob, 1)
	jobs <- &syncJob{SrcServer: src.Listener.Addr().String(), DstServer: dst.Listener.Addr().String(), OldName: "old", NewName: "new"}
	close(jobs)
	throttles := map[string]chan struct{}{src.Listener.Addr().String(): make(chan struct{}, 1)}
	var wg sync.WaitGroup
	wg.Add(1)
	go ms.sync(jobs, throttles, &wg)
	wg.Wait()
	if got := atomic.LoadInt32(&deletes); got != 0 {
		t.Fatalf("deletes=%d", got)
	}
}

func TestDeleteMetricVersionPropagatesConflictAndLegacyHasNoToken(t *testing.T) {
	oldFile, oldClient := APITokenFile, httpClient
	defer func() { APITokenFile, httpClient = oldFile, oldClient }()
	APITokenFile = ""
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			if r.URL.RawQuery != "" {
				t.Errorf("legacy query=%q", r.URL.RawQuery)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusConflict)
	}))
	defer s.Close()
	if err := DeleteMetricVersion(s.Listener.Addr().String(), "metric", ""); err != nil {
		t.Fatal(err)
	}
	if err := DeleteMetricVersion(s.Listener.Addr().String(), "metric", "new"); err == nil {
		t.Fatal("409 conflict must prevent source delete")
	}
}

func TestSyncOffloadWithoutDeletionDoesNotRequireSourceHEAD(t *testing.T) {
	oldClient := httpClient
	defer func() { httpClient = oldClient }()
	httpClient = nil
	var sourceRequests, copies int32
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&sourceRequests, 1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer src.Close()
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&copies, 1)
		_, _ = w.Write([]byte("{}"))
	}))
	defer dst.Close()
	ms := newMetricSyncer(&metricSyncerFlags{offloadFetch: true})
	runSyncForTest(ms, &syncJob{SrcServer: src.Listener.Addr().String(), DstServer: dst.Listener.Addr().String(), OldName: "old", NewName: "new"})
	if atomic.LoadInt32(&sourceRequests) != 0 || atomic.LoadInt32(&copies) != 1 || atomic.LoadInt64(&ms.stat.copyError) != 0 {
		t.Fatal("copy without deletion required source metadata permission")
	}
}
