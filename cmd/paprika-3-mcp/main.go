package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/brendanjerwin/paprika-3-mcp/internal/mcpserver"
	"github.com/brendanjerwin/paprika-3-mcp/internal/paprika"
	"github.com/brendanjerwin/paprika-3-mcp/internal/store"
	"github.com/brendanjerwin/paprika-3-mcp/internal/syncer"
)

var version = "dev" // set during build with -ldflags

// defaultDataDir returns ~/.local/state/paprika-3-mcp (XDG_STATE_HOME-aware).
func defaultDataDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "paprika-3-mcp")
	}
	if h := os.Getenv("HOME"); h != "" {
		return filepath.Join(h, ".local", "state", "paprika-3-mcp")
	}
	return filepath.Join(os.TempDir(), "paprika-3-mcp")
}

// userNamespace returns a stable 16-hex-char fingerprint of the
// Paprika username. Different accounts on the same host get different
// directories. Hashing the username (not the password) keeps the path
// safe to inspect.
func userNamespace(username string) string {
	sum := sha256.Sum256([]byte(username))
	return hex.EncodeToString(sum[:8])
}

// reapStalePIDDirs removes per-PID index directories whose owning
// process is no longer running. Each paprika-3-mcp process gets its
// own private Bleve at <userDir>/<pid>/recipes.bleve; without this
// cleanup the directory accumulates dead siblings forever.
//
// The current process's own dir is never reaped (we recreate it
// immediately afterward, but skipping it explicitly keeps the
// reaper's intent obvious).
func reapStalePIDDirs(userDir string, selfPID int, logger *slog.Logger) {
	entries, err := os.ReadDir(userDir)
	if err != nil {
		// Missing dir is fine — first run on this host. Other errors
		// (permission denied, etc.) shouldn't block startup.
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a PID dir
		}
		if pid == selfPID {
			continue
		}
		if pidAlive(pid) {
			continue
		}
		stale := filepath.Join(userDir, e.Name())
		if err := os.RemoveAll(stale); err != nil {
			logger.Warn("reap stale pid dir failed", "dir", stale, "err", err)
			continue
		}
		logger.Info("reaped stale per-pid index", "dir", stale, "pid", pid)
	}
}

// pidAlive uses the kill(pid, 0) trick: signal 0 doesn't deliver any
// signal, it just performs permission checks. errors.Is(err, ESRCH)
// means the process is gone.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

func main() {
	dataDir := flag.String("data-dir", defaultDataDir(), "Per-host root for paprika-3-mcp state. The actual index lives at <data-dir>/<userhash>/<pid>/recipes.bleve.")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
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
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	pid := os.Getpid()
	userDir := filepath.Join(*dataDir, userNamespace(username))
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		logger.Error("create user state dir failed", "dir", userDir, "err", err)
		os.Exit(1)
	}

	// Best-effort reap of dead siblings before we drop our own per-PID
	// dir into the namespace. Cheap and bounded by directory size.
	reapStalePIDDirs(userDir, pid, logger)

	pidDir := filepath.Join(userDir, strconv.Itoa(pid))
	indexPath := filepath.Join(pidDir, "recipes.bleve")
	tokenPath := filepath.Join(userDir, "token") // shared across processes for this account

	logger.Info("paprika-3-mcp starting",
		"version", version,
		"index", indexPath,
		"pid", pid,
	)

	// NewClient doesn't log in synchronously — the auth round-trip
	// happens on first authenticated request, so the MCP server can
	// answer the `initialize` handshake before that lands. Token
	// cache is shared across processes for this credential.
	client, err := paprika.NewClient(paprika.ClientOptions{
		Username:       username,
		Password:       password,
		Version:        version,
		Logger:         logger,
		TokenCachePath: tokenPath,
	})
	if err != nil {
		logger.Error("paprika client init failed", "err", err)
		os.Exit(1)
	}

	st, err := store.Open(store.Options{Path: indexPath, Logger: logger})
	if err != nil {
		logger.Error("open index failed", "err", err)
		os.Exit(1)
	}
	defer func() {
		_ = st.Close()
		// Drop our private dir on clean exit. Stale dirs from killed
		// processes are reaped on the next sibling startup via
		// reapStalePIDDirs.
		_ = os.RemoveAll(pidDir)
	}()

	rootCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sync := syncer.New(syncer.Options{
		Client: client,
		Store:  st,
		Logger: logger,
	})
	go sync.Run(rootCtx)

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
	logger.Info("ready")

	if err := srv.Serve(rootCtx); err != nil {
		logger.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}
