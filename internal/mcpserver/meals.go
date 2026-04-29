package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/brendanjerwin/paprika-3-mcp/internal/paprika"
)

// ----- meal-plan handlers -----

type listMealsResponse struct {
	Count int                 `json:"count"`
	Meals []listMealsResponseEntry `json:"meals"`
}

type listMealsResponseEntry struct {
	UID        string `json:"uid"`
	Date       string `json:"date"`
	Type       int    `json:"type"`
	Name       string `json:"name,omitempty"`
	RecipeUID  string `json:"recipe_uid,omitempty"`
	RecipeName string `json:"recipe_name,omitempty"`
}

func (s *Server) handleListMeals(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	from := req.GetString("from_date", "")
	to := req.GetString("to_date", "")
	includeDeleted := req.GetBool("include_deleted", false)

	resp, err := s.paprika3.ListMealPlan(ctx)
	if err != nil {
		return nil, err
	}

	out := listMealsResponse{Meals: []listMealsResponseEntry{}}
	for _, m := range resp.Result {
		if !includeDeleted && m.Deleted {
			continue
		}
		// `Date` is stored as "YYYY-MM-DD HH:MM:SS"; substring-compare is
		// fine for date-only filtering.
		datePart := m.Date
		if idx := strings.Index(datePart, " "); idx > 0 {
			datePart = datePart[:idx]
		}
		if from != "" && datePart < from {
			continue
		}
		if to != "" && datePart > to {
			continue
		}
		entry := listMealsResponseEntry{
			UID:       m.UID,
			Date:      datePart,
			Type:      m.Type,
			Name:      m.Name,
			RecipeUID: m.RecipeUID,
		}
		// Best-effort recipe-name resolution from the local index.
		if m.RecipeUID != "" {
			if r, err := s.store.Get(m.RecipeUID); err == nil && r != nil {
				entry.RecipeName = r.Name
			}
		}
		out.Meals = append(out.Meals, entry)
	}
	out.Count = len(out.Meals)

	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(body)), nil
}

func (s *Server) handleAddMeal(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	dateStr, err := req.RequireString("date")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if _, err := validateDate(dateStr); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	recipeUID := strings.TrimSpace(req.GetString("recipe_uid", ""))
	name := strings.TrimSpace(req.GetString("name", ""))
	if recipeUID == "" && name == "" {
		return mcp.NewToolResultError("either recipe_uid or name is required"), nil
	}

	typeUID := strings.TrimSpace(req.GetString("type_uid", ""))
	mealTypeName := strings.TrimSpace(req.GetString("meal_type", ""))
	if typeUID != "" && mealTypeName != "" {
		return mcp.NewToolResultError("pass meal_type OR type_uid, not both"), nil
	}
	if typeUID == "" {
		if mealTypeName == "" {
			mealTypeName = "Dinner"
		}
		mt, err := s.paprika3.ListMealTypes(ctx)
		if err != nil {
			return nil, fmt.Errorf("look up meal types: %w", err)
		}
		for _, t := range mt.Result {
			if t.Deleted {
				continue
			}
			if strings.EqualFold(t.Name, mealTypeName) {
				typeUID = t.UID
				break
			}
		}
		if typeUID == "" {
			return mcp.NewToolResultError(fmt.Sprintf("meal type %q not found; call list_paprika_meal_types to see options", mealTypeName)), nil
		}
	}

	// Verify recipe_uid actually points at a real recipe. Local index
	// is the fast path; API GetRecipe is the fallback for recipes the
	// reader hasn't synced yet.
	if recipeUID != "" {
		if r, _ := s.store.Get(recipeUID); r == nil {
			fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			fetched, ferr := s.paprika3.GetRecipe(fetchCtx, recipeUID)
			cancel()
			if ferr != nil || fetched == nil || fetched.UID == "" {
				return mcp.NewToolResultError(fmt.Sprintf("recipe_uid %q not found; use search_paprika_recipes to find valid UIDs", recipeUID)), nil
			}
		}
	}

	plan := paprika.MealPlan{
		RecipeUID: recipeUID,
		Name:      name,
		Date:      dateStr + " 00:00:00",
		TypeUID:   typeUID,
	}
	saved, err := s.paprika3.SaveMealPlan(ctx, plan)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(fmt.Sprintf("Added meal %s on %s (uid=%s, type_uid=%s).", display(saved), dateStr, saved.UID, saved.TypeUID)), nil
}

func (s *Server) handleRemoveMeal(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid, err := req.RequireString("uid")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return mcp.NewToolResultError("uid is required"), nil
	}

	// Confirm the meal exists and isn't already deleted before sending
	// the soft-delete. Paprika silently accepts deletes for unknown UIDs.
	plan, err := s.paprika3.ListMealPlan(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify meal exists: %w", err)
	}
	var found bool
	var alreadyDeleted bool
	for _, m := range plan.Result {
		if m.UID == uid {
			found = true
			alreadyDeleted = m.Deleted
			break
		}
	}
	if !found {
		return mcp.NewToolResultError(fmt.Sprintf("meal %q not found", uid)), nil
	}
	if alreadyDeleted {
		return mcp.NewToolResultText(fmt.Sprintf("meal %s was already soft-deleted; no-op", uid)), nil
	}

	if err := s.paprika3.DeleteMealPlan(ctx, uid); err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(fmt.Sprintf("Soft-deleted meal %s.", uid)), nil
}

func (s *Server) handleListMealTypes(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := s.paprika3.ListMealTypes(ctx)
	if err != nil {
		return nil, err
	}
	type out struct {
		UID  string `json:"uid"`
		Name string `json:"name"`
	}
	keep := []out{}
	for _, t := range resp.Result {
		if t.Deleted {
			continue
		}
		keep = append(keep, out{UID: t.UID, Name: t.Name})
	}
	body, err := json.MarshalIndent(keep, "", "  ")
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(body)), nil
}

// ----- grocery handlers -----

func (s *Server) handleListGroceries(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	listUID := req.GetString("list_uid", "")
	onlyUnpurchased := req.GetBool("only_unpurchased", false)
	includeDeleted := req.GetBool("include_deleted", false)

	resp, err := s.paprika3.ListGroceries(ctx)
	if err != nil {
		return nil, err
	}
	type row struct {
		UID       string `json:"uid"`
		Name      string `json:"name"`
		Purchased bool   `json:"purchased"`
		Aisle     string `json:"aisle,omitempty"`
		ListUID   string `json:"list_uid,omitempty"`
	}
	out := []row{}
	for _, item := range resp.Result {
		if !includeDeleted && item.Deleted {
			continue
		}
		if onlyUnpurchased && item.Purchased {
			continue
		}
		if listUID != "" && item.ListUID != listUID {
			continue
		}
		out = append(out, row{
			UID:       item.UID,
			Name:      item.Name,
			Purchased: item.Purchased,
			Aisle:     item.Aisle,
			ListUID:   item.ListUID,
		})
	}
	body, err := json.MarshalIndent(map[string]interface{}{"count": len(out), "items": out}, "", "  ")
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(body)), nil
}

func (s *Server) handleAddGrocery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	ingredient, err := req.RequireString("ingredient")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	ingredient, err = requireNonBlank("ingredient", ingredient)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	listUID := strings.TrimSpace(req.GetString("list_uid", ""))

	// Always fetch the lists collection so we can either resolve the
	// default OR validate a caller-supplied list_uid. Paprika's API
	// silently accepts unknown list_uids (the row just becomes invisible
	// in the app), so we have to enforce membership here.
	lists, err := s.paprika3.ListGroceryLists(ctx)
	if err != nil {
		return nil, fmt.Errorf("look up grocery lists: %w", err)
	}
	known := map[string]bool{}
	var defaultUID, firstUID string
	for _, l := range lists.Result {
		if l.Deleted {
			continue
		}
		known[l.UID] = true
		if firstUID == "" {
			firstUID = l.UID
		}
		if l.IsDefault {
			defaultUID = l.UID
		}
	}
	if listUID == "" {
		listUID = defaultUID
		if listUID == "" {
			listUID = firstUID
		}
		if listUID == "" {
			return mcp.NewToolResultError("no grocery lists available; create one in Paprika first"), nil
		}
	} else if !known[listUID] {
		return mcp.NewToolResultError(fmt.Sprintf("unknown list_uid %q; call list_paprika_grocery_lists to see valid UIDs", listUID)), nil
	}

	// Resolve the human-friendly aisle name to its UID. Paprika's app
	// keys categorization off `aisle_uid`; the free-text `aisle` field
	// is just a display hint and gets reclassified to "Miscellaneous"
	// when the UID is empty.
	aisleName := strings.TrimSpace(req.GetString("aisle", ""))
	var aisleUID string
	if aisleName != "" {
		aisles, err := s.paprika3.ListGroceryAisles(ctx)
		if err != nil {
			return nil, fmt.Errorf("look up grocery aisles: %w", err)
		}
		for _, a := range aisles.Result {
			if a.Deleted {
				continue
			}
			if strings.EqualFold(a.Name, aisleName) {
				aisleUID = a.UID
				aisleName = a.Name // canonicalize casing
				break
			}
		}
		if aisleUID == "" {
			return mcp.NewToolResultError(fmt.Sprintf("aisle %q not found; call list_paprika_grocery_aisles to see options", aisleName)), nil
		}
	}

	// Optional recipe_uid: validate against the local index, then API.
	recipeUID := strings.TrimSpace(req.GetString("recipe_uid", ""))
	if recipeUID != "" {
		if r, _ := s.store.Get(recipeUID); r == nil {
			fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			fetched, ferr := s.paprika3.GetRecipe(fetchCtx, recipeUID)
			cancel()
			if ferr != nil || fetched == nil || fetched.UID == "" {
				return mcp.NewToolResultError(fmt.Sprintf("recipe_uid %q not found", recipeUID)), nil
			}
		}
	}

	// Mirror `ingredient` to `name` so older Paprika clients that read
	// `name` still display something sensible. The mobile app reads
	// `ingredient`; this keeps both data-model fields in sync without
	// inventing two separate inputs.
	item := paprika.GroceryItem{
		Name:       ingredient,
		Ingredient: ingredient,
		ListUID:    listUID,
		Aisle:      aisleName,
		AisleUID:   aisleUID,
		RecipeUID:  recipeUID,
		Quantity:   strings.TrimSpace(req.GetString("quantity", "")),
	}
	saved, err := s.paprika3.SaveGroceryItem(ctx, item)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(fmt.Sprintf("Added grocery item %q (uid=%s) to list %s.", saved.Ingredient, saved.UID, saved.ListUID)), nil
}

func (s *Server) handleRemoveGrocery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid, err := req.RequireString("uid")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return mcp.NewToolResultError("uid is required"), nil
	}

	groc, err := s.paprika3.ListGroceries(ctx)
	if err != nil {
		return nil, fmt.Errorf("verify grocery item exists: %w", err)
	}
	var found, alreadyDeleted bool
	for _, g := range groc.Result {
		if g.UID == uid {
			found = true
			alreadyDeleted = g.Deleted
			break
		}
	}
	if !found {
		return mcp.NewToolResultError(fmt.Sprintf("grocery item %q not found", uid)), nil
	}
	if alreadyDeleted {
		return mcp.NewToolResultText(fmt.Sprintf("grocery item %s was already soft-deleted; no-op", uid)), nil
	}

	if err := s.paprika3.DeleteGroceryItem(ctx, uid); err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(fmt.Sprintf("Soft-deleted grocery item %s.", uid)), nil
}

func (s *Server) handleListGroceryAisles(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := s.paprika3.ListGroceryAisles(ctx)
	if err != nil {
		return nil, err
	}
	type row struct {
		UID  string `json:"uid"`
		Name string `json:"name"`
	}
	out := []row{}
	for _, a := range resp.Result {
		if a.Deleted {
			continue
		}
		out = append(out, row{UID: a.UID, Name: a.Name})
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(body)), nil
}

func (s *Server) handleListGroceryLists(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	resp, err := s.paprika3.ListGroceryLists(ctx)
	if err != nil {
		return nil, err
	}
	type row struct {
		UID       string `json:"uid"`
		Name      string `json:"name"`
		IsDefault bool   `json:"is_default"`
	}
	out := []row{}
	for _, l := range resp.Result {
		if l.Deleted {
			continue
		}
		out = append(out, row{UID: l.UID, Name: l.Name, IsDefault: l.IsDefault})
	}
	if len(out) == 0 {
		return nil, errors.New("no grocery lists found")
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(string(body)), nil
}

// ----- helpers -----

// looksLikeISODate is retained as a minimal cheap check for code paths
// that don't need full calendar validation. Use validateDate (in
// validate.go) anywhere a date is going to be sent to Paprika.
func looksLikeISODate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i, r := range s {
		if i == 4 || i == 7 {
			continue
		}
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func display(m *paprika.MealPlan) string {
	if m.Name != "" {
		return strings.TrimSpace(m.Name)
	}
	if m.RecipeUID != "" {
		return "(recipe " + m.RecipeUID + ")"
	}
	return "(unnamed)"
}
