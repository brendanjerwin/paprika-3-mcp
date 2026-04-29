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
	query, _ := req.Params.Arguments["query"].(string)
	if query == "" {
		return nil, errors.New("query is required")
	}

	opts := store.SearchOptions{Query: query}
	if v, ok := req.Params.Arguments["limit"].(float64); ok {
		opts.Limit = int(v)
	}
	if v, ok := req.Params.Arguments["min_rating"].(float64); ok {
		opts.MinRating = int(v)
	}
	if v, ok := req.Params.Arguments["category"].(string); ok {
		opts.Category = v
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
	uid, _ := req.Params.Arguments["uid"].(string)
	if uid == "" {
		return nil, errors.New("uid is required")
	}
	recipe, err := s.store.Get(uid)
	if err != nil {
		return nil, err
	}
	if recipe == nil {
		return nil, fmt.Errorf("recipe %q not found", uid)
	}
	return mcp.NewToolResultResource(recipe.Name, mcp.TextResourceContents{
		URI:      fmt.Sprintf("paprika://recipes/%s", recipe.UID),
		MIMEType: "text/markdown",
		Text:     recipe.ToMarkdown(),
	}), nil
}

func (s *Server) handleCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	r, err := recipeFromArgs(req.Params.Arguments, false)
	if err != nil {
		return nil, err
	}
	saved, err := s.paprika3.SaveRecipe(ctx, *r)
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

func (s *Server) handleUpdate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	r, err := recipeFromArgs(req.Params.Arguments, true)
	if err != nil {
		return nil, err
	}
	saved, err := s.paprika3.SaveRecipe(ctx, *r)
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

func recipeFromArgs(args map[string]interface{}, requireUID bool) (*paprika.Recipe, error) {
	getStr := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}
	r := &paprika.Recipe{
		UID:         getStr("uid"),
		Name:        getStr("name"),
		Ingredients: getStr("ingredients"),
		Directions:  getStr("directions"),
		Description: getStr("description"),
		Notes:       getStr("notes"),
		Servings:    getStr("servings"),
		PrepTime:    getStr("prep_time"),
		CookTime:    getStr("cook_time"),
		Difficulty:  getStr("difficulty"),
	}
	if requireUID && r.UID == "" {
		return nil, errors.New("uid is required")
	}
	if r.Name == "" {
		return nil, errors.New("name is required")
	}
	if r.Ingredients == "" {
		return nil, errors.New("ingredients are required")
	}
	if r.Directions == "" {
		return nil, errors.New("directions are required")
	}
	return r, nil
}
