package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-graphite/buckytools/hashing"
	"github.com/go-graphite/buckytools/metrics"
)

const buckydAuthHeader = "X-Buckyd-Authorization"

// Flags shared by sub-commands, named as in cmd/bucky.
var (
	HostPort     string
	SingleHost   bool
	JSONOutput   bool
	Verbose      bool
	APITokenFile string
	HTTPTimeout  int
)

func setupCommon(c Command) {
	host := os.Getenv("BUCKYHOST")
	if host == "" {
		host = "localhost:4242"
	}
	c.Flag.StringVar(&HostPort, "h", host, "HOST:PORT of a go-carbon buckyd API.")
	c.Flag.StringVar(&HostPort, "host", host, "HOST:PORT of a go-carbon buckyd API.")
	c.Flag.BoolVar(&SingleHost, "s", false, "Operate on the given host only, do not discover the cluster.")
	c.Flag.BoolVar(&SingleHost, "single", false, "Operate on the given host only, do not discover the cluster.")
	c.Flag.BoolVar(&JSONOutput, "j", false, "Output JSON instead of text.")
	c.Flag.BoolVar(&JSONOutput, "json", false, "Output JSON instead of text.")
	c.Flag.BoolVar(&Verbose, "v", false, "Verbose log output.")
	c.Flag.IntVar(&HTTPTimeout, "t", 600, "HTTP request timeout in seconds.")
	c.Flag.StringVar(&APITokenFile, "api-token-file", "", "File with the JWT sent to buckyd in "+buckydAuthHeader+".")
}

var httpClient = sync.OnceValue(func() *http.Client {
	return &http.Client{
		Timeout: time.Duration(HTTPTimeout) * time.Second,
		// Never follow redirects: Go would forward the buckyd JWT header to
		// whatever host a redirect names.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			IdleConnTimeout:     10 * time.Minute,
			MaxIdleConnsPerHost: 64,
		},
	}
})

var apiToken = sync.OnceValue(func() string {
	if APITokenFile == "" {
		return ""
	}
	data, err := os.ReadFile(APITokenFile)
	if err != nil {
		log.Fatalf("api token file: %s", err)
	}
	return strings.TrimSpace(string(data))
})

// apiError is a non-2xx reply from buckyd.
type apiError struct {
	Status int
	Body   string
	// StoreFenced: the refusal covers the whole store (whole-store copy), so
	// no smaller batch can pass it.
	StoreFenced bool
}

func storeFenced(err error) bool {
	e, ok := err.(*apiError)
	return ok && e.StoreFenced
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// statusOf returns the HTTP status of an apiError, or 0 for transport errors.
func statusOf(err error) int {
	if e, ok := err.(*apiError); ok {
		return e.Status
	}
	return 0
}

// call sends in as JSON (when not nil) and decodes a JSON reply into out
// (when not nil). server is HOST:PORT.
func call(method, server, path string, query url.Values, in, out any) (http.Header, error) {
	u := url.URL{Scheme: "http", Host: server, Path: path, RawQuery: query.Encode()}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token := apiToken(); token != "" {
		request.Header.Set(buckydAuthHeader, token)
	}
	response, err := httpClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return response.Header, &apiError{response.StatusCode, strings.TrimSpace(string(text)),
			response.StatusCode == http.StatusLocked || response.Header.Get("X-Buckyd-Store-Fence") != ""}
	}
	if out != nil {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			return response.Header, fmt.Errorf("%s %s: decode reply: %w", server, path, err)
		}
	}
	return response.Header, nil
}

// Wire types of go-carbon's buckyd (buckyd/sync.go, chunkstore.TransferVersion).

type TransferVersion struct {
	Name                     string
	ID, Generation, Revision uint64
}

type SyncRequest struct {
	Source   string
	Names    []string
	From     int64 `json:",omitempty"`
	Until    int64 `json:",omitempty"`
	MaxBytes int64 `json:",omitempty"`
	Recover  bool  `json:",omitempty"`
}

type SyncRow struct {
	Name   string
	Status string
	Source *TransferVersion `json:",omitempty"`
}

type SyncReply struct {
	Rows  []SyncRow
	Bytes int64
}

const (
	syncEqual  = "equal"
	syncCopied = "copied"
	syncMerged = "merged"
)

// supportsSync reports whether server implements /v1/transfer/sync, and
// whether that sync accepts Recover (needed by -delete).
func supportsSync(server string) (sync, recover bool, err error) {
	var status struct{ Sync, SyncRecover bool }
	_, err = call("GET", server, "/v1/transfer/status", nil, nil, &status)
	return status.Sync, status.SyncRecover, err
}

// listNames returns the metric names on server with the given prefix that
// match regex (nil keeps all), sorted, reading the catalogue in pages. Pages
// are filtered as they arrive so a narrow regex does not hold a whole
// catalogue in memory.
func listNames(server, prefix string, regex *regexp.Regexp) ([]string, error) {
	var names []string
	after := ""
	for {
		var page []string
		query := url.Values{"limit": {"10000"}, "prefix": {prefix}, "after": {after}}
		if _, err := call("GET", server, "/v1/transfer/list", query, nil, &page); err != nil {
			return nil, fmt.Errorf("list %s: %w", server, err)
		}
		if len(page) == 0 {
			return names, nil
		}
		for _, name := range page {
			if regex == nil || regex.MatchString(name) {
				names = append(names, name)
			}
		}
		after = page[len(page)-1]
	}
}

// listAll lists every server concurrently and keeps names matching regex
// (nil keeps all). Keys are HOST:PORT.
func listAll(servers []string, prefix string, regex *regexp.Regexp) (map[string][]string, error) {
	result := make(map[string][]string, len(servers))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var first error
	for _, server := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			names, err := listNames(server, prefix, regex)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && first == nil {
				first = err
			}
			result[server] = names
			log.Printf("%s: %d metrics", server, len(names))
		}()
	}
	wg.Wait()
	return result, first
}

func statMetric(server, name string) (*metrics.MetricData, error) {
	header, err := call("HEAD", server, "/metrics/"+name, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	stat := new(metrics.MetricData)
	if err := json.Unmarshal([]byte(header.Get("X-Metric-Stat")), stat); err != nil {
		return nil, fmt.Errorf("%s: X-Metric-Stat: %w", name, err)
	}
	stat.StorageVersion = header.Get("X-Storage-Version")
	return stat, nil
}

func deleteMetric(server, name string) error {
	_, err := call("DELETE", server, "/metrics/"+name, nil, nil, nil)
	return err
}

// ClusterConfig is the ring as one buckyd reports it.
type ClusterConfig struct {
	// Self is the seed as HOST:PORT, by its ring name when it is a member.
	Self    string
	Port    string
	Servers []string
	Hash    hashing.HashRing
	Healthy bool
}

func (c *ClusterConfig) HostPorts() []string {
	out := make([]string, len(c.Servers))
	for i, server := range c.Servers {
		out[i] = net.JoinHostPort(server, c.Port)
	}
	return out
}

// requireRing fails for a daemon that reports no ring nodes. Ownership
// lookups on an empty ring panic, and "every member" would be nobody.
func (c *ClusterConfig) requireRing(seed string) error {
	if len(c.Servers) == 0 {
		return fmt.Errorf("%s reports a hash ring without nodes: set nodes in its [buckyd] config, or use -s for commands that do not need the ring", seed)
	}
	return nil
}

// Owner is the HOST:PORT the ring assigns name to. Call requireRing first.
func (c *ClusterConfig) Owner(name string) string {
	return net.JoinHostPort(c.Hash.GetNode(name).Server, c.Port)
}

func getRing(server string) (*hashing.JSONRingType, error) {
	ring := new(hashing.JSONRingType)
	_, err := call("GET", server, "/hashring", nil, nil, ring)
	return ring, err
}

// newCluster discovers the ring from one member and checks that every member
// reports the same ring.
func newCluster(seed string) (*ClusterConfig, error) {
	_, port, err := net.SplitHostPort(seed)
	if err != nil {
		return nil, fmt.Errorf("invalid host:port %q: %w", seed, err)
	}
	master, err := getRing(seed)
	if err != nil {
		return nil, fmt.Errorf("hashring of %s: %w", seed, err)
	}
	// The seed under the name the ring knows it by: -h may be an IP or alias
	// of a ring member, and ownership is compared by ring name.
	cluster := &ClusterConfig{Port: port, Healthy: true, Self: seed}
	for _, node := range master.Nodes {
		if node.Server == master.Name {
			cluster.Self = net.JoinHostPort(master.Name, port)
		}
	}
	switch master.Algo {
	case "carbon":
		cluster.Hash = hashing.NewCarbonHashRing()
	case "fnv1a":
		cluster.Hash = hashing.NewFNV1aHashRing()
	case "jump_fnv1a":
		cluster.Hash = hashing.NewJumpHashRing(master.Replicas)
	default:
		return nil, fmt.Errorf("unknown consistent hash algorithm: %s", master.Algo)
	}
	for _, node := range master.Nodes {
		cluster.Hash.AddNode(node)
		cluster.Servers = append(cluster.Servers, node.Server)
	}
	for _, server := range cluster.HostPorts() {
		member, err := getRing(server)
		if err != nil {
			log.Printf("Cluster unhealthy: %s: %s", server, err)
			cluster.Healthy = false
			continue
		}
		same := member.Algo == master.Algo && len(member.Nodes) == len(master.Nodes)
		for i := 0; same && i < len(member.Nodes); i++ {
			same = hashing.NodeCmp(master.Nodes[i], member.Nodes[i])
		}
		if !same {
			log.Printf("Cluster unhealthy: %s reports a different ring than %s", server, seed)
			cluster.Healthy = false
		}
	}
	return cluster, nil
}

// targetServers is the seed alone with -s, otherwise every ring member.
// For commands that do not need ring ownership: -s contacts no other host.
func targetServers() ([]string, error) {
	if SingleHost {
		return []string{HostPort}, nil
	}
	servers, _, err := targetRing()
	return servers, err
}

// targetRing always discovers the ring, for commands that compute owners.
func targetRing() ([]string, *ClusterConfig, error) {
	cluster, err := newCluster(HostPort)
	if err != nil {
		return nil, nil, err
	}
	if !cluster.Healthy {
		log.Printf("Warning: Cluster is not healthy!")
	}
	if err := cluster.requireRing(HostPort); err != nil {
		return nil, nil, err
	}
	if SingleHost {
		return []string{cluster.Self}, cluster, nil
	}
	return cluster.HostPorts(), cluster, nil
}

func printJSON(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(data))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func compileRegex(expr string) *regexp.Regexp {
	if expr == "" {
		return nil
	}
	regex, err := regexp.Compile(expr)
	if err != nil {
		log.Fatalf("regex: %s", err)
	}
	return regex
}
