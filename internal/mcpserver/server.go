package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/brendanjerwin/paprika-3-mcp/internal/paprika"
	"github.com/brendanjerwin/paprika-3-mcp/internal/store"
	"github.com/brendanjerwin/paprika-3-mcp/internal/syncer"
)

// NewServerOptions configures the MCP server. The Paprika client and store
// are injected so the caller controls lifecycle (close, etc.).
type NewServerOptions struct {
	Version string
	Client  *paprika.Client
	Store   *store.Store
	Syncer  *syncer.Syncer
	Logger  *slog.Logger
}

func NewServer(opts NewServerOptions) (*Server, error) {
	if opts.Client == nil || opts.Store == nil {
		return nil, errors.New("Client and Store are required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	srv := server.NewMCPServer(
		"paprika-3-mcp",
		opts.Version,
		// Both flags here are notification capabilities the upstream
		// project disabled; we leave them off too because we re-derive
		// the resource list from the index on every request anyway.
		server.WithResourceCapabilities(false, false),
	)

	return &Server{
		paprika3: opts.Client,
		store:    opts.Store,
		syncer:   opts.Syncer,
		server:   srv,
		logger:   logger,
		version:  opts.Version,
	}, nil
}

type Server struct {
	paprika3 *paprika.Client
	store    *store.Store
	syncer   *syncer.Syncer
	logger   *slog.Logger
	server   *server.MCPServer
	version  string
}

// Serve registers tools/resources and blocks on stdio until ctx is cancelled
// or stdin closes.
func (s *Server) Serve(ctx context.Context) error {
	s.registerTools()
	s.registerResources()

	// ServeStdio blocks; the upstream code didn't honor ctx, but we keep
	// a context arg for future hooks (graceful shutdown, etc.).
	_ = ctx
	return server.ServeStdio(s.server)
}

func (s *Server) registerTools() {
	searchTool := mcp.NewTool("search_paprika_recipes",
		mcp.WithDescription("Full-text search of the local Paprika recipe index. Supports Bleve query-string syntax: bare terms (`pinto bean`), fielded queries (`name:chili`), phrases (`\"smoked paprika\"`), boolean operators (`pinto AND bean -refried`), and fuzziness (`paprika~`). Returns ranked hits with highlighted snippets so the caller can see why each recipe matched."),
		mcp.WithString("query", mcp.Description("Search query in Bleve query-string syntax."), mcp.Required()),
		mcp.WithNumber("limit", mcp.Description("Max results to return (default 10).")),
		mcp.WithNumber("min_rating", mcp.Description("Only return recipes with a rating at least this high (0-5).")),
		mcp.WithString("category", mcp.Description("Filter to a single Paprika category (exact match).")),
	)
	getTool := mcp.NewTool("get_paprika_recipe",
		mcp.WithDescription("Fetch a single recipe by UID from the local index, returning the full markdown."),
		mcp.WithString("uid", mcp.Description("Recipe UID (as returned by search_paprika_recipes)."), mcp.Required()),
	)
	createTool := mcp.NewTool("create_paprika_recipe",
		mcp.WithDescription("Save a new recipe to Paprika 3 cloud sync. The recipe is also written to the local search index immediately."),
		mcp.WithString("name", mcp.Description("Recipe name."), mcp.Required()),
		mcp.WithString("ingredients", mcp.Description("Ingredients (one per line)."), mcp.Required()),
		mcp.WithString("directions", mcp.Description("Step-by-step directions."), mcp.Required()),
		mcp.WithString("description", mcp.Description("Short description."), mcp.DefaultString("")),
		mcp.WithString("notes", mcp.Description("Cook's notes."), mcp.DefaultString("")),
		mcp.WithString("servings", mcp.Description("Servings."), mcp.DefaultString("")),
		mcp.WithString("prep_time", mcp.Description("Prep time."), mcp.DefaultString("")),
		mcp.WithString("cook_time", mcp.Description("Cook time."), mcp.DefaultString("")),
		mcp.WithString("difficulty", mcp.Description("Difficulty (Easy/Medium/Hard)."), mcp.DefaultString("")),
	)
	updateTool := mcp.NewTool("update_paprika_recipe",
		mcp.WithDescription("Update an existing recipe in Paprika 3 cloud sync (and the local index)."),
		mcp.WithString("uid", mcp.Description("UID of the recipe to update."), mcp.Required()),
		mcp.WithString("name", mcp.Description("Recipe name."), mcp.Required()),
		mcp.WithString("ingredients", mcp.Description("Ingredients."), mcp.Required()),
		mcp.WithString("directions", mcp.Description("Directions."), mcp.Required()),
		mcp.WithString("description", mcp.Description("Description."), mcp.Required()),
		mcp.WithString("notes", mcp.Description("Notes."), mcp.Required()),
		mcp.WithString("servings", mcp.Description("Servings."), mcp.Required()),
		mcp.WithString("prep_time", mcp.Description("Prep time."), mcp.Required()),
		mcp.WithString("cook_time", mcp.Description("Cook time."), mcp.Required()),
		mcp.WithString("difficulty", mcp.Description("Difficulty."), mcp.Required()),
	)

	s.server.AddTools(
		server.ServerTool{Tool: searchTool, Handler: s.handleSearch},
		server.ServerTool{Tool: getTool, Handler: s.handleGet},
		server.ServerTool{Tool: createTool, Handler: s.handleCreate},
		server.ServerTool{Tool: updateTool, Handler: s.handleUpdate},
	)
}

// registerResources exposes a paprika://recipes/{uid} resource per indexed
// recipe. The upstream surfaces every recipe as a separate, statically
// registered resource — workable for small libraries but cumbersome at 600+.
// Instead we register one ResourceTemplate; clients (or this server's own
// Read handler) use search to discover UIDs.
func (s *Server) registerResources() {
	tpl := mcp.NewResourceTemplate(
		"paprika://recipes/{uid}",
		"Paprika recipe",
		mcp.WithTemplateDescription("A single Paprika recipe rendered as Markdown. Look up UIDs via search_paprika_recipes."),
		mcp.WithTemplateMIMEType("text/markdown"),
	)
	s.server.AddResourceTemplate(tpl, s.handleReadResource)
}

func (s *Server) handleReadResource(_ context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
	uid, err := uidFromURI(req.Params.URI)
	if err != nil {
		return nil, err
	}
	recipe, err := s.store.Get(uid)
	if err != nil {
		return nil, err
	}
	if recipe == nil {
		return nil, fmt.Errorf("recipe %q not found", uid)
	}
	return []mcp.ResourceContents{mcp.TextResourceContents{
		URI:      req.Params.URI,
		MIMEType: "text/markdown",
		Text:     recipe.ToMarkdown(),
	}}, nil
}

func uidFromURI(uri string) (string, error) {
	const prefix = "paprika://recipes/"
	if len(uri) <= len(prefix) || uri[:len(prefix)] != prefix {
		return "", fmt.Errorf("expected URI like paprika://recipes/{uid}, got %q", uri)
	}
	return uri[len(prefix):], nil
}

// ----- tool handlers -----

type searchResponse struct {
	Query string            `json:"query"`
	Hits  []store.SearchHit `json:"hits"`
}

func (s *Server) handleSearch(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query, err := req.RequireString("query")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	opts := store.SearchOptions{
		Query:     query,
		Limit:     req.GetInt("limit", 0),
		MinRating: req.GetInt("min_rating", 0),
		Category:  req.GetString("category", ""),
	}

	hits, err := s.store.Search(opts)
	if err != nil {
		return nil, err
	}

	body, err := json.MarshalIndent(searchResponse{Query: query, Hits: hits}, "", "  ")
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(body)), nil
}

func (s *Server) handleGet(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid, err := req.RequireString("uid")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	recipe, err := s.store.Get(uid)
	if err != nil {
		return nil, err
	}
	if recipe == nil {
		return mcp.NewToolResultError(fmt.Sprintf("recipe %q not found", uid)), nil
	}
	return mcp.NewToolResultResource(recipe.Name, mcp.TextResourceContents{
		URI:      fmt.Sprintf("paprika://recipes/%s", recipe.UID),
		MIMEType: "text/markdown",
		Text:     recipe.ToMarkdown(),
	}), nil
}

func (s *Server) handleCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	ingredients, err := req.RequireString("ingredients")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	directions, err := req.RequireString("directions")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	recipe := paprika.Recipe{
		Name:        name,
		Ingredients: ingredients,
		Directions:  directions,
		Description: req.GetString("description", ""),
		Notes:       req.GetString("notes", ""),
		Servings:    req.GetString("servings", ""),
		PrepTime:    req.GetString("prep_time", ""),
		CookTime:    req.GetString("cook_time", ""),
		Difficulty:  req.GetString("difficulty", ""),
		Source:      req.GetString("source", ""),
		SourceURL:   req.GetString("source_url", ""),
	}

	saved, err := s.paprika3.SaveRecipe(ctx, recipe)
	if err != nil {
		return nil, err
	}
	if err := s.store.Upsert(saved); err != nil {
		s.logger.Warn("local upsert after create failed", "uid", saved.UID, "err", err)
	}
	return mcp.NewToolResultResource(saved.Name, mcp.TextResourceContents{
		URI:      fmt.Sprintf("paprika://recipes/%s", saved.UID),
		MIMEType: "text/markdown",
		Text:     saved.ToMarkdown(),
	}), nil
}

// handleUpdate fetches the existing recipe (so we keep server-managed
// fields the LLM doesn't see — categories, photo, photo_hash, rating,
// etc.) and overlays only the fields the caller provided. This is the
// fix for upstream issue soggycactus/paprika-3-mcp#7, where overwriting
// the whole record nulled out `categories`/`collection` and broke
// Paprika's mobile-app sync ("Value cannot be null").
func (s *Server) handleUpdate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid, err := req.RequireString("uid")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	existing, err := s.store.Get(uid)
	if err != nil {
		return nil, fmt.Errorf("look up existing recipe: %w", err)
	}
	if existing == nil {
		// Local index miss — fall back to the API. If that fails the
		// recipe really doesn't exist (or the user hasn't synced).
		fetched, ferr := s.paprika3.GetRecipe(ctx, uid)
		if ferr != nil {
			return mcp.NewToolResultError(fmt.Sprintf("recipe %q not found locally and API lookup failed: %s", uid, ferr)), nil
		}
		existing = fetched
	}

	// Overlay any field the caller provided; otherwise keep the existing
	// value. RequireString-ish semantics: if the caller passed "" for a
	// field we treat that as "leave it alone" rather than "blank it out",
	// because the upstream tool schema marks every text field as required
	// and clients tend to fill blanks with the empty string.
	merged := *existing
	if v, ok := requireOrSkip(req, "name"); ok {
		merged.Name = v
	}
	if v, ok := requireOrSkip(req, "ingredients"); ok {
		merged.Ingredients = v
	}
	if v, ok := requireOrSkip(req, "directions"); ok {
		merged.Directions = v
	}
	if v, ok := requireOrSkip(req, "description"); ok {
		merged.Description = v
	}
	if v, ok := requireOrSkip(req, "notes"); ok {
		merged.Notes = v
	}
	if v, ok := requireOrSkip(req, "servings"); ok {
		merged.Servings = v
	}
	if v, ok := requireOrSkip(req, "prep_time"); ok {
		merged.PrepTime = v
	}
	if v, ok := requireOrSkip(req, "cook_time"); ok {
		merged.CookTime = v
	}
	if v, ok := requireOrSkip(req, "difficulty"); ok {
		merged.Difficulty = v
	}
	if v, ok := requireOrSkip(req, "source"); ok {
		merged.Source = v
	}
	if v, ok := requireOrSkip(req, "source_url"); ok {
		merged.SourceURL = v
	}

	if merged.Name == "" {
		return mcp.NewToolResultError("name must be non-empty after merge"), nil
	}

	saved, err := s.paprika3.SaveRecipe(ctx, merged)
	if err != nil {
		return nil, err
	}
	if err := s.store.Upsert(saved); err != nil {
		s.logger.Warn("local upsert after update failed", "uid", saved.UID, "err", err)
	}
	return mcp.NewToolResultResource(saved.Name, mcp.TextResourceContents{
		URI:      fmt.Sprintf("paprika://recipes/%s", saved.UID),
		MIMEType: "text/markdown",
		Text:     saved.ToMarkdown(),
	}), nil
}

// requireOrSkip returns (value, true) iff the caller actually provided a
// non-empty string for the given key. Empty / missing → (_, false), so
// the caller can leave the existing field intact during a merge update.
func requireOrSkip(req mcp.CallToolRequest, key string) (string, bool) {
	v := req.GetString(key, "")
	if v == "" {
		return "", false
	}
	return v, true
}
