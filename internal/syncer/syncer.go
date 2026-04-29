// Package syncer reconciles the local Bleve index with Paprika cloud sync.
// It runs once on startup and then on a timer; each pass diffs (uid, hash)
// pairs and only refetches recipes whose hash changed (or that are new).
package syncer

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brendanjerwin/paprika-3-mcp/internal/paprika"
	"github.com/brendanjerwin/paprika-3-mcp/internal/store"
)

// Status is a snapshot of the syncer's progress, exposed for diagnostics.
type Status struct {
	LastStartedAt    time.Time
	LastCompletedAt  time.Time
	LastError        string
	LastDeepSyncAt   time.Time
	LastSeenCounter  int   // recipes counter from /sync/status seen on the last successful poll
	Indexed          int64 // total recipes successfully written this lifetime
	Removed          int64
	SkippedNoChange  int64 // polls where the status counter matched and we skipped listing
}

type Syncer struct {
	client   *paprika.Client
	store    *store.Store
	logger   *slog.Logger
	interval time.Duration

	// fetchConcurrency caps simultaneous GetRecipe calls. Paprika is happy
	// with ~16, slower than the upstream's 10 but the cluster doesn't care.
	fetchConcurrency int

	// deepSyncEvery forces a full list+diff this often even if the
	// /sync/status counter looks unchanged — defends against the (untested
	// but plausible) possibility that some change types don't bump the
	// recipes counter, plus catches indexer corruption. Default 1h.
	deepSyncEvery time.Duration

	mu     sync.RWMutex
	status Status

	indexed         atomic.Int64
	removed         atomic.Int64
	skippedNoChange atomic.Int64
}

type Options struct {
	Client           *paprika.Client
	Store            *store.Store
	Logger           *slog.Logger
	Interval         time.Duration // default 5m
	FetchConcurrency int           // default 16
	DeepSyncEvery    time.Duration // default 1h
}

func New(opts Options) *Syncer {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Minute
	}
	if opts.FetchConcurrency <= 0 {
		opts.FetchConcurrency = 16
	}
	if opts.DeepSyncEvery <= 0 {
		opts.DeepSyncEvery = time.Hour
	}
	return &Syncer{
		client:           opts.Client,
		store:            opts.Store,
		logger:           opts.Logger,
		interval:         opts.Interval,
		fetchConcurrency: opts.FetchConcurrency,
		deepSyncEvery:    opts.DeepSyncEvery,
	}
}

// Run blocks until ctx is cancelled, reconciling the index on the configured
// interval. The first sync runs immediately.
func (s *Syncer) Run(ctx context.Context) {
	if err := s.runOnce(ctx); err != nil {
		s.logger.Error("initial sync failed", "err", err)
	}

	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.runOnce(ctx); err != nil {
				s.logger.Error("sync failed", "err", err)
			}
		}
	}
}

// Status returns a snapshot for /status-style diagnostics.
func (s *Syncer) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := s.status
	st.Indexed = s.indexed.Load()
	st.Removed = s.removed.Load()
	st.SkippedNoChange = s.skippedNoChange.Load()
	return st
}

func (s *Syncer) runOnce(ctx context.Context) (retErr error) {
	s.mu.Lock()
	s.status.LastStartedAt = time.Now()
	s.status.LastError = ""
	lastSeenCounter := s.status.LastSeenCounter
	lastDeepSyncAt := s.status.LastDeepSyncAt
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.status.LastCompletedAt = time.Now()
		if retErr != nil {
			s.status.LastError = retErr.Error()
		}
		s.mu.Unlock()
	}()

	// Cheap delta probe: ask Paprika for global counters. If the recipes
	// counter hasn't moved since the last successful poll AND we've done
	// a deep sync recently, skip the (more expensive) full list/diff.
	statusCtx, statusCancel := context.WithTimeout(ctx, 15*time.Second)
	status, err := s.client.GetSyncStatus(statusCtx)
	statusCancel()
	if err != nil {
		// Status is just a probe — if it fails, fall through to the
		// regular list-and-diff path so we still make progress.
		s.logger.Warn("sync status probe failed; doing full diff anyway", "err", err)
	} else if lastSeenCounter > 0 && status.Recipes == lastSeenCounter && time.Since(lastDeepSyncAt) < s.deepSyncEvery {
		s.logger.Debug("recipes counter unchanged; skipping list+diff",
			"counter", status.Recipes,
			"since_deep_sync", time.Since(lastDeepSyncAt).Round(time.Second),
		)
		s.skippedNoChange.Add(1)
		return nil
	}

	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	remote, err := s.client.ListRecipes(listCtx)
	if err != nil {
		return err
	}

	local, err := s.store.HashesByUID()
	if err != nil {
		return err
	}

	// Build the set of UIDs the server claims to have (so we know what to
	// delete locally) and the subset whose hashes changed or are missing
	// (so we know what to refetch).
	remoteUIDs := make(map[string]struct{}, len(remote.Result))
	var toFetch []string
	for _, r := range remote.Result {
		remoteUIDs[r.UID] = struct{}{}
		if local[r.UID] != r.Hash {
			toFetch = append(toFetch, r.UID)
		}
	}

	var toDelete []string
	for uid := range local {
		if _, stillThere := remoteUIDs[uid]; !stillThere {
			toDelete = append(toDelete, uid)
		}
	}

	s.logger.Info("sync diff",
		"remote", len(remote.Result),
		"local", len(local),
		"refetch", len(toFetch),
		"delete", len(toDelete),
	)

	// Delete first — cheap, and shrinks the index before we add new docs.
	for _, uid := range toDelete {
		if err := s.store.Delete(uid); err != nil {
			s.logger.Warn("delete failed", "uid", uid, "err", err)
			continue
		}
		s.removed.Add(1)
	}

	// Even if there's nothing to fetch, this counts as a successful deep
	// pass — record the marker so the next probe can skip.
	if status != nil {
		s.mu.Lock()
		s.status.LastSeenCounter = status.Recipes
		s.status.LastDeepSyncAt = time.Now()
		s.mu.Unlock()
	}

	if len(toFetch) == 0 {
		return nil
	}

	sem := make(chan struct{}, s.fetchConcurrency)
	var wg sync.WaitGroup
	for _, uid := range toFetch {
		select {
		case <-ctx.Done():
			break
		default:
		}
		wg.Add(1)
		uid := uid
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			fetchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			recipe, err := s.client.GetRecipe(fetchCtx, uid)
			if err != nil {
				s.logger.Warn("fetch failed", "uid", uid, "err", err)
				return
			}
			if err := s.store.Upsert(recipe); err != nil {
				s.logger.Warn("upsert failed", "uid", uid, "err", err)
				return
			}
			s.indexed.Add(1)
		}()
	}
	wg.Wait()
	return nil
}
