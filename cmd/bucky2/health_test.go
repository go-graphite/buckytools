package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheGuardGate(t *testing.T) {
	const node = "n:4242"
	g := newCacheGuard(guardFlags{enabled: true, threshold: 0.75, protocol: "http", port: "8080"}, []string{node}, 8)
	gate := g.nodes[node]
	steps := []struct {
		size   int64
		err    error
		limit  int
		paused bool
	}{
		{size: 10, limit: 8},               // idle: stays at -w
		{size: 80, limit: 4},               // over threshold: halve
		{size: 80, limit: 2},               //
		{size: 80, limit: 1},               //
		{size: 80, limit: 1},               // floor
		{size: 10, limit: 2},               // recover by one
		{size: 96, limit: 2, paused: true}, // pause at 95%
		{size: 80, limit: 2, paused: true}, // stays paused until under threshold
		{size: 10, limit: 3},               // resumed
		{size: 96, limit: 3, paused: true}, //
		{err: errors.New("x"), limit: 3, paused: true},
		{err: errors.New("x"), limit: 3, paused: true},
		{err: errors.New("x"), limit: 1}, // third failed poll: resume at gate 1
	}
	for i, step := range steps {
		g.update(node, cacheInfo{Size: step.size, Limit: 100}, step.err)
		if gate.limit != step.limit || gate.paused != step.paused {
			t.Fatalf("step %d: limit=%d paused=%t, want %d %t", i, gate.limit, gate.paused, step.limit, step.paused)
		}
	}
	if got := g.summary(); got != node+"=1" {
		t.Errorf("summary = %q", got)
	}

	// A cache without a limit never throttles.
	// A valid poll reporting no limit clears a pause and any throttle: the
	// limit that caused them is gone.
	g.update(node, cacheInfo{Size: 96, Limit: 100}, nil)
	if !gate.paused {
		t.Fatal("96% did not pause the node")
	}
	g.update(node, cacheInfo{Size: 5}, nil)
	if gate.limit != 8 || gate.paused {
		t.Fatalf("no-limit poll left the gate throttled: %+v", gate)
	}
	// A byte-limited cache is guarded by its byte fill.
	g.update(node, cacheInfo{EstimatedBytes: 96, ByteLimit: 100}, nil)
	if !gate.paused {
		t.Fatal("byte fill of 96% did not pause the node")
	}
}

func TestCacheGuardBlocksWhilePaused(t *testing.T) {
	var size atomic.Int64
	size.Store(99)
	carbon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/info" || r.URL.Query().Get("scopes") != "cache" {
			t.Errorf("unexpected request %s", r.URL)
		}
		fmt.Fprintf(w, `{"cache":{"size":%d,"limit":100}}`, size.Load())
	}))
	defer carbon.Close()
	host, port, _ := strings.Cut(strings.TrimPrefix(carbon.URL, "http://"), ":")
	node := host + ":4242"
	g := newCacheGuard(guardFlags{enabled: true, threshold: 0.75, protocol: "http", port: port}, []string{node}, 2)
	g.poll()

	acquired := make(chan func())
	go func() { acquired <- g.acquire(node, "unknown:4242") }()
	select {
	case <-acquired:
		t.Fatal("acquired a paused node")
	case <-time.After(50 * time.Millisecond):
	}
	size.Store(10)
	g.poll()
	select {
	case release := <-acquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("still blocked after the cache drained")
	}

	if newCacheGuard(guardFlags{enabled: true, threshold: 0}, []string{node}, 2) != nil {
		t.Fatal("threshold 0 must disable the guard")
	}
	var disabled *cacheGuard
	disabled.acquire(node)() // nil guard never blocks
}
