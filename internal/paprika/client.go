package paprika

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

func userAgent(version string) string {
	return fmt.Sprintf("paprika-3-mcp/%s (golang; %s)", version, runtime.Version())
}

// NewClient builds a client that does NOT log in eagerly. The bearer
// token is fetched lazily on the first authenticated request (and
// cached thereafter), so callers can construct the client without
// blocking on Paprika's slow login endpoint. Without this, an MCP
// stdio server can't respond to the initial `initialize` handshake
// before the host gives up.
func NewClient(username, password, version string, logger *slog.Logger) (*Client, error) {
	if logger == nil {
		logger = slog.Default()
	}
	t := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}
			return d.DialContext(ctx, network, addr)
		},
		// Paprika's login endpoint can take 20-30s under load (observed
		// 2026-04-29). Generous header timeout so our requests don't
		// die before the server starts replying.
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	c := &Client{
		underlying: t,
		logger:     logger,
		username:   username,
		password:   password,
		version:    version,
	}

	c.client = &http.Client{
		Transport: &authTransport{c: c, inner: t},
		Timeout:   60 * time.Second,
	}

	return c, nil
}

type Client struct {
	client     *http.Client
	underlying http.RoundTripper
	logger     *slog.Logger

	username string
	password string
	version  string

	tokenMu sync.Mutex
	token   string
}

// ensureToken returns the bearer token, performing the login round-trip
// the first time it's called (or after invalidateToken cleared the
// cache because of a 401). Subsequent calls return the cached value.
func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token != "" {
		return c.token, nil
	}
	plain := http.Client{Transport: c.underlying, Timeout: 60 * time.Second}
	tok, err := login(ctx, plain, c.username, c.password)
	if err != nil {
		return "", err
	}
	c.token = tok
	c.logger.Info("paprika login succeeded")
	return tok, nil
}

// invalidateToken clears the cached token so the next ensureToken call
// will re-login. Called from authTransport when a request comes back
// 401, on the assumption that Paprika rotated the JWT or it expired.
// We compare to the token the failed request used so concurrent calls
// don't repeatedly invalidate a token that's already been refreshed.
func (c *Client) invalidateToken(usedToken string) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.token == usedToken {
		c.token = ""
	}
}

// authTransport is the round-tripper installed on Client.client. It
// resolves the bearer token (lazily logging in on first use) and adds
// Paprika's standard headers to every outgoing request. On a 401 it
// clears the cached token and replays the request once.
type authTransport struct {
	c     *Client
	inner http.RoundTripper
}

func (a *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Buffer the body so we can replay the request on a 401 — Go's
	// HTTP client closes the body after RoundTrip and won't replay it
	// for us. Login (no body) and GETs hit the nil branch.
	var bodyBytes []byte
	if req.Body != nil {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("buffer request body for retry: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.ContentLength = int64(len(bodyBytes))
	}

	resp, tokUsed, err := a.send(req, bodyBytes)
	if err != nil {
		return resp, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}

	// 401: drain+close the failed response, drop the cached token,
	// and replay once. ensureToken on the next attempt will perform
	// a fresh login.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	a.c.logger.Info("paprika returned 401; refreshing token and retrying once", "url", req.URL.Path)
	a.c.invalidateToken(tokUsed)

	if bodyBytes != nil {
		req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		req.ContentLength = int64(len(bodyBytes))
	}
	resp, _, err = a.send(req, bodyBytes)
	return resp, err
}

// send injects the auth headers and runs the inner round-trip once.
// Returns the token it used so the 401 path can invalidate it without
// stomping on a token a concurrent caller already refreshed.
func (a *authTransport) send(req *http.Request, _ []byte) (*http.Response, string, error) {
	tok, err := a.c.ensureToken(req.Context())
	if err != nil {
		return nil, "", err
	}
	// Strip any prior Authorization (relevant on retry after 401).
	req.Header.Del("Authorization")
	req.Header.Set("Authorization", "Bearer "+tok)
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent(a.c.version))
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}
	if req.Header.Get("Connection") == "" {
		req.Header.Set("Connection", "keep-alive")
	}
	resp, err := a.inner.RoundTrip(req)
	return resp, tok, err
}

type loginResponse struct {
	Result struct {
		Token string `json:"token"`
	} `json:"result"`
}

type errorResponse struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// login authenticates with the Paprika API and returns an authentication token
// The token is used for all subsequent requests to the API. As far as I can tell, this is a JWT with no expiration.
func login(ctx context.Context, client http.Client, username, password string) (string, error) {
	// URL-encode credentials so passwords containing `&`, `=`, `+`, or
	// other reserved characters survive the form body intact (see
	// upstream PR #8).
	body := url.Values{"email": {username}, "password": {password}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://paprikaapp.com/api/v1/account/login", bytes.NewBufferString(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to login: %s", resp.Status)
	}

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var loginResp loginResponse
	if err := json.Unmarshal(rawBytes, &loginResp); err != nil {
		return "", err
	}

	if loginResp.Result.Token == "" {
		return "", fmt.Errorf("failed to get token: %s", string(rawBytes))
	}

	return loginResp.Result.Token, nil
}

type RecipeList struct {
	Result []struct {
		UID  string `json:"uid"`
		Hash string `json:"hash"`
	} `json:"result"`
}

// SyncStatus is the response from /api/v3/sync/status/. The counters
// monotonically increase on each create/update/soft-delete in the
// matching collection, so the syncer uses the `recipes` counter as a
// cheap "did anything change?" probe before fetching the full list.
//
// Paprika doesn't publish API docs; field set is reverse-engineered from
// observation. New collections show up over time, so we keep the struct
// open by also unmarshalling into a generic map for forward compatibility.
type SyncStatus struct {
	Recipes           int `json:"recipes"`
	Categories        int `json:"categories"`
	Photos            int `json:"photos"`
	Groceries         int `json:"groceries"`
	GroceryLists      int `json:"grocerylists"`
	GroceryAisles     int `json:"groceryaisles"`
	GroceryIngredients int `json:"groceryingredients"`
	Meals             int `json:"meals"`
	MealTypes         int `json:"mealtypes"`
	Bookmarks         int `json:"bookmarks"`
	Pantry            int `json:"pantry"`
	PantryLocations   int `json:"pantrylocations"`
	Menus             int `json:"menus"`
	MenuItems         int `json:"menuitems"`
}

type syncStatusResponse struct {
	Result SyncStatus `json:"result"`
}

// GetSyncStatus fetches /api/v3/sync/status/. Used as a delta probe:
// if the `Recipes` counter is unchanged since the last successful poll,
// the syncer can skip listing recipes entirely.
func (c *Client) GetSyncStatus(ctx context.Context) (*SyncStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://paprikaapp.com/api/v3/sync/status/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get sync status: %s", resp.Status)
	}
	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var sr syncStatusResponse
	if err := json.Unmarshal(rawBytes, &sr); err != nil {
		return nil, err
	}
	return &sr.Result, nil
}

// ListRecipes retrieves a list of recipes from the Paprika API - the response objects
// only contain the UID and hash of each recipe, not the full recipe object
func (c *Client) ListRecipes(ctx context.Context) (*RecipeList, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://paprikaapp.com/api/v3/sync/recipes", nil)
	if err != nil {
		c.logger.Error("failed to create request", "error", err)
		return nil, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("failed to get recipes", "error", err)
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("failed to get recipes", "status", resp.Status)
		return nil, fmt.Errorf("failed to get recipes: %s", resp.Status)
	}

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		c.logger.Error("failed to read response body", "error", err)
		return nil, err
	}
	var recipeList RecipeList
	if err := json.Unmarshal(rawBytes, &recipeList); err != nil {
		c.logger.Error("failed to unmarshal response", "error", err)
		return nil, err
	}

	c.logger.Info("found recipes", "count", len(recipeList.Result))
	return &recipeList, nil
}

type Recipe struct {
	UID             string   `json:"uid"`
	Name            string   `json:"name"`
	Ingredients     string   `json:"ingredients"`
	Directions      string   `json:"directions"`
	Description     string   `json:"description"`
	Notes           string   `json:"notes"`
	NutritionalInfo string   `json:"nutritional_info"`
	Servings        string   `json:"servings"`
	Difficulty      string   `json:"difficulty"`
	PrepTime        string   `json:"prep_time"`
	CookTime        string   `json:"cook_time"`
	TotalTime       string   `json:"total_time"`
	Source          string   `json:"source"`
	SourceURL       string   `json:"source_url"`
	ImageURL        string   `json:"image_url"`
	Photo           string   `json:"photo"`
	PhotoHash       string   `json:"photo_hash"`
	PhotoLarge      string   `json:"photo_large"`
	Scale           string   `json:"scale"`
	Hash            string   `json:"hash"`
	Categories      []string `json:"categories"`
	Rating          int      `json:"rating"`
	InTrash         bool     `json:"in_trash"`
	IsPinned        bool     `json:"is_pinned"`
	OnFavorites     bool     `json:"on_favorites"`
	OnGroceryList   bool     `json:"on_grocery_list"`
	Created         string   `json:"created"`
	PhotoURL        string   `json:"photo_url"`
}

func (r *Recipe) ResourceDescription() string {
	if len(r.Description) == 0 {
		return fmt.Sprintf("A recipe for %s", r.Name)
	}

	return fmt.Sprintf("A recipe for %s: %s", r.Name, r.Description)
}

func (r *Recipe) ToMarkdown() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("# %s\n\n", r.Name))

	if r.Description != "" {
		sb.WriteString(fmt.Sprintf("_%s_\n\n", r.Description))
	}

	if r.Servings != "" || r.PrepTime != "" || r.CookTime != "" || r.Difficulty != "" || r.Source != "" || r.SourceURL != "" {
		sb.WriteString("## Details\n")
		if r.Servings != "" {
			sb.WriteString(fmt.Sprintf("- **Servings:** %s\n", r.Servings))
		}
		if r.PrepTime != "" {
			sb.WriteString(fmt.Sprintf("- **Prep Time:** %s\n", r.PrepTime))
		}
		if r.CookTime != "" {
			sb.WriteString(fmt.Sprintf("- **Cook Time:** %s\n", r.CookTime))
		}
		if r.Difficulty != "" {
			sb.WriteString(fmt.Sprintf("- **Difficulty:** %s\n", r.Difficulty))
		}
		if r.Source != "" {
			sb.WriteString(fmt.Sprintf("- **Source:** %s\n", r.Source))
		}
		if r.SourceURL != "" {
			sb.WriteString(fmt.Sprintf("- **Source URL:** %s\n", r.SourceURL))
		}
		sb.WriteString("\n")
	}

	if r.Ingredients != "" {
		sb.WriteString("## Ingredients\n")
		for _, line := range strings.Split(strings.TrimSpace(r.Ingredients), "\n") {
			if line != "" {
				sb.WriteString(fmt.Sprintf("- %s\n", line))
			}
		}
		sb.WriteString("\n")
	}

	if r.Directions != "" {
		sb.WriteString("## Directions\n")
		lines := strings.Split(strings.TrimSpace(r.Directions), "\n")
		for i, line := range lines {
			if line != "" {
				sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, line))
			}
		}
		sb.WriteString("\n")
	}

	if r.Notes != "" {
		sb.WriteString("## Notes\n")
		sb.WriteString(r.Notes + "\n\n")
	}

	return sb.String()
}

func (r *Recipe) generateUUID() {
	// Generate a new UUID for the recipe
	if r.UID == "" {
		r.UID = strings.ToUpper(uuid.New().String())
		return
	}

	r.UID = strings.ToUpper(r.UID)
}

func (r *Recipe) updateCreated() {
	layout := "2006-01-02 15:04:05"
	r.Created = time.Now().Format(layout)
}

func (r *Recipe) asMap() (map[string]interface{}, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}

	var fields map[string]interface{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}

	return fields, nil
}

func (r *Recipe) updateHash() error {
	fields, err := r.asMap()
	if err != nil {
		return err
	}

	// Remove the "hash" field
	delete(fields, "hash")

	// Sort keys manually to ensure consistent JSON output
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Build a sorted map for consistent hashing
	sorted := make(map[string]interface{}, len(fields))
	for _, k := range keys {
		sorted[k] = fields[k]
	}

	// Marshal the sorted map to JSON
	jsonBytes, err := json.Marshal(sorted)
	if err != nil {
		return err
	}

	hash := sha256.Sum256(jsonBytes)
	r.Hash = hex.EncodeToString(hash[:])
	return nil
}

func (r *Recipe) asGzip() ([]byte, error) {
	jsonBytes, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	_, err = writer.Write(jsonBytes)
	if err != nil {
		writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

type GetRecipeResponse struct {
	Result Recipe `json:"result"`
}

func (c *Client) GetRecipe(ctx context.Context, uid string) (*Recipe, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://paprikaapp.com/api/v3/sync/recipe/%s/", uid), nil)
	if err != nil {
		c.logger.Error("failed to create request", "error", err)
		return nil, err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("failed to get recipe", "error", err)
		return nil, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("failed to get recipe", "status", resp.Status)
		return nil, fmt.Errorf("failed to get recipe: %s", resp.Status)
	}

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		c.logger.Error("failed to read response body", "error", err)
		return nil, err
	}

	var recipeResp GetRecipeResponse
	if err := json.Unmarshal(rawBytes, &recipeResp); err != nil {
		c.logger.Error("failed to unmarshal response", "error", err)
		return nil, err
	}

	return &recipeResp.Result, nil
}

func (c *Client) DeleteRecipe(ctx context.Context, recipe Recipe) (*Recipe, error) {
	// Set the recipe to be in the trash
	// TODO: reverse-engineer full deletions; currently a user must go in-app to empty their trash and fully delete something
	recipe.InTrash = true
	return c.SaveRecipe(ctx, recipe)
}

// SaveRecipe saves a recipe to the Paprika API. If the recipe already exists, it will be updated.
// If the recipe does not exist, it will be created.
func (c *Client) SaveRecipe(ctx context.Context, recipe Recipe) (*Recipe, error) {
	// Paprika's mobile-app sync rejects records with `"categories":null`
	// — it surfaces as the "Value cannot be null. Parameter name:
	// collection." error in the iOS/Android app and blocks all further
	// sync until the bad record is fixed. The cloud's create endpoint
	// accepts the null and only barfs on subsequent client pulls, which
	// is why this didn't show up in earlier smoke tests. JSON-marshalling
	// a nil []string produces null; an empty slice produces [], which is
	// what the API actually wants. Default it here so every save path
	// (create, update, our merge-on-update) is safe.
	if recipe.Categories == nil {
		recipe.Categories = []string{}
	}
	// set the created timestamp
	recipe.updateCreated()
	// generate a new UUID if one doesn't exist
	recipe.generateUUID()
	// generate a hash of the recipe object
	if err := recipe.updateHash(); err != nil {
		return nil, err
	}

	// gzip the recipe
	fileData, err := recipe.asGzip()
	if err != nil {
		return nil, err
	}

	// Create a multipart form request
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("data", "data")
	if err != nil {
		c.logger.Error("failed to create form file", "error", err)
		return nil, err
	}

	// Write the gzipped JSON data to the form file
	if _, err := part.Write(fileData); err != nil {
		c.logger.Error("failed to write gzipped JSON data", "error", err)
		return nil, err
	}
	if err := writer.Close(); err != nil {
		c.logger.Error("failed to close multipart writer", "error", err)
		return nil, err
	}

	// Create the HTTP request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://paprikaapp.com/api/v3/sync/recipe/%s/", recipe.UID), &body)
	if err != nil {
		c.logger.Error("failed to create request", "error", err)
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.ContentLength = int64(body.Len())

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("failed to create recipe", "error", err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("failed to create recipe", "status", resp.Status)
		return nil, fmt.Errorf("failed to create recipe: %s", resp.Status)
	}

	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		c.logger.Error("failed to read response body", "error", err)
		return nil, err
	}

	if err := isErrorResponse(rawBytes); err != nil {
		c.logger.Error("failed to create recipe", "error", err)
		return nil, err
	}

	defer c.notify(ctx)

	return &recipe, nil
}

// notify sends a POST to /v2/sync/notify, which tells all Paprika clients to sync.
// We usually defer this call after a recipe is created/updated/deleted, since we don't care whether it suceeds or not.
func (c *Client) notify(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://paprikaapp.com/api/v3/sync/notify", nil)
	if err != nil {
		c.logger.Error("failed to create request", "error", err)
		return err
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Error("failed to notify", "error", err)
		return err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.logger.Error("failed to notify", "status", resp.Status)
		return fmt.Errorf("failed to notify: %s", resp.Status)
	}

	return nil
}

// isErrorResponse checks if the response body contains an error message
// and returns an error if it does. The Paprika API is very inconsistent with how it returns errors;
// sometimes a successful status code can be returned but an error is still returned in the body
func isErrorResponse(body []byte) error {
	var errResp errorResponse
	if err := json.Unmarshal(body, &errResp); err != nil {
		// Not even valid JSON
		return err
	}

	// Check if it's likely an error response
	if errResp.Error.Message != "" || errResp.Error.Code != 0 {
		return fmt.Errorf("error: %s (code: %d)", errResp.Error.Message, errResp.Error.Code)
	}

	return nil
}
