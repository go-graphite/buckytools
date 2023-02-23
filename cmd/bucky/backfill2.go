package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

type backfill2Command struct {
	srcClusterSeed, dstClusterSeed string
	metricMapFile                  string
	allowedMetricRegex             string
	*metricSyncer
}

func init() {
	usage := "[options]"
	short := "Copy/Backfill metrics from one cluster to another (can be the same cluster, for massive metric rename and backfill)."
	long := `Usage:
    bucky backfill2 -offload -src-cluster-seed 10.0.1.7:4242 -dst-cluster-seed 10.0.1.9:4242 -w 3 -ignore404 -metric-map-file metric_map_file.json
	or
    bucky backfill2 -offload -src-cluster-seed 10.0.1.7:4242 -dst-cluster-seed 10.0.1.9:4242 -w 3 -ignore404 -r '^mymetricprefix\.|^prefix2\.'
`

	var bf2Cmd backfill2Command

	c := NewCommand(bf2Cmd.do, "backfill2", usage, short, long)
	SetupCommon(c)
	SetupHostname(c)
	SetupSingle(c)

	c.Flag.StringVar(&bf2Cmd.srcClusterSeed, "src-cluster-seed", "", "Source cluster seed to copy metrics from.")
	c.Flag.StringVar(&bf2Cmd.dstClusterSeed, "dst-cluster-seed", "", "Destination cluster seed to copy metrics to.")
	c.Flag.StringVar(&bf2Cmd.metricMapFile, "metric-map-file", "", "File path to the new metric mapping.")
	c.Flag.StringVar(&bf2Cmd.allowedMetricRegex, "r", "", "Ignore metric-map-file and only copy/rebalance metrics matching regex. By default (i.e. empty), all metrics are allowed.")

	msFlags.registerFlags(c.Flag)

}

func (bf2 *backfill2Command) do(c Command) int {
	bf2.metricSyncer = newMetricSyncer(msFlags)

	bf2.flags.verbose = Verbose

	if bf2.srcClusterSeed == "" || bf2.dstClusterSeed == "" {
		log.Printf("Please specify -src-cluster-seed and -dst-cluster-seed.")
		return 1
	}

	// note: in the current bucky implementation, many functions are using
	// the global variable Cluster. so in backfill2, we are using it as a
	// source seed node.
	srcCluster, err := GetClusterConfig(bf2.srcClusterSeed)
	if err != nil {
		log.Printf("failed to init src cluster: %s", err.Error())
		return 1
	}
	dstCluster, err := newCluster(bf2.dstClusterSeed)
	if err != nil {
		log.Printf("failed to init dst cluster: %s", err.Error())
		return 1
	}

	jobs := map[string]map[string][]*syncJob{}

	if bf2.allowedMetricRegex != "" {
		hostPorts := srcCluster.HostPorts()
		if len(hostPorts) == 0 || !srcCluster.Healthy {
			log.Printf("Cluster is unhealthy or error finding cluster members.")
			return 1
		}
		metricMap, err := ListRegexMetrics(hostPorts, bf2.allowedMetricRegex, listForce)
		if err != nil {
			log.Printf("Can't get list of metrics to copy.")
			return 1
		}
		log.Printf("Number of metrics to copy: %d", len(metricMap))
		// make jobs struct in the similar way as rebalance does
		// but we have our src and dst cluster explicitly
		moves := make(map[string]int)
		servers := make([]string, 0)
		for src, metrics := range metricMap {
			servers = append(servers, src)
			for _, m := range metrics {
				job := new(syncJob)
				node := dstCluster.Hash.GetNode(m)
				dst := fmt.Sprintf("%s:%s", node.Server, dstCluster.Port)

				job.OldName = m
				job.NewName = m

				moves[src]++

				if _, ok := jobs[dst]; !ok {
					jobs[dst] = map[string][]*syncJob{}
				}
				jobs[dst][src] = append(jobs[dst][src], job)

				if msFlags.noop {
					log.Printf("[%s] %s => %s", src, m, dst)
				}
			}
		}
	} else {
		metricsMap := map[string]string{}
		metricsFile, err := os.Open(bf2.metricMapFile)
		if err != nil {
			log.Printf("failed to open metric map file: %s", err.Error())
			return 1
		}
		if err := json.NewDecoder(metricsFile).Decode(&metricsMap); err != nil {
			log.Printf("failed to unmarshal metric map file: %s", err.Error())
			return 1
		}

		log.Printf("Number of metrics to copy: %d", len(metricsMap))

		for srcm, dstm := range metricsMap {
			srcServer := fmt.Sprintf("%s:%s", srcCluster.Hash.GetNode(srcm).Server, Cluster.Port)
			dstServer := fmt.Sprintf("%s:%s", dstCluster.Hash.GetNode(dstm).Server, Cluster.Port)

			if jobs[dstServer] == nil {
				jobs[dstServer] = map[string][]*syncJob{}
			}
			jobs[dstServer][srcServer] = append(jobs[dstServer][srcServer], &syncJob{OldName: srcm, NewName: dstm})
		}
	}

	log.Println("Copying Stats:")
	for dst, srcJobs := range jobs {
		for src, jobs := range srcJobs {
			log.Printf("  %s -> %s: %d", src, dst, len(jobs))
		}
	}

	err = bf2.run(jobs)
	if err != nil {
		return 1
	}
	return 0
}
