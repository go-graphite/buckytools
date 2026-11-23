package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// syncFlags are shared by rebalance, backfill and copy.
type syncFlags struct {
	workers int
	batch   int
	delete  bool
	noop    bool
	// requireSource: the names were asked for explicitly, so one that does
	// not exist on the source is a failure, not an expired metric.
	requireSource bool
	settle        time.Duration
	from          int64
	until         int64
	guard         guardFlags
}

func (f *syncFlags) register(fs *flag.FlagSet, withDelete bool) {
	fs.IntVar(&f.workers, "w", 5, "Concurrent batches per destination.")
	fs.IntVar(&f.workers, "workers", 5, "Concurrent batches per destination.")
	fs.IntVar(&f.batch, "batch", 1000, "Metric names per sync request.")
	fs.BoolVar(&f.noop, "no-op", false, "Do not alter metrics and print report.")
	fs.DurationVar(&f.settle, "settle", 10*time.Minute, "Ignore points newer than this when comparing, so live tails do not count as differences.")
	fs.Int64Var(&f.from, "from", 0, "Compare only points at or after this Unix time (default: full history).")
	fs.Int64Var(&f.until, "until", 0, "Compare only points before this Unix time (default: now minus -settle).")
	if withDelete {
		fs.BoolVar(&f.delete, "delete", false, "Delete a metric from the source after it was copied whole to a destination that lacked it. Metrics already present on the destination are synced but kept, and reported.")
	}
	f.guard.register(fs)
}

// backoffBase is the first 429/5xx retry delay; tests shorten it.
var backoffBase = time.Second

const (
	maxAttempts    = 8     // per request, for 429 and transient failures
	maxSyncNames   = 10000 // go-carbon: chunkstore.MaxTransferMetrics
	maxDeleteNames = 256   // go-carbon: Store.DeleteTransfer
)

type syncer struct {
	flags *syncFlags
	guard *cacheGuard
	from  int64
	until int64

	total                                  int64
	equal, copied, merged, absent, changed atomic.Int64
	deleted, failed, bytes, done           atomic.Int64
	deleteFailed                           atomic.Int64
	// unfenced counts names in place on the destination that -delete could
	// not remove because the reply carried no deletion version: the metric
	// already existed on the destination (equal or merged), which does not
	// prove every stored source point arrived. They stay on the source, so
	// the run must not report success.
	unfenced atomic.Int64
}

// run syncs jobs (destination -> source -> names) and returns an error when
// any name failed. Hosts are HOST:PORT.
func (f *syncFlags) run(jobs map[string]map[string][]string) error {
	s := &syncer{flags: f, from: f.from, until: f.until}
	switch {
	case f.workers < 1:
		return errors.New("-w must be at least 1")
	case f.batch < 1 || f.batch > maxSyncNames:
		return fmt.Errorf("-batch must be 1..%d", maxSyncNames)
	case f.guard.enabled && f.guard.threshold > 0 && f.guard.interval < 1:
		return errors.New("-go-carbon-health-check-interval must be at least 1 second")
	}
	// The window only decides which names are pulled. It never decides what
	// may be deleted: go-carbon returns a deletion version only for a metric
	// that was absent on the destination and copied whole.
	if s.from == 0 {
		s.from = 1
	}
	if s.until == 0 {
		s.until = time.Now().Add(-f.settle).Unix()
	}
	if s.until <= s.from {
		return fmt.Errorf("empty comparison window [%d, %d)", s.from, s.until)
	}
	nodes := map[string]bool{}
	for dst, sources := range jobs {
		nodes[dst] = true
		for src, names := range sources {
			nodes[src] = true
			// go-carbon rejects a batch that names a metric twice.
			slices.Sort(names)
			names = slices.Compact(names)
			sources[src] = names
			s.total += int64(len(names))
			log.Printf("  %s -> %s: %d", src, dst, len(names))
		}
	}
	log.Printf("Number of metrics to sync: %d", s.total)
	if f.noop {
		for _, dst := range sortedKeys(jobs) {
			for _, src := range sortedKeys(jobs[dst]) {
				for _, name := range jobs[dst][src] {
					fmt.Printf("[%s] %s => %s\n", src, name, dst)
				}
			}
		}
		return nil
	}
	if s.total == 0 {
		return nil
	}
	for dst := range jobs {
		ok, canRecover, err := supportsSync(dst)
		if err != nil {
			return fmt.Errorf("%s: %w", dst, err)
		}
		if !ok {
			return fmt.Errorf("%s: go-carbon is too old, it has no /v1/transfer/sync", dst)
		}
		// Checked on every destination before anything moves: an older
		// daemon would reject each -delete batch with 400.
		if f.delete && !canRecover {
			return fmt.Errorf("%s: go-carbon is too old for -delete, its sync cannot recover interrupted copies", dst)
		}
	}

	stop := make(chan struct{})
	defer close(stop)
	s.guard = newCacheGuard(f.guard, sortedKeys(nodes), f.workers)
	s.guard.run(time.Duration(f.guard.interval)*time.Second, stop)
	go s.progress(stop)

	var wg sync.WaitGroup
	for dst, sources := range jobs {
		type batch struct {
			src   string
			names []string
		}
		queue := make(chan batch)
		for range f.workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for b := range queue {
					s.sync(b.src, dst, b.names)
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(queue)
			for _, src := range sortedKeys(sources) {
				names := sources[src]
				sort.Strings(names)
				for len(names) > 0 {
					n := min(f.batch, len(names))
					queue <- batch{src, names[:n]}
					names = names[n:]
				}
			}
		}()
	}
	wg.Wait()
	log.Print(s.report())
	if n := s.unfenced.Load(); n > 0 {
		log.Printf("%d metrics were synced but kept on the source: they already existed on the destination, and a comparison or merge cannot prove that every stored source point (for example a future-dated one) arrived. Only metrics copied whole to a destination that lacked them are deleted.", n)
	}
	// A row with a status this client does not know (changed, from an earlier
	// daemon) was not proven synced: always incomplete work.
	n := s.failed.Load() + s.deleteFailed.Load() + s.unfenced.Load() + s.changed.Load()
	if f.requireSource {
		if absent := s.absent.Load(); absent > 0 {
			log.Printf("%d explicitly requested metrics do not exist on the source.", absent)
			n += absent
		}
	}
	if n > 0 {
		return fmt.Errorf("%d metrics failed, were missing or were left on the source", n)
	}
	return nil
}

// retry runs request until it succeeds or fails for good. 429 (pressure,
// transfer slots) and transient failures back off; anything else returns.
func (s *syncer) retry(request func() error, gates ...string) error {
	delay := backoffBase
	var err error
	for range maxAttempts {
		release := s.guard.acquire(gates...)
		err = request()
		release()
		switch status := statusOf(err); {
		case err == nil:
			return nil
		case status == 429 || status == 502 || status == 503 || status == 504 || status == 0:
			time.Sleep(delay/2 + time.Duration(rand.Int63n(int64(delay))))
			delay = min(2*delay, 30*time.Second)
		default:
			return err
		}
	}
	return err
}

// sync moves one batch. A reply that rejects the batch because of its size
// or one name (413 too large, 409 conflicting, 404 vanished) halves it until
// the offending names are isolated. Store-wide refusals (403, 421 same
// daemon, 423 whole-store copy) would fail every half too: the batch fails once.
func (s *syncer) sync(src, dst string, names []string) {
	var reply SyncReply
	err := s.retry(func() error {
		reply = SyncReply{}
		// With -delete, ask the target to recover copies an interrupted
		// earlier run already committed.
		request := SyncRequest{Source: "http://" + src, Names: names, From: s.from, Until: s.until, Recover: s.flags.delete}
		_, err := call("POST", dst, "/v1/transfer/sync", nil, request, &reply)
		return err
	}, src, dst)
	if err == nil && len(reply.Rows) != len(names) {
		err = errors.New("sync reply has the wrong row count")
	}
	// Bind every row, and so every deletion proof, to the name requested at
	// that position: a reply must never cause a delete outside this batch.
	for i := 0; err == nil && i < len(names); i++ {
		if reply.Rows[i].Name != names[i] {
			err = fmt.Errorf("sync reply row %d names %q, want %q", i, reply.Rows[i].Name, names[i])
		}
	}
	if err != nil {
		if status := statusOf(err); len(names) > 1 && !storeFenced(err) && (status == 413 || status == 409 || status == 404) {
			s.sync(src, dst, names[:len(names)/2])
			s.sync(src, dst, names[len(names)/2:])
			return
		}
		s.failed.Add(int64(len(names)))
		s.done.Add(int64(len(names)))
		log.Printf("sync %s -> %s failed for %d metrics (%s ...): %s", src, dst, len(names), names[0], err)
		return
	}
	s.bytes.Add(reply.Bytes)
	var proven []TransferVersion
	for _, row := range reply.Rows {
		switch row.Status {
		case syncEqual:
			s.equal.Add(1)
		case syncCopied:
			s.copied.Add(1)
		case syncMerged:
			s.merged.Add(1)
		case "absent_on_source":
			s.absent.Add(1)
			continue
		default: // changed: the source moved during the copy; rerun picks it up
			s.changed.Add(1)
			continue
		}
		// Delete only on a copied row whose proof names this metric. A
		// daemon speaking an earlier reply format may attach a version to
		// equal or merged rows; those do not prove a complete copy.
		if row.Status == syncCopied && row.Source != nil && row.Source.Name == row.Name {
			proven = append(proven, *row.Source)
		} else if s.flags.delete {
			s.unfenced.Add(1)
		}
	}
	if s.flags.delete {
		s.delete(src, proven)
	}
	s.done.Add(int64(len(names)))
}

// delete removes proven names from the source in chunks the server accepts.
// The version fence refuses (409) a chunk holding a name written since it was
// compared; only that error is worth halving, to isolate such names. Any
// other failure (403, 5xx after retries) would fail the same way per name.
func (s *syncer) delete(src string, versions []TransferVersion) {
	for len(versions) > maxDeleteNames {
		s.delete(src, versions[:maxDeleteNames])
		versions = versions[maxDeleteNames:]
	}
	if len(versions) == 0 {
		return
	}
	err := s.retry(func() error {
		_, err := call("POST", src, "/v1/transfer/delete", nil, versions, nil)
		return err
	}, src)
	switch {
	case err == nil:
		s.deleted.Add(int64(len(versions)))
	case len(versions) > 1 && statusOf(err) == 409 && !storeFenced(err):
		s.delete(src, versions[:len(versions)/2])
		s.delete(src, versions[len(versions)/2:])
	default:
		s.deleteFailed.Add(int64(len(versions)))
		log.Printf("delete on %s failed for %d metrics (%s ...): %s", src, len(versions), versions[0].Name, err)
	}
}

func (s *syncer) report() string {
	parts := []string{fmt.Sprintf("%d/%d metrics", s.done.Load(), s.total),
		fmt.Sprintf("equal=%d copied=%d merged=%d", s.equal.Load(), s.copied.Load(), s.merged.Load())}
	for name, n := range map[string]int64{"absent_on_source": s.absent.Load(), "changed": s.changed.Load(),
		"deleted": s.deleted.Load(), "failed": s.failed.Load(), "delete_failed": s.deleteFailed.Load(),
		"kept_on_source": s.unfenced.Load()} {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", name, n))
		}
	}
	sort.Strings(parts[2:])
	parts = append(parts, fmt.Sprintf("%.1f MiB", float64(s.bytes.Load())/(1<<20)))
	if throttled := s.guard.summary(); throttled != "" {
		parts = append(parts, "throttled: "+throttled)
	}
	return strings.Join(parts, "  ")
}

func (s *syncer) progress(stop <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			log.Print(s.report())
		}
	}
}
