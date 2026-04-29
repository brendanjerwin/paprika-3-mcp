package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

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

func main() {
	dataDir := flag.String("data-dir", defaultDataDir(), "Local index location (overrides $XDG_STATE_HOME/paprika-3-mcp)")
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

	// Logs go to stderr — stdio is reserved for MCP JSON-RPC. The upstream
	// wrote to /var/log/paprika-3-mcp/server.log, which silently failed
	// for non-root users on Linux.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	logger.Info("paprika-3-mcp starting", "version", version, "data_dir", *dataDir)

	// NewClient no longer logs in synchronously — the actual auth
	// round-trip happens on first authenticated request, so the MCP
	// server can answer the `initialize` handshake before that lands.
	client, err := paprika.NewClient(username, password, version, logger)
	if err != nil {
		logger.Error("paprika client init failed", "err", err)
		os.Exit(1)
	}

	indexPath := filepath.Join(*dataDir, "recipes.bleve")
	st, err := store.Open(indexPath, logger)
	if err != nil {
		logger.Error("open index failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	sync := syncer.New(syncer.Options{
		Client: client,
		Store:  st,
		Logger: logger,
	})

	rootCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

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

	if err := srv.Serve(rootCtx); err != nil {
		logger.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}
