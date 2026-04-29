package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	date, err := req.RequireString("date")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if !looksLikeISODate(date) {
		return mcp.NewToolResultError("date must be YYYY-MM-DD"), nil
	}
	recipeUID := req.GetString("recipe_uid", "")
	name := req.GetString("name", "")
	if recipeUID == "" && name == "" {
		return mcp.NewToolResultError("either recipe_uid or name is required"), nil
	}

	typeUID := req.GetString("type_uid", "")
	mealTypeName := req.GetString("meal_type", "")
	if typeUID == "" {
		if mealTypeName == "" {
			mealTypeName = "Dinner"
		}
		// Resolve the meal-type name → UID via the API. We could cache
		// these but the list is short and lookups are cheap.
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

	// If recipe_uid is given but no name, use the recipe's name as the
	// freeform display label so older clients that don't dereference
	// recipe_uid still show something useful.
	if recipeUID != "" && name == "" {
		if r, err := s.store.Get(recipeUID); err == nil && r != nil {
			name = r.Name
		}
	}

	plan := paprika.MealPlan{
		RecipeUID: recipeUID,
		Name:      name,
		Date:      date + " 00:00:00",
		TypeUID:   typeUID,
	}
	saved, err := s.paprika3.SaveMealPlan(ctx, plan)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(fmt.Sprintf("Added meal %s on %s (uid=%s, type_uid=%s).", display(saved), date, saved.UID, saved.TypeUID)), nil
}

func (s *Server) handleRemoveMeal(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid, err := req.RequireString("uid")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
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
	name, err := req.RequireString("name")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	listUID := req.GetString("list_uid", "")
	if listUID == "" {
		// Fall back to the user's default list. If none is marked
		// default, the API will pick "the" list when there's only one.
		lists, err := s.paprika3.ListGroceryLists(ctx)
		if err != nil {
			return nil, fmt.Errorf("look up default grocery list: %w", err)
		}
		for _, l := range lists.Result {
			if l.Deleted {
				continue
			}
			if l.IsDefault {
				listUID = l.UID
				break
			}
		}
		if listUID == "" {
			// No default flagged — pick the first non-deleted list.
			for _, l := range lists.Result {
				if !l.Deleted {
					listUID = l.UID
					break
				}
			}
		}
		if listUID == "" {
			return mcp.NewToolResultError("no grocery lists available; create one in Paprika first"), nil
		}
	}

	item := paprika.GroceryItem{
		Name:      name,
		ListUID:   listUID,
		Aisle:     req.GetString("aisle", ""),
		RecipeUID: req.GetString("recipe_uid", ""),
		Quantity:  req.GetString("quantity", ""),
	}
	saved, err := s.paprika3.SaveGroceryItem(ctx, item)
	if err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(fmt.Sprintf("Added grocery item %q (uid=%s) to list %s.", saved.Name, saved.UID, saved.ListUID)), nil
}

func (s *Server) handleRemoveGrocery(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	uid, err := req.RequireString("uid")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if err := s.paprika3.DeleteGroceryItem(ctx, uid); err != nil {
		return nil, err
	}
	return mcp.NewToolResultText(fmt.Sprintf("Soft-deleted grocery item %s.", uid)), nil
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

// looksLikeISODate is a permissive sanity check: 10 chars, dashes in the
// right places, all-digit otherwise. No strict YYYY validation — we want
// to bail on obvious typos, not enforce calendar validity.
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
