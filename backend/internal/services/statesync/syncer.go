// Package statesync keeps the persistent state-analysis store reconciled with
// the configured state backends. Instead of the dashboard re-reading every
// state file on demand, a background loop diffs each source's listing against
// the stored version markers and only reads + re-analyzes states that changed
// (new key, changed size/last-modified, or no marker to compare). Removed
// states are pruned. This makes dashboard aggregation O(changed states) per
// cycle instead of O(all states) per page view, which is what lets a source
// with thousands of state files (or a rate-limited backend like HCP Terraform)
// stay fully counted.
//
// Like the scheduler, this is a leaf service: it depends on repositories and
// the statesource connectors, never on the HTTP layer.
package statesync

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/terraform-state-manager/terraform-state-manager/internal/analyzer"
	"github.com/terraform-state-manager/terraform-state-manager/internal/db/repositories"
	"github.com/terraform-state-manager/terraform-state-manager/internal/statesource"
)

const (
	defaultInterval = 5 * time.Minute
	// readConcurrency is per source and kept modest: HCP rate-limits around
	// 30 req/30s per token and every state read there is two round-trips.
	readConcurrency = 6
	// perSourceTimeout bounds one source's full cycle (listing + changed
	// reads). Generous on purpose — a first backfill of a large org is
	// allowed to take minutes; nothing waits on it.
	perSourceTimeout = 10 * time.Minute
)

// Connect builds a live connector for a source. Implemented in the api layer
// (which owns credential decryption); kept as an injected func so this package
// does not import crypto or the api package.
type Connect func(s *repositories.Source) (statesource.Connector, error)

// ErrSyncInProgress is returned by SyncAll/SyncSources when a cycle is already
// running; callers (e.g. ?refresh=true) should serve the current store instead
// of waiting.
var ErrSyncInProgress = fmt.Errorf("a sync cycle is already running")

// Syncer reconciles the analysis store on an interval.
type Syncer struct {
	sources    *repositories.SourceRepository
	store      *repositories.StateAnalysisRepository
	connect    Connect
	interval   time.Duration
	retryDelay time.Duration
	stopCh     chan struct{}
	logger     *slog.Logger
	runMu      sync.Mutex
}

// New constructs a Syncer. Call Start to begin the background loop.
func New(sources *repositories.SourceRepository, store *repositories.StateAnalysisRepository, connect Connect) *Syncer {
	return &Syncer{
		sources:    sources,
		store:      store,
		connect:    connect,
		interval:   defaultInterval,
		retryDelay: time.Second,
		stopCh:     make(chan struct{}),
		logger:     slog.With("component", "statesync"),
	}
}

// Start launches the reconcile loop in a goroutine and returns immediately.
// The first cycle runs right away so a fresh boot backfills the store.
func (s *Syncer) Start() {
	ticker := time.NewTicker(s.interval)
	s.logger.Info("statesync started", "interval", s.interval.String())
	go func() {
		s.syncAllLogged()
		for {
			select {
			case <-ticker.C:
				s.syncAllLogged()
			case <-s.stopCh:
				ticker.Stop()
				s.logger.Info("statesync stopped")
				return
			}
		}
	}()
}

// Stop ends the loop. Safe to call once.
func (s *Syncer) Stop() { close(s.stopCh) }

func (s *Syncer) syncAllLogged() {
	if err := s.SyncAll(context.Background()); err != nil && err != ErrSyncInProgress {
		s.logger.Error("sync cycle failed", "error", err)
	}
}

// SyncAll reconciles every source. Only one cycle runs at a time; a second
// caller gets ErrSyncInProgress immediately.
func (s *Syncer) SyncAll(ctx context.Context) error {
	if !s.runMu.TryLock() {
		return ErrSyncInProgress
	}
	defer s.runMu.Unlock()

	sources, err := s.sources.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sources: %w", err)
	}
	for i := range sources {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.syncSource(ctx, &sources[i])
	}
	// Bound the history table: one cheap DELETE per cycle.
	if err := s.store.PruneHistory(ctx); err != nil {
		s.logger.Error("failed to prune analysis history", "error", err)
	}
	return nil
}

// SyncSources reconciles only the named sources, so a filtered view (e.g. the
// Reports page scoped to one source) can refresh just its own data instead of
// reconciling the whole fleet — which matters for cost and backend rate limits
// (HCP allows ~30 req/30s per token). Shares runMu with SyncAll, so it returns
// ErrSyncInProgress when a background or full cycle is already running; an empty
// id list is a no-op. Unknown ids are silently skipped. History pruning is left
// to SyncAll's full cycle.
func (s *Syncer) SyncSources(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if !s.runMu.TryLock() {
		return ErrSyncInProgress
	}
	defer s.runMu.Unlock()

	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}

	sources, err := s.sources.List(ctx)
	if err != nil {
		return fmt.Errorf("failed to list sources: %w", err)
	}
	for i := range sources {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, ok := want[sources[i].ID]; !ok {
			continue
		}
		s.syncSource(ctx, &sources[i])
	}
	return nil
}

// syncSource reconciles one source and records its sync status. Failures are
// recorded, never returned: one bad source must not stop the others.
func (s *Syncer) syncSource(ctx context.Context, src *repositories.Source) {
	ctx, cancel := context.WithTimeout(ctx, perSourceTimeout)
	defer cancel()

	status := &repositories.SourceSyncStatus{SourceID: src.ID}
	defer func() {
		if err := s.store.UpsertSyncStatus(context.WithoutCancel(ctx), status); err != nil {
			s.logger.Error("failed to record sync status", "source", src.Name, "error", err)
		}
	}()

	conn, err := s.connect(src)
	if err != nil {
		status.LastError = fmt.Sprintf("connect: %v", err)
		return
	}
	refs, err := conn.List(ctx)
	if err != nil {
		status.LastError = fmt.Sprintf("list: %v", err)
		return
	}
	status.StatesListed = len(refs)

	markers, err := s.store.Markers(ctx, src.ID)
	if err != nil {
		status.LastError = fmt.Sprintf("markers: %v", err)
		return
	}

	keep := make([]string, 0, len(refs))
	var changed []statesource.StateRef
	for _, ref := range refs {
		keep = append(keep, ref.Key)
		m := analysisMarker(ref)
		// Re-read when the marker moved, or when the listing carries no
		// metadata to compare (m == ""). The marker folds in the analyzer
		// logic version, so a bump also re-reads byte-unchanged states whose
		// stored counts were computed by an older analyzer.
		if m == "" || markers[ref.Key] != m {
			changed = append(changed, ref)
		}
	}

	var mu sync.Mutex
	var failed []statesource.StateRef
	sem := make(chan struct{}, readConcurrency)
	var wg sync.WaitGroup
	for _, ref := range changed {
		if ctx.Err() != nil {
			mu.Lock()
			failed = append(failed, ref)
			mu.Unlock()
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ref statesource.StateRef) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.syncOne(ctx, conn, src.ID, ref); err != nil {
				mu.Lock()
				failed = append(failed, ref)
				mu.Unlock()
			}
		}(ref)
	}
	wg.Wait()

	// Burst backfills can trip backend rate limits (HCP allows ~30 req/30s);
	// a paced serial retry clears those transients within the same cycle.
	// Persistent failures stay counted and re-try next cycle (no marker).
	var firstErr string
	readErrs := 0
	for _, ref := range failed {
		if ctx.Err() != nil {
			readErrs++
			continue
		}
		time.Sleep(s.retryDelay)
		if err := s.syncOne(ctx, conn, src.ID, ref); err != nil {
			readErrs++
			if firstErr == "" {
				firstErr = err.Error()
			}
		}
	}
	status.ReadErrors = readErrs
	status.LastError = firstErr

	// Prune states that vanished from the backend — but only on a clean,
	// complete listing pass, so a partial cycle never drops good rows.
	if err := s.store.DeleteMissing(ctx, src.ID, keep); err != nil {
		s.logger.Error("failed to prune missing states", "source", src.Name, "error", err)
	}

	if len(changed) > 0 || readErrs > 0 {
		s.logger.Info("source synced",
			"source", src.Name, "listed", len(refs), "changed", len(changed), "errors", readErrs)
	}
}

// syncOne reads, analyzes, and upserts a single state.
func (s *Syncer) syncOne(ctx context.Context, conn statesource.Connector, sourceID string, ref statesource.StateRef) error {
	rs, err := conn.Read(ctx, ref.Key)
	if err != nil {
		return fmt.Errorf("read %s: %w", ref.Key, err)
	}
	if rs == nil {
		return fmt.Errorf("read %s: no state returned", ref.Key)
	}
	a, err := analyzer.Analyze(rs.Data)
	if err != nil {
		return fmt.Errorf("analyze %s: %w", ref.Key, err)
	}
	row := analysisRow(sourceID, ref, a, int64(len(rs.Data)))
	if err := s.store.Upsert(ctx, row); err != nil {
		return err
	}
	// Append-only history feeds per-state time series; the repository skips the
	// insert when nothing changed vs the latest snapshot. Best-effort: the live
	// store is already current.
	if _, err := s.store.AppendHistoryIfChanged(ctx, row); err != nil {
		s.logger.Error("failed to append analysis history", "key", ref.Key, "error", err)
	}
	return nil
}

// SyncKey refreshes a single state's analysis after a TSM-initiated write, so
// edits/restores/transfers reflect on the dashboard immediately instead of on
// the next cycle. Best-effort: errors are logged, the write already succeeded.
func (s *Syncer) SyncKey(src *repositories.Source, key string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	conn, err := s.connect(src)
	if err != nil {
		s.logger.Error("post-write analysis refresh failed", "source", src.Name, "key", key, "error", err)
		return
	}
	// Re-list to pick up the fresh marker; fall back to a marker-less ref so
	// the next full cycle re-checks the key.
	ref := statesource.StateRef{Key: key, Name: key}
	if refs, err := conn.List(ctx); err == nil {
		for _, r := range refs {
			if r.Key == key {
				ref = r
				break
			}
		}
	}
	if err := s.syncOne(ctx, conn, src.ID, ref); err != nil {
		s.logger.Error("post-write analysis refresh failed", "source", src.Name, "key", key, "error", err)
	}
}

// DropKey removes a state's analysis row after a TSM-initiated delete.
func (s *Syncer) DropKey(src *repositories.Source, key string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.Delete(ctx, src.ID, key); err != nil {
		s.logger.Error("failed to drop analysis row", "source", src.Name, "key", key, "error", err)
	}
}

// marker derives the change-detection token from listing metadata: size plus
// last-modified (Azure/local), last-modified alone (HCP workspace updated-at),
// or a backend version token (consul ModifyIndex, pg content hash) for
// backends without timestamps. Empty when the backend exposes nothing — those
// states are re-read every cycle.
func marker(ref statesource.StateRef) string {
	lm := ""
	if ref.LastModified != nil {
		lm = ref.LastModified.UTC().Format(time.RFC3339Nano)
	}
	if ref.Size == 0 && lm == "" && ref.Version == "" {
		return ""
	}
	if ref.Version != "" {
		return fmt.Sprintf("%d|%s|%s", ref.Size, lm, ref.Version)
	}
	return fmt.Sprintf("%d|%s", ref.Size, lm)
}

// analysisMarker folds the analyzer logic version into the change-detection
// token, so a stored analysis is treated as current only when BOTH the state
// bytes and the analyzer revision are unchanged. Bumping analyzer.AnalysisVersion
// therefore forces a one-time re-analysis of every stored state whose bytes
// never moved (e.g. long-static 0.11.x states whose counts were computed before
// legacy support existed).
//
// Marker-less backends (empty token — no listing metadata to compare) keep the
// "" sentinel so they continue to be re-read every cycle, version regardless.
func analysisMarker(ref statesource.StateRef) string {
	m := marker(ref)
	if m == "" {
		return ""
	}
	return m + "|a" + strconv.Itoa(analyzer.AnalysisVersion)
}

func analysisRow(sourceID string, ref statesource.StateRef, a *analyzer.Analysis, size int64) *repositories.StateAnalysis {
	providers := map[string]int{}
	for _, c := range a.Providers {
		providers[c.Key] = c.Count
	}
	resTypes := map[string]int{}
	for _, c := range a.ResourceTypes {
		resTypes[c.Key] = c.Count
	}
	return &repositories.StateAnalysis{
		SourceID:         sourceID,
		StateKey:         ref.Key,
		VersionMarker:    analysisMarker(ref),
		Size:             size,
		TerraformVersion: a.TerraformVersion,
		Serial:           a.Serial,
		Lineage:          a.Lineage,
		RUM:              a.RUM,
		ManagedResources: a.ManagedResources,
		DataSources:      a.DataSources,
		TotalResources:   a.TotalResources,
		Providers:        providers,
		ResourceTypes:    resTypes,
	}
}
