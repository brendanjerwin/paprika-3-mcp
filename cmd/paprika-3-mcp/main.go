package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/brendanjerwin/paprika-3-mcp/internal/mcpserver"
	"github.com/brendanjerwin/paprika-3-mcp/internal/paprika"
	"github.com/brendanjerwin/paprika-3-mcp/internal/store"
	"github.com/brendanjerwin/paprika-3-mcp/internal/syncer"
)

var version = "dev" // set during build with -ldflags

// defaultDataDir returns ~/.local/state/paprika-3-mcp (XDG_STATE_HOME-aware).
// Bleve owns this directory.
func defaultDataDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "paprika-3-mcp")
	}
	if h := os.Getenv("HOME"); h != "" {
		return filepath.Join(h, ".local", "state", "paprika-3-mcp")
	}
	// Last-resort fallback. Avoids /var/log behavior of upstream that
	// failed silently for non-root users.
	return filepath.Join(os.TempDir(), "paprika-3-mcp")
}

// userNamespace returns a stable 16-hex-char fingerprint of the
// Paprika username. We hash so the directory name is filesystem-safe
// and so passwords (which we never read here) can't leak through path
// inspection. Two processes with different credentials get different
// directories; same credentials → same directory and they coordinate
// through flock.
func userNamespace(username string) string {
	sum := sha256.Sum256([]byte(username))
	return hex.EncodeToString(sum[:8])
}

func main() {
	dataDir := flag.String("data-dir", defaultDataDir(), "Per-host root for paprika-3-mcp state. The actual index lives at <data-dir>/<userhash>/recipes.bleve.")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	readerReopenInterval := flag.Duration("reader-reopen", 60*time.Second, "How often a read-only Store reloads to pick up the writer's commits.")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("paprika-3-mcp version %s\n", version)
		os.Exit(0)
	}

	username := os.Getenv("PAPRIKA_USERNAME")
	password := os.Getenv("PAPRIKA_PASSWORD")
	if username == "" || password == "" {
		fmt.Fprintln(os.Stderr, "PAPRIKA_USERNAME and PAPRIKA_PASSWORD must be set in the environment.")
		fmt.Fprintln(os.Stderr, "Older versions accepted --username/--password on the command line; that mode was removed because")
		fmt.Fprintln(os.Stderr, "the password leaked into `ps` output of every user on the host.")
		os.Exit(1)
	}

	level := slog.LevelInfo
	switch *logLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	// Logs go to stderr — stdio is reserved for MCP JSON-RPC.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	// Per-credential namespace prevents collisions between different
	// Paprika accounts on the same host.
	userDir := filepath.Join(*dataDir, userNamespace(username))
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		logger.Error("create user state dir failed", "dir", userDir, "err", err)
		os.Exit(1)
	}
	indexPath := filepath.Join(userDir, "recipes.bleve")
	lockPath := filepath.Join(userDir, "writer.lock")

	logger.Info("paprika-3-mcp starting",
		"version", version,
		"index", indexPath,
		"pid", os.Getpid(),
	)

	// Try to be the writer. Non-blocking: if another instance already
	// holds the lock we open read-only and run as a passive consumer.
	writerLock, lockErr := store.TryLock(lockPath)
	role := "writer"
	if errors.Is(lockErr, store.ErrLockHeld) {
		role = "reader"
		logger.Info("another writer is active; running read-only", "lock", lockPath)
	} else if lockErr != nil {
		logger.Error("acquire writer lock failed", "lock", lockPath, "err", lockErr)
		os.Exit(1)
	}

	// NewClient no longer logs in synchronously — the actual auth
	// round-trip happens on first authenticated request, so the MCP
	// server can answer the `initialize` handshake before that lands.
	client, err := paprika.NewClient(username, password, version, logger)
	if err != nil {
		logger.Error("paprika client init failed", "err", err)
		os.Exit(1)
	}

	st, err := openStoreWithRetry(indexPath, role == "reader", logger)
	if err != nil {
		logger.Error("open index failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	rootCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	syncerOpts := syncer.Options{
		Client: client,
		Store:  st,
		Logger: logger,
	}
	var sync *syncer.Syncer
	if role == "writer" {
		sync = syncer.New(syncerOpts)
		go sync.Run(rootCtx)
		// Held until process exit; OS releases on death.
		defer writerLock.Close()
	} else {
		// Reader: poll the lock so we can take over if the original
		// writer dies, otherwise just refresh the in-memory index.
		go runReaderLoop(rootCtx, readerLoopOpts{
			Store:        st,
			LockPath:     lockPath,
			Interval:     *readerReopenInterval,
			Logger:       logger,
			SyncerOpts:   syncerOpts,
		})
	}

	srv, err := mcpserver.NewServer(mcpserver.NewServerOptions{
		Version: version,
		Client:  client,
		Store:   st,
		Syncer:  sync,
		Logger:  logger,
	})
	if err != nil {
		logger.Error("server init failed", "err", err)
		os.Exit(1)
	}
	logger.Info("ready", "role", role)

	if err := srv.Serve(rootCtx); err != nil {
		logger.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// openStoreWithRetry handles two flavors of read-only race:
//   1. The writer hasn't created the index yet — quick retries pick it
//      up as soon as the writable Open finishes the initial materialize.
//   2. The writer is busy holding bbolt's exclusive lock and the read-
//      only open would block forever. store.Open returns
//      ErrOpenTimedOut for this case (per-attempt 2s here so we get a
//      handful of retries inside the MCP handshake window).
//
// Writers don't retry — Open creates the directory and never has to
// wait on another holder of the file.
func openStoreWithRetry(path string, readOnly bool, logger *slog.Logger) (*store.Store, error) {
	if !readOnly {
		return store.Open(store.Options{Path: path, ReadOnly: false, Logger: logger})
	}

	// 4 attempts × ~2s open + 250ms sleep ≈ 9s wall-clock max.
	const maxAttempts = 4
	const interval = 250 * time.Millisecond
	const perAttemptDeadline = 2 * time.Second
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		st, err := store.Open(store.Options{
			Path:        path,
			ReadOnly:    true,
			OpenTimeout: perAttemptDeadline,
			Logger:      logger,
		})
		if err == nil {
			return st, nil
		}
		lastErr = err
		logger.Debug("reader open failed; retrying", "err", err, "attempt", i+1)
		time.Sleep(interval)
	}
	return nil, fmt.Errorf("read-only open after %d attempts: %w", maxAttempts, lastErr)
}

type readerLoopOpts struct {
	Store      *store.Store
	LockPath   string
	Interval   time.Duration
	Logger     *slog.Logger
	SyncerOpts syncer.Options
}

// runReaderLoop is the reader's background goroutine. On each tick it:
//  1. Tries to acquire the writer flock — if the previous writer died
//     (or was never around), this reader promotes itself to writer,
//     reopens the index writable, kicks off the syncer, and exits the
//     loop holding the lock for the rest of the process lifetime.
//  2. Otherwise, calls store.Reload() so the in-memory index picks up
//     commits the current writer has flushed to disk.
//
// Promotion is a permanent state change (sticky); a process never
// demotes back to reader. The flock fd lives in the goroutine so the
// kernel releases it on process death.
func runReaderLoop(ctx context.Context, opts readerLoopOpts) {
	t := time.NewTicker(opts.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			lock, err := store.TryLock(opts.LockPath)
			switch {
			case err == nil:
				opts.Logger.Info("writer lock acquired by reader; promoting", "lock", opts.LockPath)
				if perr := opts.Store.Promote(); perr != nil {
					opts.Logger.Error("promote store failed; staying reader", "err", perr)
					_ = lock.Close()
					goto reload
				}
				// Lock is intentionally not closed here — held for
				// the rest of the process lifetime via the closure.
				_ = lock // keep reference alive
				opts.Logger.Info("promoted reader to writer; starting syncer")
				go syncer.New(opts.SyncerOpts).Run(ctx)
				return
			case errors.Is(err, store.ErrLockHeld):
				// Expected: a different process is the writer.
			default:
				opts.Logger.Warn("writer-lock probe failed; will retry", "err", err)
			}
		reload:
			if err := opts.Store.Reload(); err != nil {
				opts.Logger.Warn("reader reload failed", "err", err)
			}
		}
	}
}
