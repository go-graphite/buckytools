package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cache guard: bucky's -go-carbon-health-check for batch transfers. It polls
// each node's write cache through carbonserver /admin/info and limits how
// many batches may touch a node at once.
//
//   - fill >= threshold: each poll halves the node's gate, down to 1
//   - fill >= pauseFill: no new batch until the fill is under the threshold
//   - fill <  threshold: each poll raises the gate by one, up to -w
//   - three failed polls: a paused node resumes at gate 1

const pauseFill = 0.95

type guardFlags struct {
	enabled   bool
	interval  int
	protocol  string
	port      string
	threshold float64
}

func (f *guardFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&f.enabled, "go-carbon-health-check", true, "Throttle while a go-carbon write cache fills above -go-carbon-cache-threshold.")
	fs.IntVar(&f.interval, "go-carbon-health-check-interval", 10, "Go-Carbon health check interval (seconds).")
	fs.StringVar(&f.protocol, "go-carbon-protocol", "http", "Use http or https to retrieve go-carbon /admin/info.")
	fs.StringVar(&f.port, "go-carbon-port", "8080", "Carbonserver port serving /admin/info.")
	fs.Float64Var(&f.threshold, "go-carbon-cache-threshold", 0.75, "Cache fill ratio that starts throttling. 0 disables the guard.")
}

type nodeGate struct {
	infoURL string
	limit   int // batches allowed at once
	active  int
	paused  bool
	fails   int
	fill    float64
	noLimit bool // reported once: cache has no limit
}

type cacheGuard struct {
	mu        sync.Mutex
	cond      *sync.Cond
	nodes     map[string]*nodeGate // key: buckyd HOST:PORT
	max       int
	threshold float64
	client    *http.Client
}

// newCacheGuard returns nil (no guarding) when the guard is disabled.
func newCacheGuard(f guardFlags, servers []string, workers int) *cacheGuard {
	if !f.enabled || f.threshold <= 0 {
		return nil
	}
	g := &cacheGuard{nodes: map[string]*nodeGate{}, max: workers, threshold: f.threshold,
		client: &http.Client{Timeout: 5 * time.Second}}
	g.cond = sync.NewCond(&g.mu)
	for _, server := range servers {
		host, _, err := net.SplitHostPort(server)
		if err != nil {
			host = server
		}
		g.nodes[server] = &nodeGate{limit: workers,
			infoURL: fmt.Sprintf("%s://%s/admin/info?scopes=cache", f.protocol, net.JoinHostPort(host, f.port))}
	}
	return g
}

// acquire blocks until every server may take one more batch and returns the
// release function. A nil guard and unknown servers never block.
func (g *cacheGuard) acquire(servers ...string) func() {
	if g == nil {
		return func() {}
	}
	var gates []*nodeGate
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, server := range servers {
		if gate := g.nodes[server]; gate != nil {
			gates = append(gates, gate)
		}
	}
	ready := func() bool {
		for _, gate := range gates {
			if gate.paused || gate.active >= gate.limit {
				return false
			}
		}
		return true
	}
	for !ready() {
		g.cond.Wait()
	}
	for _, gate := range gates {
		gate.active++
	}
	return func() {
		g.mu.Lock()
		for _, gate := range gates {
			gate.active--
		}
		g.mu.Unlock()
		g.cond.Broadcast()
	}
}

// cacheInfo is the "cache" scope of carbonserver /admin/info. The cache may
// be limited by points, by bytes, or both.
type cacheInfo struct {
	Size           int64 `json:"size"`
	Limit          int64 `json:"limit"`
	EstimatedBytes int64 `json:"estimated_bytes"`
	ByteLimit      int64 `json:"byte_limit"`
}

// fill is the larger of points/limit and bytes/byte limit; limited is false
// when the cache has neither limit.
func (c cacheInfo) fill() (ratio float64, limited bool) {
	if c.Limit > 0 {
		ratio, limited = float64(c.Size)/float64(c.Limit), true
	}
	if c.ByteLimit > 0 {
		ratio, limited = max(ratio, float64(c.EstimatedBytes)/float64(c.ByteLimit)), true
	}
	return ratio, limited
}

// update applies one poll result to a node's gate.
func (g *cacheGuard) update(server string, info cacheInfo, err error) {
	fill, limited := info.fill()
	g.mu.Lock()
	defer g.mu.Unlock()
	defer g.cond.Broadcast()
	gate := g.nodes[server]
	was, wasPaused := gate.limit, gate.paused
	switch {
	case err != nil:
		gate.fails++
		log.Printf("cache guard: %s: %s", server, err)
		if gate.fails >= 3 && gate.paused {
			gate.paused, gate.limit = false, 1
		}
	case !limited:
		// A valid poll without a limit: nothing can fill, so a pause or
		// throttle from an earlier limited poll must not outlive it.
		gate.fails, gate.fill, gate.paused, gate.limit = 0, 0, false, g.max
		if !gate.noLimit {
			gate.noLimit = true
			log.Printf("cache guard: %s has no cache limit and cannot be guarded", server)
		}
	default:
		gate.fails = 0
		gate.fill = fill
		switch {
		case gate.fill >= pauseFill:
			gate.paused = true
		case gate.fill >= g.threshold:
			if !gate.paused {
				gate.limit = max(1, gate.limit/2)
			}
		default:
			gate.paused = false
			gate.limit = min(g.max, gate.limit+1)
		}
	}
	if gate.limit != was || gate.paused != wasPaused {
		log.Printf("cache guard: %s fill=%.2f gate=%d paused=%t", server, gate.fill, gate.limit, gate.paused)
	}
}

func (g *cacheGuard) poll() {
	var wg sync.WaitGroup
	for server, gate := range g.nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var info struct {
				Cache cacheInfo `json:"cache"`
			}
			response, err := g.client.Get(gate.infoURL)
			if err == nil {
				if response.StatusCode != 200 {
					err = fmt.Errorf("%s: %s", gate.infoURL, response.Status)
				} else {
					err = json.NewDecoder(response.Body).Decode(&info)
				}
				response.Body.Close()
			}
			g.update(server, info.Cache, err)
		}()
	}
	wg.Wait()
}

// run polls once before returning, then every interval until stop closes.
func (g *cacheGuard) run(interval time.Duration, stop <-chan struct{}) {
	if g == nil {
		return
	}
	g.poll()
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				g.poll()
			}
		}
	}()
}

// summary lists only nodes that are throttled or paused.
func (g *cacheGuard) summary() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var parts []string
	for server, gate := range g.nodes {
		if gate.paused {
			parts = append(parts, server+"=paused")
		} else if gate.limit < g.max {
			parts = append(parts, fmt.Sprintf("%s=%d", server, gate.limit))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, " ")
}
