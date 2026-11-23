package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	regexFlag        string
	fileFlag         string
	filterCache      bool
	goCarbonPrefix   string
	allowedDsts      string
	srcSeed, dstSeed string
	copySrc, copyDst string
	rebalanceSync    syncFlags
	backfillSync     syncFlags
	copySync         syncFlags
	deleteNoop       bool
)

func init() {
	c := NewCommand(serversCommand, "servers", "[options]", "List the servers in the hash ring.",
		"Print the ring one buckyd reports and whether every member agrees on it.")
	setupCommon(c)

	c = NewCommand(listCommand, "list", "[options] [prefix]", "List metrics.",
		"List metric names found in the cluster, or on one host with -s.\nAn optional prefix is matched by the server; -r filters by regular expression.")
	setupCommon(c)
	c.Flag.StringVar(&regexFlag, "r", "", "Only metrics matching this regular expression.")

	c = NewCommand(statCommand, "stat", "[options] <metric...>", "Stat metrics.",
		"Print size, modification time and storage version of each metric on every server that holds it.")
	setupCommon(c)

	c = NewCommand(duCommand, "du", "[options] [prefix]", "Sum the logical Whisper footprint of metrics.",
		"Sum the logical Whisper footprint of metrics matching the prefix and -r: the size a\nWhisper file with each metric's retention would have, not bytes stored in Pebble.\nOne stat request per metric.")
	setupCommon(c)
	c.Flag.StringVar(&regexFlag, "r", "", "Only metrics matching this regular expression.")

	c = NewCommand(locateCommand, "locate", "[options] <metric...>", "Print the server the ring assigns each metric to.",
		"Metrics are read from the arguments, from -f FILE, or from STDIN with -f -.")
	setupCommon(c)
	c.Flag.StringVar(&fileFlag, "f", "", "Read metric names, one per line, from this file (- for STDIN).")

	c = NewCommand(deleteCommand, "delete", "[options] <metric...>", "Delete metrics.",
		"Delete metrics from every server (or one with -s). Metrics are read from the\narguments, from -f FILE, from STDIN with -f -, or selected with -r.")
	setupCommon(c)
	c.Flag.StringVar(&fileFlag, "f", "", "Read metric names, one per line, from this file (- for STDIN).")
	c.Flag.StringVar(&regexFlag, "r", "", "Delete metrics matching this regular expression.")
	c.Flag.BoolVar(&deleteNoop, "no-op", false, "Print what would be deleted.")

	c = NewCommand(inconsistentCommand, "inconsistent", "[options]", "Find metrics not in correct locations.",
		"Print server: metric for each metric the hash ring assigns to another server.\nUse bucky2 rebalance to correct.")
	setupCommon(c)
	setupRingFilter(c)

	c = NewCommand(rebalanceCommand, "rebalance", "[options] [additional buckyd servers...]", "Rebalance a server or the entire cluster.",
		`Locate metrics on the wrong server and sync them to the server the ring
assigns. Only metrics whose points differ are transferred, and existing
destination points win. Additional HOST:PORT arguments name servers outside
the ring to drain. --delete removes a source copy once the destination is
proven to hold its points. A rerun skips everything already in place.`)
	setupCommon(c)
	setupRingFilter(c)
	c.Flag.StringVar(&allowedDsts, "allowed-dsts", "", "Only sync to these destinations (host:port;host:port).")
	rebalanceSync.register(c.Flag, true)

	c = NewCommand(backfillCommand, "backfill", "[options]", "Fill one cluster from another.",
		`Sync every metric found in the source cluster to the server the destination
ring assigns. Both seeds may name the same cluster. Use -from/-until to
compare, and so repair, a time window only.

    bucky2 backfill -src-cluster-seed 10.0.1.7:4242 -dst-cluster-seed 10.0.1.9:4242 -r '^prefix\.'`)
	setupCommon(c)
	c.Flag.StringVar(&srcSeed, "src-cluster-seed", "", "Source cluster seed HOST:PORT.")
	c.Flag.StringVar(&dstSeed, "dst-cluster-seed", "", "Destination cluster seed HOST:PORT.")
	c.Flag.StringVar(&regexFlag, "r", "", "Only metrics matching this regular expression.")
	backfillSync.register(c.Flag, false)

	c = NewCommand(copyCommand, "copy", "[options] [metric...]", "Copy metrics from one server to another.",
		"Sync metrics from -src to -dst regardless of the ring. Metrics are read from\nthe arguments or -f; the default is every metric on -src. Names are kept:\nrenaming is not supported by the Pebble transfer.")
	setupCommon(c)
	c.Flag.StringVar(&copySrc, "src", "", "Source HOST:PORT.")
	c.Flag.StringVar(&copyDst, "dst", "", "Destination HOST:PORT.")
	c.Flag.StringVar(&fileFlag, "f", "", "Read metric names, one per line, from this file (- for STDIN).")
	c.Flag.StringVar(&regexFlag, "r", "", "Only metrics matching this regular expression.")
	copySync.register(c.Flag, false)
}

func setupRingFilter(c Command) {
	c.Flag.StringVar(&regexFlag, "r", "", "Only metrics matching this regular expression.")
	c.Flag.BoolVar(&filterCache, "filter-cache-metrics", true, "Ignore go-carbon's own metrics, which are written where they are produced.")
	c.Flag.StringVar(&goCarbonPrefix, "go-carbon-prefix", "carbon.agents.", "go-carbon metric prefix.")
}

func fail(err error) int {
	log.Print(err)
	return 1
}

// argNames returns metric names from the arguments and -f.
func argNames(c Command) ([]string, error) {
	names := c.Flag.Args()
	if fileFlag == "" {
		return names, nil
	}
	file := os.Stdin
	if fileFlag != "-" {
		var err error
		if file, err = os.Open(fileFlag); err != nil {
			return nil, err
		}
		defer file.Close()
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		if name := strings.TrimSpace(scanner.Text()); name != "" {
			names = append(names, name)
		}
	}
	return names, scanner.Err()
}

func serversCommand(c Command) int {
	cluster, err := newCluster(HostPort)
	if err != nil {
		return fail(err)
	}
	if JSONOutput {
		printJSON(cluster.HostPorts())
	} else {
		fmt.Printf("Buckets: %d\nReplicas: %d\nHealthy: %t\n", cluster.Hash.Len(), cluster.Hash.Replicas(), cluster.Healthy)
		for _, node := range cluster.Hash.Nodes() {
			fmt.Printf("\t%s\n", node)
		}
	}
	if !cluster.Healthy {
		return 1
	}
	return 0
}

func listCommand(c Command) int {
	servers, err := targetServers()
	if err != nil {
		return fail(err)
	}
	list, err := listAll(servers, c.Flag.Arg(0), compileRegex(regexFlag))
	if err != nil {
		return fail(err)
	}
	if JSONOutput {
		printJSON(list)
		return 0
	}
	seen := map[string]bool{}
	for _, names := range list {
		for _, name := range names {
			seen[name] = true
		}
	}
	for _, name := range sortedKeys(seen) {
		fmt.Println(name)
	}
	return 0
}

// forEach runs fn for every (server, name) with bounded concurrency.
func forEach(servers, names []string, fn func(server, name string)) {
	var wg sync.WaitGroup
	slots := make(chan struct{}, 32)
	for _, server := range servers {
		for _, name := range names {
			wg.Add(1)
			slots <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				fn(server, name)
			}()
		}
	}
	wg.Wait()
}

func statCommand(c Command) int {
	servers, err := targetServers()
	if err != nil {
		return fail(err)
	}
	type row struct {
		Server, Name, StorageVersion string
		Size, ModTime                int64
	}
	var mu sync.Mutex
	var rows []row
	var failed atomic.Int64 // reads that failed for a reason other than 404
	forEach(servers, c.Flag.Args(), func(server, name string) {
		stat, err := statMetric(server, name)
		if err != nil {
			if statusOf(err) != 404 {
				failed.Add(1)
				log.Printf("%s: %s: %s", server, name, err)
			}
			return
		}
		mu.Lock()
		rows = append(rows, row{server, name, stat.StorageVersion, stat.Size, stat.ModTime})
		mu.Unlock()
	})
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Name < rows[j].Name || rows[i].Name == rows[j].Name && rows[i].Server < rows[j].Server
	})
	if JSONOutput {
		printJSON(rows)
	} else {
		for _, r := range rows {
			fmt.Printf("%s: %s\n\tLogical Whisper size: %d\n\tModTime: %s\n\tStorageVersion: %s\n", r.Server, r.Name, r.Size,
				time.Unix(r.ModTime, 0).Format(time.RFC3339), r.StorageVersion)
		}
	}
	return incomplete(failed.Load())
}

// incomplete turns failed reads into a non-zero exit: the printed result is
// missing data and must not pass for a complete answer.
func incomplete(failed int64) int {
	if failed > 0 {
		log.Printf("Result is incomplete: %d requests failed.", failed)
		return 1
	}
	return 0
}

func duCommand(c Command) int {
	servers, err := targetServers()
	if err != nil {
		return fail(err)
	}
	list, err := listAll(servers, c.Flag.Arg(0), compileRegex(regexFlag))
	if err != nil {
		return fail(err)
	}
	var total, failed atomic.Int64
	for server, names := range list {
		forEach([]string{server}, names, func(server, name string) {
			if stat, err := statMetric(server, name); err == nil {
				total.Add(stat.Size)
			} else if statusOf(err) != 404 {
				failed.Add(1)
				log.Printf("%s: %s: %s", server, name, err)
			}
		})
	}
	// go-carbon reports the size a Whisper file with the metric's retention
	// would have, not bytes stored in Pebble: sparse and full metrics with
	// the same retention count the same.
	if JSONOutput {
		printJSON(map[string]int64{"logical_whisper_bytes": total.Load()})
	} else {
		fmt.Printf("%d bytes  %.2f GiB  (logical Whisper footprint from retention, not bytes stored)\n", total.Load(), float64(total.Load())/(1<<30))
	}
	return incomplete(failed.Load())
}

func locateCommand(c Command) int {
	cluster, err := newCluster(HostPort)
	if err == nil {
		err = cluster.requireRing(HostPort)
	}
	if err != nil {
		return fail(err)
	}
	names, err := argNames(c)
	if err != nil {
		return fail(err)
	}
	located := map[string]string{}
	for _, name := range names {
		located[name] = cluster.Owner(name)
	}
	if JSONOutput {
		printJSON(located)
		return 0
	}
	for _, name := range names {
		fmt.Printf("%s: %s\n", located[name], name)
	}
	return 0
}

func deleteCommand(c Command) int {
	servers, err := targetServers()
	if err != nil {
		return fail(err)
	}
	names, err := argNames(c)
	if err != nil {
		return fail(err)
	}
	if regexFlag != "" {
		list, err := listAll(servers, "", compileRegex(regexFlag))
		if err != nil {
			return fail(err)
		}
		seen := map[string]bool{}
		for _, found := range list {
			for _, name := range found {
				seen[name] = true
			}
		}
		names = append(names, sortedKeys(seen)...)
	}
	if deleteNoop {
		for _, name := range names {
			fmt.Println(name)
		}
		return 0
	}
	var deleted, failed atomic.Int64
	forEach(servers, names, func(server, name string) {
		switch err := deleteMetric(server, name); {
		case err == nil:
			deleted.Add(1)
		case statusOf(err) != 404:
			failed.Add(1)
			log.Printf("%s: %s: %s", server, name, err)
		}
	})
	log.Printf("Deleted %d metric copies, %d errors.", deleted.Load(), failed.Load())
	if failed.Load() > 0 {
		return 1
	}
	return 0
}

// inconsistent maps each server to the metrics the ring assigns elsewhere.
func inconsistent(cluster *ClusterConfig, servers []string) (map[string][]string, error) {
	if err := cluster.requireRing(HostPort); err != nil {
		return nil, err
	}
	list, err := listAll(servers, "", compileRegex(regexFlag))
	if err != nil {
		return nil, err
	}
	result := map[string][]string{}
	for server, names := range list {
		for _, name := range names {
			// go-carbon's own metrics are written where they are produced.
			if filterCache && strings.HasPrefix(name, goCarbonPrefix) {
				continue
			}
			if cluster.Owner(name) != server {
				result[server] = append(result[server], name)
			}
		}
		log.Printf("%d inconsistent metrics found on %s", len(result[server]), server)
	}
	return result, nil
}

func inconsistentCommand(c Command) int {
	servers, cluster, err := targetRing()
	if err != nil {
		return fail(err)
	}
	result, err := inconsistent(cluster, servers)
	if err != nil {
		return fail(err)
	}
	if JSONOutput {
		printJSON(result)
		return 0
	}
	for _, server := range sortedKeys(result) {
		for _, name := range result[server] {
			fmt.Printf("%s: %s\n", server, name)
		}
	}
	return 0
}

func rebalanceCommand(c Command) int {
	servers, cluster, err := targetRing()
	if err != nil {
		return fail(err)
	}
	if !cluster.Healthy {
		return fail(fmt.Errorf("cluster is unhealthy"))
	}
	misplaced, err := inconsistent(cluster, append(servers, c.Flag.Args()...))
	if err != nil {
		return fail(err)
	}
	allowed := map[string]bool{}
	for _, dst := range strings.Split(allowedDsts, ";") {
		if dst = strings.TrimSpace(dst); dst != "" {
			allowed[dst] = true
		}
	}
	jobs := map[string]map[string][]string{}
	excluded := 0 // misplaced, but owned by a destination outside -allowed-dsts
	for src, names := range misplaced {
		for _, name := range names {
			dst := cluster.Owner(name)
			if len(allowed) > 0 && !allowed[dst] {
				excluded++
				continue
			}
			if jobs[dst] == nil {
				jobs[dst] = map[string][]string{}
			}
			jobs[dst][src] = append(jobs[dst][src], name)
		}
	}
	if excluded > 0 {
		log.Printf("%d misplaced metrics are owned by destinations outside -allowed-dsts and were not moved.", excluded)
	}
	if len(jobs) == 0 {
		if excluded > 0 {
			return 1
		}
		log.Printf("Cluster is balanced.")
		return 0
	}
	if err := rebalanceSync.run(jobs); err != nil {
		return fail(err)
	}
	return 0
}

func backfillCommand(c Command) int {
	if srcSeed == "" || dstSeed == "" {
		return fail(fmt.Errorf("please specify -src-cluster-seed and -dst-cluster-seed"))
	}
	src, err := newCluster(srcSeed)
	if err != nil {
		return fail(err)
	}
	dst, err := newCluster(dstSeed)
	if err != nil {
		return fail(err)
	}
	if !src.Healthy || !dst.Healthy {
		return fail(fmt.Errorf("cluster is unhealthy"))
	}
	for seed, cluster := range map[string]*ClusterConfig{srcSeed: src, dstSeed: dst} {
		if err := cluster.requireRing(seed); err != nil {
			return fail(err)
		}
	}
	list, err := listAll(src.HostPorts(), "", compileRegex(regexFlag))
	if err != nil {
		return fail(err)
	}
	jobs := map[string]map[string][]string{}
	for server, names := range list {
		for _, name := range names {
			owner := dst.Owner(name)
			if owner == server {
				continue
			}
			if jobs[owner] == nil {
				jobs[owner] = map[string][]string{}
			}
			jobs[owner][server] = append(jobs[owner][server], name)
		}
	}
	if err := backfillSync.run(jobs); err != nil {
		return fail(err)
	}
	return 0
}

func copyCommand(c Command) int {
	if copySrc == "" || copyDst == "" {
		return fail(fmt.Errorf("please specify -src and -dst"))
	}
	names, err := argNames(c)
	if err != nil {
		return fail(err)
	}
	// Everything on -src only when no names were asked for at all: an empty
	// -f file means nothing to copy, not everything.
	if fileFlag == "" && c.Flag.NArg() == 0 {
		list, err := listAll([]string{copySrc}, "", compileRegex(regexFlag))
		if err != nil {
			return fail(err)
		}
		names = list[copySrc]
	} else {
		// Explicit names: -r filters them too, and each must exist on -src.
		copySync.requireSource = true
		if regex := compileRegex(regexFlag); regex != nil {
			names = slices.DeleteFunc(names, func(name string) bool { return !regex.MatchString(name) })
		}
	}
	if err := copySync.run(map[string]map[string][]string{copyDst: {copySrc: names}}); err != nil {
		return fail(err)
	}
	return 0
}
