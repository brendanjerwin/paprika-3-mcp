// One-shot repair tool: walks every recipe in the user's Paprika cloud,
// flags any with a nil/missing Categories field, and re-saves them.
// SaveRecipe in the v0.7 client defaults Categories to []string{} so
// the re-save fixes the "Value cannot be null. Parameter name:
// collection." error that breaks Paprika's mobile-app sync.
//
// Usage:
//   PAPRIKA_USERNAME=... PAPRIKA_PASSWORD=... \
//     go run ./cmd/repair-categories [name-substring]
//
// If `name-substring` is supplied, only recipes whose name contains
// that string (case-insensitive) are touched. Otherwise every nil-
// categories recipe is repaired.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/brendanjerwin/paprika-3-mcp/internal/paprika"
)

func main() {
	username := os.Getenv("PAPRIKA_USERNAME")
	password := os.Getenv("PAPRIKA_PASSWORD")
	if username == "" || password == "" {
		fmt.Fprintln(os.Stderr, "PAPRIKA_USERNAME and PAPRIKA_PASSWORD must be set")
		os.Exit(1)
	}
	filter := ""
	if len(os.Args) > 1 {
		filter = strings.ToLower(os.Args[1])
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	client, err := paprika.NewClient(username, password, "repair", logger)
	if err != nil {
		fmt.Fprintln(os.Stderr, "client init:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	list, err := client.ListRecipes(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list:", err)
		os.Exit(1)
	}
	fmt.Printf("scanning %d recipes (filter=%q)...\n", len(list.Result), filter)

	type bad struct {
		uid  string
		name string
	}
	var (
		mu     sync.Mutex
		bads   []bad
		nFetch int
	)
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, r := range list.Result {
		wg.Add(1)
		uid := r.UID
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rec, err := client.GetRecipe(ctx, uid)
			if err != nil {
				return
			}
			mu.Lock()
			nFetch++
			mu.Unlock()
			if rec.InTrash {
				return
			}
			if filter != "" &&
				!strings.Contains(strings.ToLower(rec.Name), filter) &&
				!strings.EqualFold(rec.UID, filter) {
				return
			}
			if filter != "" {
				fmt.Printf("  matched %q (uid=%s) categories=%v\n", rec.Name, rec.UID, rec.Categories)
			}
			if rec.Categories == nil {
				mu.Lock()
				bads = append(bads, bad{uid: uid, name: rec.Name})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	fmt.Printf("fetched %d recipes; %d need repair\n", nFetch, len(bads))

	for _, b := range bads {
		rec, err := client.GetRecipe(ctx, b.uid)
		if err != nil {
			fmt.Printf("  ! GET %s failed: %v\n", b.name, err)
			continue
		}
		// Defensive copy; SaveRecipe will mutate the value (it sets
		// Created/Hash). The underlying fix is in SaveRecipe itself —
		// it defaults Categories to []string{} when nil.
		_, err = client.SaveRecipe(ctx, *rec)
		if err != nil {
			fmt.Printf("  ! SAVE %s failed: %v\n", b.name, err)
			continue
		}
		fmt.Printf("  ✓ repaired %s (uid=%s)\n", b.name, b.uid)
	}
}
