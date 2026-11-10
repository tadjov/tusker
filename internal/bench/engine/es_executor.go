package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"time"

	"github.com/google/uuid"
	"github.com/tadjov/tusker/internal/bench/spec"
)

type EsExecutor struct {
	name    string
	baseURL string
	index   string
	client  *http.Client
	// defaultTimeout bounds a call whose caller set no deadline.
	defaultTimeout time.Duration
}

func NewEsExecutor(name, baseURL, index string) *EsExecutor {
	return &EsExecutor{
		name:    name,
		baseURL: baseURL,
		index:   index,
		client:  &http.Client{},

		defaultTimeout: spec.DefaultQueryTimeout,
	}
}

// withDeadline keeps the caller's deadline, or sets the default one: the client
// carries no timeout of its own.
func (e *EsExecutor) withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, e.defaultTimeout)
}

func (e *EsExecutor) Execute(ctx context.Context, search Request) (*Execution, error) {
	url := fmt.Sprintf("%s/%s/_search", e.baseURL, e.index)
	body := []byte(search.Query)
	if search.SearchTemplateID != "" {
		url = fmt.Sprintf("%s/%s/_search/template", e.baseURL, e.index)
		var err error
		if body, err = storedTemplateCall(search); err != nil {
			return nil, err
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("es create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("es request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("es read response: %w", err)
	}
	latency := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("es status %d: %s", resp.StatusCode, string(respBody))
	}

	var esResp esSearchResponse
	if err := json.Unmarshal(respBody, &esResp); err != nil {
		return nil, fmt.Errorf("es parse response: %w", err)
	}

	ids := make([]uuid.UUID, 0, len(esResp.Hits.Hits))
	for _, hit := range esResp.Hits.Hits {
		id, err := hit.articleID()
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}

	return &Execution{
		RankedDocIDs:  ids,
		CorpusMatches: esResp.Hits.Total.exactCount(),
		Latency:       latency,
	}, nil
}

func storedTemplateCall(search Request) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"id": search.SearchTemplateID, "params": search.Params})
	if err != nil {
		return nil, fmt.Errorf("es encode template %q params: %w", search.SearchTemplateID, err)
	}
	return body, nil
}

// RegisterSearchTemplate stores the template's Mustache source with
// PUT _scripts/<id>, replacing any earlier version.
func (e *EsExecutor) RegisterSearchTemplate(ctx context.Context, template SearchTemplate) error {
	body, err := json.Marshal(map[string]any{
		"script": map[string]string{"lang": "mustache", "source": template.Source},
	})
	if err != nil {
		return fmt.Errorf("es encode template %q: %w", template.ID, err)
	}
	url := fmt.Sprintf("%s/_scripts/%s", e.baseURL, neturl.PathEscape(template.ID))
	if _, err := e.send(ctx, http.MethodPut, url, body); err != nil {
		return fmt.Errorf("es store template %q: %w", template.ID, err)
	}
	return nil
}

// renderTemplate returns the search body ES builds from a stored template and
// its params, without running it.
func (e *EsExecutor) renderTemplate(ctx context.Context, search Request) (string, error) {
	body, err := storedTemplateCall(search)
	if err != nil {
		return "", err
	}
	respBody, err := e.send(ctx, http.MethodPost, e.baseURL+"/_render/template", body)
	if err != nil {
		return "", fmt.Errorf("es render template %q: %w", search.SearchTemplateID, err)
	}
	var rendered struct {
		TemplateOutput json.RawMessage `json:"template_output"`
	}
	if err := json.Unmarshal(respBody, &rendered); err != nil {
		return "", fmt.Errorf("es render template %q: %w", search.SearchTemplateID, err)
	}
	return string(rendered.TemplateOutput), nil
}

func (e *EsExecutor) send(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

func (e *EsExecutor) CorpusCount(ctx context.Context) (int64, error) {
	ctx, cancel := e.withDeadline(ctx)
	defer cancel()
	url := fmt.Sprintf("%s/%s/_count", e.baseURL, e.index)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("es count request: %w", err)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("es count http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("es count read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("es count status %d: %s", resp.StatusCode, string(body))
	}
	var c struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return 0, fmt.Errorf("es count parse: %w", err)
	}
	return c.Count, nil
}

func (e *EsExecutor) Name() string { return e.name }
func (e *EsExecutor) Close() error { return nil }

// Validate posts the query to <index>/_validate/query?explain=true. ES parses
// the JSON, type-checks fields, and returns "valid: false" with an explanation
// for malformed queries — no documents scanned. A stored template is rendered
// with its params first and the rendered body validated.
//
// Bodies that use top-level `knn` (vector search) are routed to
// validateKnnBody: _validate/query only understands the Query DSL and rejects
// `knn` outright ("request does not support [knn]"), so the knn clause is
// checked structurally instead.
func (e *EsExecutor) Validate(ctx context.Context, search Request) error {
	ctx, cancel := e.withDeadline(ctx)
	defer cancel()
	rawQuery := search.Query
	if search.SearchTemplateID != "" {
		rendered, err := e.renderTemplate(ctx, search)
		if err != nil {
			return err
		}
		rawQuery = rendered
	}
	if hasKnn(rawQuery) {
		return e.validateKnnBody(ctx, rawQuery)
	}
	url := fmt.Sprintf("%s/%s/_validate/query?explain=true", e.baseURL, e.index)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBufferString(rawQuery))
	if err != nil {
		return fmt.Errorf("es validate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("es validate http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("es validate read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("es validate status %d: %s", resp.StatusCode, string(body))
	}

	var v esValidateResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return fmt.Errorf("es validate parse: %w", err)
	}
	if !v.Valid {
		for _, exp := range v.Explanations {
			if exp.Error != "" {
				return fmt.Errorf("es invalid: %s", exp.Error)
			}
		}
		// _validate/query rejects top-level fields like "size","from","aggs".
		// Strip them and retry with just the query block before failing.
		stripped, ok := stripToQueryBody(rawQuery)
		if !ok {
			return fmt.Errorf("es invalid: %s", string(body))
		}
		return e.validateBody(ctx, stripped, body)
	}
	return nil
}

// validateBody is a second-chance validation against a stripped query body
// (just the `query` field). origBody is the original response, used in the
// error message if the retry also fails.
func (e *EsExecutor) validateBody(ctx context.Context, body []byte, origBody []byte) error {
	url := fmt.Sprintf("%s/%s/_validate/query?explain=true", e.baseURL, e.index)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("es validate retry: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("es validate retry http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	var v esValidateResponse
	if err := json.Unmarshal(respBody, &v); err != nil {
		return fmt.Errorf("es invalid: %s", string(origBody))
	}
	if !v.Valid {
		for _, exp := range v.Explanations {
			if exp.Error != "" {
				return fmt.Errorf("es invalid: %s", exp.Error)
			}
		}
		return fmt.Errorf("es invalid: %s", string(respBody))
	}
	return nil
}

// stripToQueryBody extracts just the "query" field from an ES search body —
// _validate/query rejects top-level "size", "from", "aggs", etc.
func stripToQueryBody(raw string) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, false
	}
	q, ok := m["query"]
	if !ok {
		return nil, false
	}
	out, err := json.Marshal(map[string]json.RawMessage{"query": q})
	if err != nil {
		return nil, false
	}
	return out, true
}

// hasKnn reports whether the search body carries a top-level `knn` clause.
func hasKnn(raw string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return false
	}
	_, ok := m["knn"]
	return ok
}

// validateKnnBody validates a vector-search body. The `knn` clause is checked
// structurally; an accompanying `query` block, if present, is validated against
// _validate/query as usual.
func (e *EsExecutor) validateKnnBody(ctx context.Context, rawQuery string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rawQuery), &m); err != nil {
		return fmt.Errorf("es invalid: body is not JSON: %w", err)
	}
	if err := validateKnnClause(m["knn"]); err != nil {
		return fmt.Errorf("es invalid: %w", err)
	}
	if q, ok := m["query"]; ok {
		body, err := json.Marshal(map[string]json.RawMessage{"query": q})
		if err != nil {
			return fmt.Errorf("es invalid: %w", err)
		}
		return e.validateBody(ctx, body, []byte(rawQuery))
	}
	return nil
}

// validateKnnClause accepts a single knn object or an array of them.
func validateKnnClause(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("knn clause is empty")
	}
	var single map[string]json.RawMessage
	if err := json.Unmarshal(raw, &single); err == nil {
		return validateKnnEntry(single)
	}
	var many []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &many); err != nil {
		return fmt.Errorf("knn must be an object or array of objects")
	}
	if len(many) == 0 {
		return fmt.Errorf("knn array is empty")
	}
	for i, entry := range many {
		if err := validateKnnEntry(entry); err != nil {
			return fmt.Errorf("knn[%d]: %w", i, err)
		}
	}
	return nil
}

func validateKnnEntry(entry map[string]json.RawMessage) error {
	field, ok := entry["field"]
	if !ok {
		return fmt.Errorf("knn missing required \"field\"")
	}
	var fieldName string
	if err := json.Unmarshal(field, &fieldName); err != nil || fieldName == "" {
		return fmt.Errorf("knn \"field\" must be a non-empty string")
	}
	_, hasVec := entry["query_vector"]
	_, hasBuilder := entry["query_vector_builder"]
	if !hasVec && !hasBuilder {
		return fmt.Errorf("knn requires \"query_vector\" or \"query_vector_builder\"")
	}
	return nil
}

type esValidateResponse struct {
	Valid        bool                    `json:"valid"`
	Explanations []esValidateExplanation `json:"explanations,omitempty"`
}

type esValidateExplanation struct {
	Index string `json:"index"`
	Valid bool   `json:"valid"`
	Error string `json:"error,omitempty"`
}

type esSearchResponse struct {
	Hits esHits `json:"hits"`
}

type esHits struct {
	Total *esTotal `json:"total"`
	Hits  []esHit  `json:"hits"`
}

// esTotal is ES hits.total, absent when track_total_hits is false. Relation is
// "gte" when ES stopped counting at track_total_hits rather than counting every
// match.
type esTotal struct {
	Value    int64  `json:"value"`
	Relation string `json:"relation"`
}

func (t *esTotal) exactCount() *int64 {
	if t == nil || t.Relation == "gte" {
		return nil
	}
	v := t.Value
	return &v
}

type esHit struct {
	ID     string   `json:"_id"`
	Source esSource `json:"_source"`
}

// articleID reads the article id from _id, which the indexer sets to it, so a
// search can disable _source. _source.id covers an index whose _id is not the
// article id.
func (h esHit) articleID() (uuid.UUID, error) {
	if id, err := uuid.Parse(h.ID); err == nil {
		return id, nil
	}
	id, err := uuid.Parse(h.Source.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("es parse doc id: _id %q and _source.id %q are not uuids: %w", h.ID, h.Source.ID, err)
	}
	return id, nil
}

type esSource struct {
	ID string `json:"id"`
}
