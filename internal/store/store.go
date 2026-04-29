// Package store wraps a Bleve index that holds the local copy of every
// non-trashed Paprika recipe. The index is the only source of truth for
// search, get, and (uid -> hash) reconciliation; there's no separate SQL
// store or sidecar file.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/keyword"
	"github.com/blevesearch/bleve/v2/analysis/lang/en"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/search"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/brendanjerwin/paprika-3-mcp/internal/paprika"
)

const docType = "recipe"

// ErrReadOnly is returned by Upsert/Delete when the store was opened
// in read-only mode (because another process holds the writer lock).
var ErrReadOnly = errors.New("store is read-only; another process holds the writer lock")

type Store struct {
	path     string
	logger   *slog.Logger
	readOnly bool

	// upsertMu serializes index writes so a sync pass and a tool-driven
	// create/update cannot interleave on the same UID.
	upsertMu sync.Mutex

	// idxMu protects idx — readers may swap it during Reload() to pick
	// up commits from the writer process, and any in-flight Search/Get
	// must hold a read lock for the duration of their query.
	idxMu sync.RWMutex
	idx   bleve.Index
}

// Options configures Open.
type Options struct {
	Path        string        // index directory (e.g. ~/.local/state/paprika-3-mcp/<userhash>/recipes.bleve)
	ReadOnly    bool          // true → open Bleve read-only, refuse writes, allow Reload
	OpenTimeout time.Duration // applies to read-only opens only; 0 → 5s default
	Logger      *slog.Logger
}

// Open opens or creates the index. When ReadOnly is true, Upsert and
// Delete return ErrReadOnly and Reload() may be used to refresh the
// in-memory view from disk after another process has committed.
//
// Read-only opens are guarded by OpenTimeout to avoid hanging when an
// active writer holds bbolt's exclusive lock — the underlying scorch
// open hardcodes Timeout=0 (wait forever). On timeout Open returns
// ErrOpenTimedOut so the caller can fall back gracefully.
func Open(opts Options) (*Store, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(opts.Path), 0o700); err != nil {
		return nil, fmt.Errorf("create index parent dir: %w", err)
	}

	var (
		idx bleve.Index
		err error
	)
	if opts.ReadOnly {
		deadline := opts.OpenTimeout
		if deadline <= 0 {
			deadline = 5 * time.Second
		}
		idx, err = openIndexTimed(opts.Path, true, deadline)
	} else {
		idx, err = openIndex(opts.Path, false)
	}
	if err != nil {
		return nil, err
	}
	return &Store{
		path:     opts.Path,
		logger:   logger,
		readOnly: opts.ReadOnly,
		idx:      idx,
	}, nil
}

// ErrOpenTimedOut means the underlying Bleve / bbolt call did not
// return within the configured deadline. In practice this happens when
// a writer process holds the scorch root.bolt's exclusive fcntl lock
// and bbolt's hardcoded Timeout: 0 makes our read-only opener wait
// forever. Surfacing the timeout as an error lets callers fail-fast
// instead of hanging the MCP handshake.
var ErrOpenTimedOut = errors.New("bleve open timed out (writer likely holds an exclusive lock)")

// openIndex synchronously opens (or, for writers, creates) the Bleve
// index. Used by writers — readers should go through openIndexTimed.
func openIndex(path string, readOnly bool) (bleve.Index, error) {
	if readOnly {
		idx, err := bleve.OpenUsing(path, map[string]interface{}{"read_only": true})
		if err != nil {
			return nil, fmt.Errorf("open bleve index read-only at %s: %w", path, err)
		}
		return idx, nil
	}

	idx, err := bleve.Open(path)
	if errors.Is(err, bleve.ErrorIndexPathDoesNotExist) {
		idx, err = bleve.New(path, buildMapping())
	}
	if err != nil {
		return nil, fmt.Errorf("open bleve index at %s: %w", path, err)
	}
	return idx, nil
}

// openIndexTimed runs openIndex in a goroutine and returns
// ErrOpenTimedOut if the call doesn't finish before the deadline.
//
// Caveat: a timed-out goroutine is leaked — it stays parked inside
// bbolt's flock syscall until the kernel hands the lock over (which
// may be never if the writer never exits). The leaked goroutine
// holds nothing else; once it eventually returns, the open Bleve
// handle is closed and discarded. We accept the leak because the
// alternative (forking Bleve to plumb a timeout into bbolt.Options)
// is far more invasive.
func openIndexTimed(path string, readOnly bool, deadline time.Duration) (bleve.Index, error) {
	type result struct {
		idx bleve.Index
		err error
	}
	ch := make(chan result, 1)
	go func() {
		idx, err := openIndex(path, readOnly)
		select {
		case ch <- result{idx, err}:
		default:
			// Caller already gave up. Discard the handle so we don't
			// leave a stale Bleve open in our own process.
			if idx != nil {
				_ = idx.Close()
			}
		}
	}()

	select {
	case r := <-ch:
		return r.idx, r.err
	case <-time.After(deadline):
		return nil, ErrOpenTimedOut
	}
}

// Reload closes and reopens the index. Read-only callers use this to
// pick up changes the writer process has committed to disk. No-op for
// writers (and a hard error if called).
func (s *Store) Reload() error {
	if !s.readOnly {
		return errors.New("Reload() is only valid on a read-only Store")
	}
	fresh, err := openIndex(s.path, true)
	if err != nil {
		return err
	}
	s.idxMu.Lock()
	old := s.idx
	s.idx = fresh
	s.idxMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	return nil
}

// Promote switches a read-only Store to writable in place. Caller is
// responsible for already holding the writer flock (the orchestration
// is in cmd/paprika-3-mcp/main.go's reader→writer promotion path).
// After Promote returns nil, Upsert/Delete succeed and Reload() is no
// longer valid. Idempotent if already a writer.
func (s *Store) Promote() error {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if !s.readOnly {
		return nil
	}
	if err := s.idx.Close(); err != nil {
		return fmt.Errorf("close read-only index before promote: %w", err)
	}
	fresh, err := openIndex(s.path, false)
	if err != nil {
		return fmt.Errorf("reopen index writable for promote: %w", err)
	}
	s.idx = fresh
	s.readOnly = false
	return nil
}

// ReadOnly reports whether this Store rejects writes.
func (s *Store) ReadOnly() bool { return s.readOnly }

func (s *Store) Close() error {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	return s.idx.Close()
}

// Upsert indexes (or replaces) a single recipe. Trashed recipes are deleted
// from the index instead of stored, so search results never include them.
// Returns ErrReadOnly when the Store is read-only.
func (s *Store) Upsert(r *paprika.Recipe) error {
	if s.readOnly {
		return ErrReadOnly
	}
	if r == nil || r.UID == "" {
		return errors.New("recipe must have UID")
	}
	s.upsertMu.Lock()
	defer s.upsertMu.Unlock()

	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	if r.InTrash {
		return s.idx.Delete(r.UID)
	}
	return s.idx.Index(r.UID, toDoc(r))
}

// Delete removes a UID from the index. No error if it isn't there.
// Returns ErrReadOnly when the Store is read-only.
func (s *Store) Delete(uid string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	s.upsertMu.Lock()
	defer s.upsertMu.Unlock()
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	return s.idx.Delete(uid)
}

// Get reconstructs a Recipe by retrieving its stored _raw blob via a
// DocID query — simpler and more portable than poking at the internal
// index.Document interface.
func (s *Store) Get(uid string) (*paprika.Recipe, error) {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	q := bleve.NewDocIDQuery([]string{uid})
	req := bleve.NewSearchRequestOptions(q, 1, 0, false)
	req.Fields = []string{"_raw"}
	res, err := s.idx.Search(req)
	if err != nil {
		return nil, err
	}
	if len(res.Hits) == 0 {
		return nil, nil
	}
	raw, ok := res.Hits[0].Fields["_raw"].(string)
	if !ok || raw == "" {
		return nil, errors.New("stored recipe missing _raw blob")
	}
	var r paprika.Recipe
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("decode stored recipe: %w", err)
	}
	return &r, nil
}

// HashesByUID returns every (uid -> hash) pair currently in the index.
// Used by the syncer to decide which recipes need refetching.
func (s *Store) HashesByUID() (map[string]string, error) {
	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	q := bleve.NewMatchAllQuery()
	out := map[string]string{}

	const pageSize = 500
	from := 0
	for {
		req := bleve.NewSearchRequestOptions(q, pageSize, from, false)
		req.Fields = []string{"hash"}
		res, err := s.idx.Search(req)
		if err != nil {
			return nil, err
		}
		if len(res.Hits) == 0 {
			break
		}
		for _, hit := range res.Hits {
			h, _ := hit.Fields["hash"].(string)
			out[hit.ID] = h
		}
		from += len(res.Hits)
		if uint64(from) >= res.Total {
			break
		}
	}
	return out, nil
}

// SearchHit is the public projection of a search match.
type SearchHit struct {
	UID        string              `json:"uid"`
	Name       string              `json:"name"`
	Score      float64             `json:"score"`
	Categories []string            `json:"categories,omitempty"`
	Rating     int                 `json:"rating,omitempty"`
	Source     string              `json:"source,omitempty"`
	Snippets   map[string][]string `json:"snippets,omitempty"`
}

// SearchOptions controls a query. All fields optional.
type SearchOptions struct {
	Query     string // free-text query (Bleve query string syntax)
	Limit     int
	MinRating int    // 0 = no minimum
	Category  string // exact match against indexed categories
}

// Search runs a query and returns ranked hits with highlighted snippets.
func (s *Store) Search(opts SearchOptions) ([]SearchHit, error) {
	if opts.Limit <= 0 {
		opts.Limit = 10
	}

	var qs []query.Query
	if q := buildUserQuery(opts.Query); q != nil {
		qs = append(qs, q)
	}
	if opts.MinRating > 0 {
		min := float64(opts.MinRating)
		rq := bleve.NewNumericRangeQuery(&min, nil)
		rq.SetField("rating")
		qs = append(qs, rq)
	}
	if opts.Category != "" {
		tq := bleve.NewTermQuery(opts.Category)
		tq.SetField("categories")
		qs = append(qs, tq)
	}

	var combined query.Query
	switch len(qs) {
	case 0:
		combined = bleve.NewMatchAllQuery()
	case 1:
		combined = qs[0]
	default:
		combined = bleve.NewConjunctionQuery(qs...)
	}

	req := bleve.NewSearchRequestOptions(combined, opts.Limit, 0, false)
	req.Fields = []string{"name", "categories", "rating", "source"}
	req.Highlight = bleve.NewHighlight()
	req.Highlight.Fields = []string{"name", "ingredients", "directions", "notes", "description"}

	s.idxMu.RLock()
	defer s.idxMu.RUnlock()
	res, err := s.idx.Search(req)
	if err != nil {
		return nil, err
	}
	return projectHits(res.Hits), nil
}

func buildUserQuery(q string) query.Query {
	if q == "" {
		return nil
	}
	// QueryStringQuery gives the user fielded syntax (`name:chili`),
	// phrases (`"smoked paprika"`), boolean operators, and fuzziness
	// suffixes (`pinto~`). The default field is _all, which Bleve
	// composes from every text field with IncludeInAll=true.
	return bleve.NewQueryStringQuery(q)
}

func projectHits(hits search.DocumentMatchCollection) []SearchHit {
	out := make([]SearchHit, 0, len(hits))
	for _, h := range hits {
		hit := SearchHit{
			UID:   h.ID,
			Score: h.Score,
		}
		if name, ok := h.Fields["name"].(string); ok {
			hit.Name = name
		}
		if src, ok := h.Fields["source"].(string); ok {
			hit.Source = src
		}
		if r, ok := h.Fields["rating"].(float64); ok {
			hit.Rating = int(r)
		}
		hit.Categories = stringsField(h.Fields["categories"])
		if len(h.Fragments) > 0 {
			hit.Snippets = map[string][]string{}
			for f, frags := range h.Fragments {
				hit.Snippets[f] = frags
			}
		}
		out = append(out, hit)
	}
	return out
}

func stringsField(v interface{}) []string {
	switch x := v.(type) {
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []interface{}:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		sort.Strings(out)
		return out
	}
	return nil
}

// recipeDoc is the projection we hand to Bleve. We keep the full marshalled
// recipe under _raw so Get can return a fully-faithful object without
// a second round-trip to Paprika.
type recipeDoc struct {
	Type        string   `json:"_type"`
	Raw         string   `json:"_raw"`
	UID         string   `json:"uid"`
	Hash        string   `json:"hash"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Ingredients string   `json:"ingredients"`
	Directions  string   `json:"directions"`
	Notes       string   `json:"notes"`
	Source      string   `json:"source"`
	SourceURL   string   `json:"source_url"`
	Categories  []string `json:"categories"`
	Rating      int      `json:"rating"`
	Servings    string   `json:"servings"`
	PrepTime    string   `json:"prep_time"`
	CookTime    string   `json:"cook_time"`
	Difficulty  string   `json:"difficulty"`
}

func toDoc(r *paprika.Recipe) recipeDoc {
	raw, _ := json.Marshal(r)
	return recipeDoc{
		Type:        docType,
		Raw:         string(raw),
		UID:         r.UID,
		Hash:        r.Hash,
		Name:        r.Name,
		Description: r.Description,
		Ingredients: r.Ingredients,
		Directions:  r.Directions,
		Notes:       r.Notes,
		Source:      r.Source,
		SourceURL:   r.SourceURL,
		Categories:  r.Categories,
		Rating:      r.Rating,
		Servings:    r.Servings,
		PrepTime:    r.PrepTime,
		CookTime:    r.CookTime,
		Difficulty:  r.Difficulty,
	}
}

func buildMapping() mapping.IndexMapping {
	im := bleve.NewIndexMapping()

	// English analyzer applies lowercasing, stop-word removal, and Porter
	// stemming, which is what makes "pinto bean" match "pinto beans" and
	// "smoked paprika" match "smoke paprika".
	enText := bleve.NewTextFieldMapping()
	enText.Analyzer = en.AnalyzerName
	enText.Store = true
	enText.IncludeInAll = true

	// Stored-but-not-analyzed fields (UID, hash, prep_time, etc.).
	keep := bleve.NewTextFieldMapping()
	keep.Analyzer = keyword.Name
	keep.Store = true
	keep.IncludeInAll = false

	// Categories: indexed as keywords (so we can filter by exact match)
	// AND included in _all so they participate in free-text relevance.
	catField := bleve.NewTextFieldMapping()
	catField.Analyzer = keyword.Name
	catField.Store = true
	catField.IncludeInAll = true

	rating := bleve.NewNumericFieldMapping()
	rating.Store = true
	rating.IncludeInAll = false

	rawField := bleve.NewTextFieldMapping()
	rawField.Index = false
	rawField.Store = true

	dm := bleve.NewDocumentMapping()
	dm.AddFieldMappingsAt("_raw", rawField)
	dm.AddFieldMappingsAt("uid", keep)
	dm.AddFieldMappingsAt("hash", keep)
	dm.AddFieldMappingsAt("name", enText)
	dm.AddFieldMappingsAt("description", enText)
	dm.AddFieldMappingsAt("ingredients", enText)
	dm.AddFieldMappingsAt("directions", enText)
	dm.AddFieldMappingsAt("notes", enText)
	dm.AddFieldMappingsAt("source", keep)
	dm.AddFieldMappingsAt("source_url", keep)
	dm.AddFieldMappingsAt("categories", catField)
	dm.AddFieldMappingsAt("rating", rating)
	dm.AddFieldMappingsAt("servings", keep)
	dm.AddFieldMappingsAt("prep_time", keep)
	dm.AddFieldMappingsAt("cook_time", keep)
	dm.AddFieldMappingsAt("difficulty", keep)

	im.AddDocumentMapping(docType, dm)
	im.DefaultType = docType
	im.TypeField = "_type"
	return im
}
