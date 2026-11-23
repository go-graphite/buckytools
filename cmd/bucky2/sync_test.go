package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSyncEngine drives the engine against fake buckyd nodes: the destination
// rejects the first request with 429 and any batch above two names with 413;
// the source records fenced deletes.
func TestSyncEngine(t *testing.T) {
	backoffBase = time.Millisecond
	var mu sync.Mutex
	var deleted, sizes []string
	calls := 0

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/transfer/delete" {
			http.NotFound(w, r)
			return
		}
		var versions []TransferVersion
		if err := json.NewDecoder(r.Body).Decode(&versions); err != nil {
			t.Error(err)
		}
		mu.Lock()
		for _, v := range versions {
			deleted = append(deleted, v.Name)
		}
		mu.Unlock()
		fmt.Fprint(w, `{}`)
	}))
	defer source.Close()
	src := strings.TrimPrefix(source.URL, "http://")

	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/transfer/status":
			fmt.Fprint(w, `{"Sync":true,"SyncRecover":true}`)
		case "/v1/transfer/sync":
			var request SyncRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			mu.Lock()
			calls++
			first := calls == 1
			sizes = append(sizes, fmt.Sprint(len(request.Names)))
			mu.Unlock()
			if request.Source != source.URL || request.From != 1 || request.Until <= request.From || !request.Recover {
				t.Errorf("request = %+v", request)
			}
			if first {
				http.Error(w, "busy", 429)
				return
			}
			if len(request.Names) > 2 {
				http.Error(w, "too large", 413)
				return
			}
			reply := SyncReply{Bytes: 10}
			for _, name := range request.Names {
				row := SyncRow{Name: name, Status: syncCopied, Source: &TransferVersion{Name: name, ID: 1}}
				// go-carbon attaches a version to copied rows only. Keep one
				// on equal and merged here, as an earlier reply format did:
				// the client must still not delete those.
				switch name {
				case "m.equal":
					row.Status = syncEqual
				case "m.merged":
					row.Status = syncMerged
				case "m.wrongproof": // copied, but the proof names another metric
					row.Source = &TransferVersion{Name: "m.other", ID: 1}
				case "m.changed":
					row.Status, row.Source = "changed", nil
				case "m.absent":
					row.Status, row.Source = "absent_on_source", nil
				}
				reply.Rows = append(reply.Rows, row)
			}
			json.NewEncoder(w).Encode(reply)
		default:
			http.NotFound(w, r)
		}
	}))
	defer destination.Close()
	dst := strings.TrimPrefix(destination.URL, "http://")

	names := []string{"m.a", "m.absent", "m.b", "m.changed", "m.equal", "m.merged", "m.wrongproof"}
	flags := &syncFlags{workers: 2, batch: 8, delete: true, settle: time.Minute}
	// m.changed, m.equal, m.merged and m.wrongproof stay on the source, so a
	// delete run must not report success.
	err := flags.run(map[string]map[string][]string{dst: {src: names}})
	if err == nil || !strings.HasPrefix(err.Error(), "4 metrics") {
		t.Fatalf("err = %v, want 4 metrics left on the source", err)
	}

	// Only names copied whole, with a proof naming them, may be deleted.
	sort.Strings(deleted)
	if got, want := strings.Join(deleted, " "), "m.a m.b"; got != want {
		t.Errorf("deleted = %q, want %q", got, want)
	}
	// 7 names: 429 retry, 413 for 7, then 3+4 -> 413 each -> 1,2,2,2.
	if got, want := strings.Join(sizes, " "), "7 7 3 1 2 4 2 2"; got != want {
		t.Errorf("batch sizes = %q, want %q", got, want)
	}
}

// A store-wide refusal (423 whole-store copy fence) must fail the batch with
// one request, not one request per name.
func TestSyncDoesNotSplitStoreWideRefusal(t *testing.T) {
	calls := 0
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/transfer/status" {
			fmt.Fprint(w, `{"Sync":true,"SyncRecover":true}`)
			return
		}
		calls++
		http.Error(w, "fenced by whole-store copy", http.StatusLocked)
	}))
	defer node.Close()
	dst := strings.TrimPrefix(node.URL, "http://")
	flags := &syncFlags{workers: 1, batch: 8, settle: time.Minute}
	err := flags.run(map[string]map[string][]string{dst: {"src:4242": {"a", "b", "c", "d", "e", "f", "g", "h"}}})
	if err == nil || calls != 1 {
		t.Fatalf("err = %v, sync requests = %d, want a failure after 1 request", err, calls)
	}
}

// A metric already in place on the destination carries no deletion version:
// -delete cannot remove the source copy, so the run must fail, not pass.
func TestSyncDeleteWithoutVersionFails(t *testing.T) {
	var request SyncRequest
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/transfer/status":
			fmt.Fprint(w, `{"Sync":true,"SyncRecover":true}`)
		case "/v1/transfer/sync":
			json.NewDecoder(r.Body).Decode(&request)
			fmt.Fprint(w, `{"Rows":[{"Name":"m","Status":"equal"}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer node.Close()
	dst := strings.TrimPrefix(node.URL, "http://")
	jobs := map[string]map[string][]string{dst: {"src:4242": {"m"}}}

	flags := &syncFlags{workers: 1, batch: 10, delete: true, settle: time.Minute}
	if err := flags.run(jobs); err == nil || !strings.Contains(err.Error(), "left on the source") {
		t.Fatalf("err = %v, want a failure for the undeleted metric", err)
	}
	// Without -delete the same reply is a success and the window is bounded.
	flags = &syncFlags{workers: 1, batch: 10, settle: time.Minute}
	if err := flags.run(jobs); err != nil || request.From != 1 || request.Until <= 1 {
		t.Fatalf("err = %v, request = %+v", err, request)
	}
}

// A permanent delete failure must cost one request per 256-name chunk, not a
// request per name.
func TestDeleteChunksAndDoesNotAmplify(t *testing.T) {
	var mu sync.Mutex
	var sizes []string
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/transfer/status":
			fmt.Fprint(w, `{"Sync":true,"SyncRecover":true}`)
		case "/v1/transfer/sync":
			var request SyncRequest
			json.NewDecoder(r.Body).Decode(&request)
			var reply SyncReply
			for _, name := range request.Names {
				reply.Rows = append(reply.Rows, SyncRow{Name: name, Status: syncCopied, Source: &TransferVersion{Name: name}})
			}
			json.NewEncoder(w).Encode(reply)
		case "/v1/transfer/delete":
			var versions []TransferVersion
			json.NewDecoder(r.Body).Decode(&versions)
			mu.Lock()
			sizes = append(sizes, fmt.Sprint(len(versions)))
			mu.Unlock()
			// The source's whole-store copy fence: 409, like a per-metric
			// version conflict, but marked as store-wide.
			w.Header().Set("X-Buckyd-Store-Fence", "store-copy")
			http.Error(w, "fenced by whole-store copy", 409)
		}
	}))
	defer node.Close()
	host := strings.TrimPrefix(node.URL, "http://")
	names := make([]string, 300)
	for i := range names {
		names[i] = fmt.Sprintf("m.%03d", i)
	}
	flags := &syncFlags{workers: 1, batch: 300, delete: true}
	err := flags.run(map[string]map[string][]string{host: {host: names}})
	if err == nil || !strings.HasPrefix(err.Error(), "300 metrics") {
		t.Fatalf("err = %v, want 300 metrics left on the source", err)
	}
	if got := strings.Join(sizes, " "); got != "256 44" {
		t.Errorf("delete request sizes = %q, want 256 44", got)
	}
}

func TestSyncRejectsInvalidFlags(t *testing.T) {
	jobs := map[string]map[string][]string{"dst:4242": {"src:4242": {"m"}}}
	for name, flags := range map[string]*syncFlags{
		"-w":        {workers: 0, batch: 10},
		"-batch":    {workers: 1, batch: 0},
		"-interval": {workers: 1, batch: 10, guard: guardFlags{enabled: true, threshold: 0.75, interval: 0}},
	} {
		if err := flags.run(jobs); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A redirect must not carry the buckyd JWT to another host.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	hit := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer other.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	_, err := call("GET", strings.TrimPrefix(redirector.URL, "http://"), "/hashring", nil, nil, nil)
	if statusOf(err) != http.StatusTemporaryRedirect || hit {
		t.Fatalf("err = %v, redirect followed = %t", err, hit)
	}
}

func TestSyncRefusesOldServer(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Protocol":1}`)
	}))
	defer old.Close()
	dst := strings.TrimPrefix(old.URL, "http://")
	flags := &syncFlags{workers: 1, batch: 10, settle: time.Minute}
	err := flags.run(map[string]map[string][]string{dst: {"src:4242": {"m"}}})
	if err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("err = %v, want too old", err)
	}

	// A daemon with sync but without Recover must be refused for -delete
	// before anything moves, and accepted without it up to the first sync.
	syncs := 0
	noRecover := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/transfer/sync" {
			syncs++
			fmt.Fprint(w, `{"Rows":[{"Name":"m","Status":"absent_on_source"}]}`)
			return
		}
		fmt.Fprint(w, `{"Sync":true}`)
	}))
	defer noRecover.Close()
	jobs := map[string]map[string][]string{strings.TrimPrefix(noRecover.URL, "http://"): {"src:4242": {"m", "m"}}}
	flags = &syncFlags{workers: 1, batch: 10, delete: true, settle: time.Minute}
	if err := flags.run(jobs); err == nil || !strings.Contains(err.Error(), "too old for -delete") || syncs != 0 {
		t.Fatalf("err = %v, syncs = %d, want -delete refused before any sync", err, syncs)
	}
	// The duplicate name is sent once. A missing metric fails only when it
	// was requested explicitly.
	flags = &syncFlags{workers: 1, batch: 10, settle: time.Minute}
	if err := flags.run(jobs); err != nil || syncs != 1 {
		t.Fatalf("err = %v, syncs = %d", err, syncs)
	}
	flags = &syncFlags{workers: 1, batch: 10, settle: time.Minute, requireSource: true}
	if err := flags.run(jobs); err == nil {
		t.Fatal("explicitly requested metric absent on the source did not fail the run")
	}
}

// -h given as an IP of a ring member that advertises a hostname: with -s the
// seed must be addressed by its ring name, or its own metrics look misplaced.
func TestSingleHostUsesRingName(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Name":"localhost","Algo":"carbon","Nodes":[{"Server":"localhost"}]}`)
	}))
	defer node.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(node.URL, "http://"), ":")
	HostPort, SingleHost = "127.0.0.1:"+port, true
	defer func() { SingleHost = false }()
	servers, cluster, err := targetRing()
	if err != nil {
		t.Fatal(err)
	}
	if want := "localhost:" + port; len(servers) != 1 || servers[0] != want || cluster.Owner("any.metric") != want {
		t.Fatalf("servers = %v, owner = %s, want %s", servers, cluster.Owner("any.metric"), want)
	}
}

// A reply whose rows do not match the requested names must not delete.
func TestSyncRejectsMismatchedRows(t *testing.T) {
	deletes := 0
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/transfer/status":
			fmt.Fprint(w, `{"Sync":true,"SyncRecover":true}`)
		case "/v1/transfer/sync":
			fmt.Fprint(w, `{"Rows":[{"Name":"other","Status":"copied","Source":{"Name":"other","ID":1}}]}`)
		default:
			deletes++
		}
	}))
	defer node.Close()
	host := strings.TrimPrefix(node.URL, "http://")
	flags := &syncFlags{workers: 1, batch: 10, delete: true, settle: time.Minute}
	if err := flags.run(map[string]map[string][]string{host: {host: {"m"}}}); err == nil || deletes != 0 {
		t.Fatalf("err = %v, deletes = %d, want a failure and no delete", err, deletes)
	}
}
