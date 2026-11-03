package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"syscall"
	"testing"

	"github.com/go-graphite/buckytools/hashing"
)

func TestIsHealthyMissingPeer(t *testing.T) {
	nodes := []hashing.Node{
		{Server: "seed", Port: 2003, Instance: "a"},
		{Server: "peer", Port: 2003, Instance: "b"},
	}
	seed := &hashing.JSONRingType{Name: "seed", Nodes: nodes, Algo: "fnv1a", Replicas: 100}
	peer := &hashing.JSONRingType{Name: "peer", Nodes: nodes, Algo: "fnv1a", Replicas: 100}
	tests := []struct {
		name string
		ring []*hashing.JSONRingType
		want bool
	}{
		{name: "healthy", ring: []*hashing.JSONRingType{peer}, want: true},
		{name: "healthy seed in results", ring: []*hashing.JSONRingType{seed, peer}, want: true},
		{name: "missing peer", ring: []*hashing.JSONRingType{nil}},
		{name: "missing peer before seed", ring: []*hashing.JSONRingType{nil, seed}},
		{name: "missing peer after seed", ring: []*hashing.JSONRingType{seed, nil}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isHealthy(seed, tt.ring); got != tt.want {
				t.Errorf("isHealthy() = %t, want %t", got, tt.want)
			}
		})
	}
}

type hashRingRoundTripper func(*http.Request) (*http.Response, error)

func (f hashRingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestServersCommandPeerHealth(t *testing.T) {
	tests := []struct {
		name        string
		unreachable bool
		want        int
	}{
		{name: "healthy"},
		{name: "connection refused", unreachable: true, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldCluster, oldHostPort, oldClient, oldTokenFile := Cluster, HostPort, httpClient, APITokenFile
			t.Cleanup(func() {
				Cluster, HostPort, httpClient, APITokenFile = oldCluster, oldHostPort, oldClient, oldTokenFile
			})
			Cluster, HostPort, APITokenFile = nil, "seed:4242", ""
			httpClient = &http.Client{Transport: hashRingRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Hostname() == "peer" && tt.unreachable {
					return nil, syscall.ECONNREFUSED
				}
				ring := hashing.JSONRingType{
					Name: r.URL.Hostname(), Algo: "fnv1a", Replicas: 100,
					Nodes: []hashing.Node{
						{Server: "seed", Port: 2003, Instance: "a"},
						{Server: "peer", Port: 2003, Instance: "b"},
					},
				}
				body, err := json.Marshal(ring)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}

			if got := serversCommand(Command{}); got != tt.want {
				t.Errorf("serversCommand() = %d, want %d", got, tt.want)
			}
			if Cluster == nil || Cluster.Healthy != !tt.unreachable {
				t.Errorf("cluster = %+v, want healthy = %t", Cluster, !tt.unreachable)
			}
		})
	}
}
