package paprika

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// newUID mints a fresh Paprika-style UID (uppercase canonical UUID).
func newUID() string {
	return strings.ToUpper(uuid.NewString())
}

// MealPlan is one entry on the Paprika meal-plan calendar (one date,
// one meal slot, one recipe-or-freeform-name). UID is server-generated
// the first time SaveMealPlan is called with it empty.
type MealPlan struct {
	UID       string `json:"uid"`
	RecipeUID string `json:"recipe_uid"`
	Date      string `json:"date"` // YYYY-MM-DD HH:MM:SS, time portion is usually 00:00:00
	Type      int    `json:"type"` // resolves against MealType.UID-or-builtin
	Name      string `json:"name"` // freeform name (used when RecipeUID is empty)
	OrderFlag int    `json:"order_flag"`
	TypeUID   string `json:"type_uid,omitempty"`
	Deleted   bool   `json:"deleted"`
}

type MealPlanResponse struct {
	Result []MealPlan `json:"result"`
}

// MealType is a user-defined meal slot (Breakfast/Lunch/Dinner/Snack/...).
// Paprika lets the user customize these, so the integer Type on a
// MealPlan is only meaningful via this lookup.
type MealType struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	OrderFlag int    `json:"order_flag"`
	Deleted   bool   `json:"deleted"`
}

type MealTypeResponse struct {
	Result []MealType `json:"result"`
}

// GroceryItem is one row on a grocery list. RecipeUID links the row to
// the meal-plan recipe that produced it (when added by the app's
// "ingredients to grocery list" flow); empty for ad-hoc additions.
type GroceryItem struct {
	UID         string `json:"uid"`
	RecipeUID   string `json:"recipe_uid"`
	Name        string `json:"name"`
	OrderFlag   int    `json:"order_flag"`
	Purchased   bool   `json:"purchased"`
	Aisle       string `json:"aisle"`
	Ingredient  string `json:"ingredient"`
	Recipe      string `json:"recipe"`
	Instruction string `json:"instruction"`
	Quantity    string `json:"quantity"`
	AisleUID    string `json:"aisle_uid"`
	ListUID     string `json:"list_uid"`
	Deleted     bool   `json:"deleted"`
}

type GroceryResponse struct {
	Result []GroceryItem `json:"result"`
}

// GroceryList is a named shopping list (the user can have several).
type GroceryList struct {
	UID           string `json:"uid"`
	Name          string `json:"name"`
	OrderFlag     int    `json:"order_flag"`
	IsDefault     bool   `json:"is_default"`
	RemindersList string `json:"reminders_list"`
	Deleted       bool   `json:"deleted"`
}

type GroceryListResponse struct {
	Result []GroceryList `json:"result"`
}

// GroceryAisle is a global aisle category (Produce, Dairy, ...). The
// list is shared across grocery lists. Items reference aisles via
// AisleUID (the free-text Aisle on a GroceryItem is mostly a display
// hint; AisleUID is what determines categorization in the app).
type GroceryAisle struct {
	UID       string `json:"uid"`
	Name      string `json:"name"`
	OrderFlag int    `json:"order_flag"`
	Deleted   bool   `json:"deleted"`
}

type GroceryAisleResponse struct {
	Result []GroceryAisle `json:"result"`
}

// ListMealPlan returns every meal-plan entry. Paprika has no
// date-range filter, so we pull everything and let the caller filter.
func (c *Client) ListMealPlan(ctx context.Context) (*MealPlanResponse, error) {
	var out MealPlanResponse
	if err := c.getJSON(ctx, "https://paprikaapp.com/api/v3/sync/meals", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListMealTypes returns all user-defined meal slots.
func (c *Client) ListMealTypes(ctx context.Context) (*MealTypeResponse, error) {
	var out MealTypeResponse
	if err := c.getJSON(ctx, "https://paprikaapp.com/api/v3/sync/mealtypes", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SaveMealPlan creates or updates a meal-plan entry. Paprika uses the
// bulk POST /sync/meals/ endpoint (array body) — passing a single-item
// array works for both create and update. The server fills in UID on
// create; if the caller provided one, it's preserved.
func (c *Client) SaveMealPlan(ctx context.Context, m MealPlan) (*MealPlan, error) {
	if m.UID == "" {
		m.UID = newUID()
	}
	if err := c.postBulk(ctx, "https://www.paprikaapp.com/api/v2/sync/meals/", []MealPlan{m}); err != nil {
		return nil, err
	}
	defer c.notify(ctx)
	return &m, nil
}

// DeleteMealPlan soft-deletes an entry by UID (sets Deleted=true and
// re-saves it). Paprika treats soft-deletes as the canonical removal.
func (c *Client) DeleteMealPlan(ctx context.Context, uid string) error {
	m := MealPlan{UID: uid, Deleted: true}
	return c.postBulk(ctx, "https://www.paprikaapp.com/api/v2/sync/meals/", []MealPlan{m})
}

// ListGroceries returns every grocery row across every list.
func (c *Client) ListGroceries(ctx context.Context) (*GroceryResponse, error) {
	var out GroceryResponse
	if err := c.getJSON(ctx, "https://paprikaapp.com/api/v3/sync/groceries", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListGroceryLists returns every named grocery list.
func (c *Client) ListGroceryLists(ctx context.Context) (*GroceryListResponse, error) {
	var out GroceryListResponse
	if err := c.getJSON(ctx, "https://paprikaapp.com/api/v3/sync/grocerylists", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListGroceryAisles returns the user's configured aisle categories.
// Used to resolve human-friendly aisle names (e.g. "Dairy") to the
// aisle_uid the app needs for categorization to stick.
func (c *Client) ListGroceryAisles(ctx context.Context) (*GroceryAisleResponse, error) {
	var out GroceryAisleResponse
	if err := c.getJSON(ctx, "https://paprikaapp.com/api/v3/sync/groceryaisles", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SaveGroceryItem creates or updates a single grocery row.
func (c *Client) SaveGroceryItem(ctx context.Context, item GroceryItem) (*GroceryItem, error) {
	if item.UID == "" {
		item.UID = newUID()
	}
	if err := c.postBulk(ctx, "https://www.paprikaapp.com/api/v2/sync/groceries/", []GroceryItem{item}); err != nil {
		return nil, err
	}
	defer c.notify(ctx)
	return &item, nil
}

// DeleteGroceryItem soft-deletes a grocery row by UID.
func (c *Client) DeleteGroceryItem(ctx context.Context, uid string) error {
	item := GroceryItem{UID: uid, Deleted: true}
	return c.postBulk(ctx, "https://www.paprikaapp.com/api/v2/sync/groceries/", []GroceryItem{item})
}

// getJSON is a thin wrapper that GETs `url`, JSON-decodes into v, and
// surfaces non-2xx as errors. Used for read-only endpoints.
func (c *Client) getJSON(ctx context.Context, url string, v interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if err := isErrorResponse(rawBytes); err != nil {
		return err
	}
	return json.Unmarshal(rawBytes, v)
}

// postBulk gzips a JSON array body, wraps it in multipart/form-data
// (field name "data"), and POSTs to one of Paprika's bulk endpoints
// (/sync/meals/, /sync/groceries/). The recipe save path uses the
// per-item endpoint instead — see SaveRecipe in client.go.
//
// Note: meals/groceries use `www.paprikaapp.com` (with the www); the
// recipe endpoints use `paprikaapp.com` (without). That asymmetry is
// reverse-engineered from upstream PR #11; both hosts work today but
// the upstream maintainer reported 401s on the bulk path without the
// www subdomain.
func (c *Client) postBulk(ctx context.Context, url string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(data); err != nil {
		gw.Close()
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("data", "data")
	if err != nil {
		return err
	}
	if _, err := part.Write(gzBuf.Bytes()); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = int64(body.Len())

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rawBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: %s: %s", url, resp.Status, string(rawBytes))
	}
	return isErrorResponse(rawBytes)
}
