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

	var sync *syncer.Syncer
	if role == "writer" {
		sync = syncer.New(syncer.Options{
			Client: client,
			Store:  st,
			Logger: logger,
		})
		go sync.Run(rootCtx)
		// Held until process exit; OS releases on death.
		defer writerLock.Close()
	} else {
		go runReaderReloader(rootCtx, st, *readerReopenInterval, logger)
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

// openStoreWithRetry handles the cold-start race: if a reader spawns
// before the writer has materialized the index, the read-only Open
// fails with "index does not exist". Retry briefly so the reader
// catches up rather than crashing — but stay well within an MCP
// handshake window (50 × 100ms = 5s budget).
func openStoreWithRetry(path string, readOnly bool, logger *slog.Logger) (*store.Store, error) {
	const maxAttempts = 50
	const interval = 100 * time.Millisecond
	var lastErr error
	for i := 0; i < maxAttempts; i++ {
		st, err := store.Open(store.Options{Path: path, ReadOnly: readOnly, Logger: logger})
		if err == nil {
			return st, nil
		}
		lastErr = err
		if !readOnly {
			// Writers don't retry — Open creates the directory if missing.
			return nil, err
		}
		logger.Debug("reader waiting for writer to create index", "err", err, "attempt", i+1)
		time.Sleep(interval)
	}
	return nil, fmt.Errorf("waiting for writer-created index: %w", lastErr)
}

// runReaderReloader periodically calls store.Reload() so the reader
// process picks up commits the writer has flushed to disk. Bleve has
// no inotify-style mechanism for cross-process change notifications,
// so polling is the simplest correct option.
func runReaderReloader(ctx context.Context, st *store.Store, every time.Duration, logger *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := st.Reload(); err != nil {
				logger.Warn("reader reload failed", "err", err)
			}
		}
	}
}
