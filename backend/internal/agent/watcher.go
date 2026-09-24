package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ResetSmith/cronomicon/internal/runnerproto"
	"github.com/ResetSmith/cronomicon/internal/watchspec"
)

// The file-arrival watcher (ET-D, agent side).
//
// # Why polling, not inotify
//
// Decided 2026-08-11. Three reasons, in order of weight:
//
//  1. The stability window below ALREADY requires polling — "size unchanged for
//     N seconds" is a repeated stat by definition — so inotify would be a second
//     mechanism layered on one we need anyway.
//  2. inotify does not fire for changes made by another host on NFS/CIFS/SMB,
//     and a drop directory on a network share is the single most common shape of
//     this feature. A watcher that silently never fires there is worse than a
//     slower one that works everywhere.
//  3. No third-party dependency in the agent, and no per-user watch-descriptor
//     limit to exhaust on a host with many watches.
//
// The cost is latency bounded by scanInterval, and a directory scan per tick.
//
// # The stability window
//
// A file is reported only once its size has been unchanged across two
// consecutive observations at least StableSeconds apart. Without it the watcher
// fires on a half-written CSV — which is not a rare race but the NORMAL case for
// anything large arriving over a network, and it would have been this feature's
// first production incident.

const (
	// watchScanInterval is how often every watched glob is scanned.
	watchScanInterval = 10 * time.Second
	// watchReportMax bounds one report batch, so a directory that suddenly gains
	// ten thousand files produces a steady stream rather than one enormous POST.
	// The rest are picked up on the next tick; nothing is lost because a file is
	// only forgotten once the SERVER has recorded it.
	watchReportMax = 100
)

// watcher polls the specs the server sends and reports stable arrivals.
type watcher struct {
	a *Agent

	mu    sync.Mutex
	specs []runnerproto.WatchSpec
	// pending tracks files seen but not yet stable: path → what we last saw.
	pending map[string]pendingFile
	// reported remembers what has already been sent, so a file sitting in the
	// directory is not re-reported every tick. Keyed path+size+mtime, matching
	// the server's de-dupe key exactly — if the two disagreed, either the agent
	// would spam or a replaced file would never fire.
	reported map[string]time.Time
}

type pendingFile struct {
	size      int64
	mtime     time.Time
	firstSeen time.Time
	job       runnerproto.WatchSpec
}

func newWatcher(a *Agent) *watcher {
	return &watcher{a: a, pending: map[string]pendingFile{}, reported: map[string]time.Time{}}
}

// setSpecs replaces the watch set from a poll response.
//
// A spec REMOVED by the server clears its pending state: a file that was
// mid-stability-window for a watch that no longer exists must not fire later.
func (wt *watcher) setSpecs(specs []runnerproto.WatchSpec) {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	wt.specs = specs
	live := map[string]bool{}
	for _, s := range specs {
		live[watchIdentity(s)] = true
	}
	for k, p := range wt.pending {
		if !live[watchIdentity(p.job)] {
			delete(wt.pending, k)
		}
	}
}

// run is the watcher loop. Started once at agent start; it does nothing at all
// until the server sends specs, so an agent without the capability costs a
// ticker.
func (wt *watcher) run(ctx context.Context) {
	t := time.NewTicker(watchScanInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			wt.scanOnce(ctx, time.Now())
		}
	}
}

// scanOnce walks every spec and reports whatever has become stable.
func (wt *watcher) scanOnce(ctx context.Context, now time.Time) {
	wt.mu.Lock()
	specs := append([]runnerproto.WatchSpec(nil), wt.specs...)
	wt.mu.Unlock()
	if len(specs) == 0 {
		return
	}

	var ready []sightingOut
	for _, spec := range specs {
		// The allowlist is applied to what the SERVER sent, every scan. This is
		// the whole security posture of the feature: the server chooses the
		// globs, so the agent is the thing that says no.
		if !watchspec.PathAllowed(spec.Path, wt.a.cfg.WatchPaths) {
			wt.a.log.Warn("refusing a watch outside this runner's -watch-paths allowlist",
				"path", spec.Path, "job", spec.JobName)
			continue
		}
		matches, err := filepath.Glob(spec.Path)
		if err != nil {
			continue
		}
		for _, m := range matches {
			if s := wt.observe(m, spec, now); s != nil {
				ready = append(ready, *s)
				if len(ready) >= watchReportMax {
					break
				}
			}
		}
		if len(ready) >= watchReportMax {
			break
		}
	}
	if len(ready) == 0 {
		wt.forgetStale(now)
		return
	}
	// Deterministic order so a truncated batch makes progress through a large
	// directory rather than re-sending the same arbitrary subset.
	sort.Slice(ready, func(i, j int) bool { return ready[i].Path < ready[j].Path })
	wt.report(ctx, ready)
}

// observe applies the stability window to one file, returning a sighting when
// it has settled.
func (wt *watcher) observe(path string, spec runnerproto.WatchSpec, now time.Time) *sightingOut {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return nil // vanished between glob and stat, or a directory matched
	}
	key := sightingKey(path, fi.Size(), fi.ModTime())

	wt.mu.Lock()
	defer wt.mu.Unlock()
	if _, done := wt.reported[key]; done {
		return nil
	}
	stable := spec.StableSeconds
	if stable <= 0 {
		stable = watchspec.DefaultStableSeconds
	}
	prev, seen := wt.pending[path]
	if !seen || prev.size != fi.Size() || !prev.mtime.Equal(fi.ModTime()) {
		// First sight, or it changed since last tick — restart its window.
		wt.pending[path] = pendingFile{size: fi.Size(), mtime: fi.ModTime(), firstSeen: now, job: spec}
		return nil
	}
	if now.Sub(prev.firstSeen) < time.Duration(stable)*time.Second {
		return nil // still settling
	}
	wt.reported[key] = now
	delete(wt.pending, path)
	return &sightingOut{
		JobSource: spec.JobSource, JobName: spec.JobName, JobUID: spec.JobUID,
		Path: path, SizeBytes: fi.Size(), MTime: fi.ModTime().UTC().Format(time.RFC3339),
	}
}

// forgetStale bounds the reported ledger. The SERVER is authoritative on what
// has fired (its UNIQUE index), so this map is only an optimisation to avoid
// re-POSTing the same file every ten seconds — and it can be forgotten safely,
// at the cost of one redundant report the server will ignore.
func (wt *watcher) forgetStale(now time.Time) {
	wt.mu.Lock()
	defer wt.mu.Unlock()
	for k, at := range wt.reported {
		if now.Sub(at) > 24*time.Hour {
			delete(wt.reported, k)
		}
	}
	for p, f := range wt.pending {
		if now.Sub(f.firstSeen) > 24*time.Hour {
			delete(wt.pending, p)
		}
	}
}

type sightingOut struct {
	JobSource string `json:"jobSource"`
	JobName   string `json:"jobName"`
	// JobUID is echoed back exactly as received (v11), never invented. The
	// server matches it against what it actually distributed and fires from its
	// own copy, so this is a correlation handle rather than an instruction.
	JobUID    string `json:"jobUid,omitempty"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"sizeBytes"`
	MTime     string `json:"mtime"`
}

// watchIdentity keys a spec for the agent's own pending-state bookkeeping. It
// prefers the uid so that a job renamed server-side (same identity, new name)
// keeps its in-progress stability windows instead of silently restarting them,
// and so two same-named jobs in different agencies never share an entry.
func watchIdentity(s runnerproto.WatchSpec) string {
	if s.JobUID != "" {
		return s.JobUID
	}
	return s.JobSource + "/" + s.JobName
}

// report uploads the batch. A failure forgets the files from `reported` so the
// next tick retries — an arrival the server never heard about must not be lost
// because one request failed.
func (wt *watcher) report(ctx context.Context, ready []sightingOut) {
	if err := wt.a.client.ReportFileSightings(ctx, wt.a.id, ready); err != nil {
		wt.a.log.Warn("file watch: could not report arrivals; will retry", "count", len(ready), "err", err)
		wt.mu.Lock()
		for _, s := range ready {
			delete(wt.reported, sightingKeyStr(s.Path, s.SizeBytes, s.MTime))
		}
		wt.mu.Unlock()
		return
	}
	wt.a.log.Info("file watch: reported arrivals", "count", len(ready))
}

func sightingKey(path string, size int64, mtime time.Time) string {
	return sightingKeyStr(path, size, mtime.UTC().Format(time.RFC3339))
}

func sightingKeyStr(path string, size int64, mtime string) string {
	return fmt.Sprintf("%s|%d|%s", path, size, mtime)
}
