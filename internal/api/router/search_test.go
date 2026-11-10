package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	apiserver "github.com/tadjov/tusker/internal/api/server"
	"github.com/tadjov/tusker/internal/apperr"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/tadjov/tusker/internal/types/operator"
	dquery "github.com/tadjov/tusker/internal/types/query"
)

type stubSemanticSearcher struct{}

func (stubSemanticSearcher) SearchSemantic(context.Context, *dquery.Semantic, *dquery.BaseOptions) (*storage.VectorSearchResult, error) {
	return &storage.VectorSearchResult{}, nil
}

// stubFtsSearcher declares caps and returns no hits.
type stubFtsSearcher struct {
	caps dquery.Capabilities
}

func (s stubFtsSearcher) Capabilities() dquery.Capabilities {
	return s.caps
}

func (stubFtsSearcher) SearchStringQuery(context.Context, *dquery.String, *dquery.BaseOptions) (*storage.SearchResult, error) {
	return &storage.SearchResult{}, nil
}
func (stubFtsSearcher) SearchField(context.Context, *dquery.Match, *dquery.BaseOptions) (*storage.SearchResult, error) {
	return &storage.SearchResult{}, nil
}
func (stubFtsSearcher) SearchFields(context.Context, *dquery.MultiMatch, *dquery.BaseOptions) (*storage.SearchResult, error) {
	return &storage.SearchResult{}, nil
}
func (stubFtsSearcher) SearchPhrase(context.Context, *dquery.Phrase, *dquery.BaseOptions) (*storage.SearchResult, error) {
	return &storage.SearchResult{}, nil
}
func (stubFtsSearcher) SearchBoolean(context.Context, *dquery.Boolean, *dquery.BaseOptions) (*storage.SearchResult, error) {
	return &storage.SearchResult{}, nil
}

type stubHybridSearcher struct{}

func (stubHybridSearcher) SearchHybrid(context.Context, *dquery.Hybrid, *dquery.BaseOptions) (*storage.SearchResult, error) {
	return &storage.SearchResult{}, nil
}

func TestStructuredSearchHandlerValidation(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode int
	}{
		{
			name:     "valid match request",
			body:     `{"size":10,"query":{"match":{"field":"title","query":"climate change","operator":"and"}}}`,
			wantCode: http.StatusOK,
		},
		{
			name:     "invalid match request missing query",
			body:     `{"query":{"match":{"field":"title","query":""}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "match on an unknown field",
			body:     `{"query":{"match":{"field":"titel","query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "match with supported fuzziness",
			body:     `{"query":{"match":{"field":"title","query":"climate","fuzziness":"auto"}}}`,
			wantCode: http.StatusOK,
		},
		{
			name:     "match without fuzziness",
			body:     `{"query":{"match":{"field":"title","query":"climate","fuzziness":"none"}}}`,
			wantCode: http.StatusOK,
		},
		{
			name:     "match with unsupported fuzziness",
			body:     `{"query":{"match":{"field":"title","query":"climate","fuzziness":"7"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match on known fields",
			body:     `{"query":{"multi_match":{"fields":["title^3","content"],"query":"climate"}}}`,
			wantCode: http.StatusOK,
		},
		{
			name:     "multi_match with one unknown field",
			body:     `{"query":{"multi_match":{"fields":["title","body"],"query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match with an unknown boosted field",
			body:     `{"query":{"multi_match":{"fields":["body^2"],"query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match with a malformed boost",
			body:     `{"query":{"multi_match":{"fields":["title^high"],"query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match with a negative boost",
			body:     `{"query":{"multi_match":{"fields":["title^-1"],"query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match with a zero boost",
			body:     `{"query":{"multi_match":{"fields":["title^0"],"query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match with a NaN boost",
			body:     `{"query":{"multi_match":{"fields":["title^NaN"],"query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match with an Inf boost",
			body:     `{"query":{"multi_match":{"fields":["title^Inf"],"query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match with a -Inf boost",
			body:     `{"query":{"multi_match":{"fields":["title^-Inf"],"query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match with an infinity boost",
			body:     `{"query":{"multi_match":{"fields":["title^infinity"],"query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match with a boost below float32",
			body:     `{"query":{"multi_match":{"fields":["title^1e-50"],"query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match with two boosts",
			body:     `{"query":{"multi_match":{"fields":["title^2^3"],"query":"climate"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "phrase on an unknown field",
			body:     `{"query":{"phrase":{"fields":["headline"],"query":"climate change"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "match with the NOT operator",
			body:     `{"query":{"match":{"field":"title","query":"climate","operator":"not"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "match in english",
			body:     `{"query":{"match":{"field":"title","query":"climate","language":"eng"}}}`,
			wantCode: http.StatusOK,
		},
		{
			name:     "match in serbian",
			body:     `{"query":{"match":{"field":"title","query":"klima","language":"serbian"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "multi_match in serbian",
			body:     `{"query":{"multi_match":{"fields":["title"],"query":"klima","language":"serbian"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "phrase in serbian",
			body:     `{"query":{"phrase":{"fields":["title"],"query":"klimatske promene","language":"serbian"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "boolean in serbian",
			body:     `{"query":{"boolean":{"expression":"klima AND promene","language":"serbian"}}}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "hybrid in english",
			body:     `{"query":{"hybrid":{"query":"climate","language":"eng"}}}`,
			wantCode: http.StatusOK,
		},
		{
			name:     "hybrid in serbian",
			body:     `{"query":{"hybrid":{"query":"klima","language":"serbian"}}}`,
			wantCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			(&apiserver.Server{Echo: e}).SetupValidator()
			e.HTTPErrorHandler = apperr.GlobalErrorHandler()

			r := &SearchRouter{e: e, searcher: stubFtsSearcher{caps: elasticsearchCaps}, hybridSearcher: stubHybridSearcher{}}
			r.Bind()

			req := httptest.NewRequest(http.MethodPost, "/v1/articles/_search", strings.NewReader(tt.body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}

func TestCapabilitiesHandler(t *testing.T) {
	tests := []struct {
		name         string
		withSemantic bool
		withHybrid   bool
	}{
		{name: "full-text only"},
		{name: "with semantic and hybrid searchers", withSemantic: true, withHybrid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &SearchRouter{e: echo.New(), searcher: stubFtsSearcher{caps: postgresCaps}}
			if tt.withSemantic {
				r.semanticSearcher = stubSemanticSearcher{}
			}
			if tt.withHybrid {
				r.hybridSearcher = stubHybridSearcher{}
			}
			r.Bind()

			rec := httptest.NewRecorder()
			r.e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}

			var got struct {
				QueryTypes map[string]bool     `json:"query_types"`
				FullText   dquery.Capabilities `json:"full_text"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("failed to decode body: %v", err)
			}
			for _, key := range []string{"query_string", "match", "multi_match", "phrase", "boolean"} {
				if !got.QueryTypes[key] {
					t.Errorf("query_types[%q] = false, want true", key)
				}
			}
			if got.QueryTypes["semantic"] != tt.withSemantic || got.QueryTypes["hybrid"] != tt.withHybrid {
				t.Errorf("semantic = %v, hybrid = %v, want %v and %v",
					got.QueryTypes["semantic"], got.QueryTypes["hybrid"], tt.withSemantic, tt.withHybrid)
			}
			if got.FullText.Engine != "postgres" || got.FullText.DefaultOperator != operator.And || got.FullText.Fuzziness {
				t.Errorf("full_text = %+v, want the searcher's declared capabilities", got.FullText)
			}
		})
	}
}

func TestSearchHandlerValidation(t *testing.T) {
	tests := []struct {
		name     string
		target   string
		wantCode int
	}{
		{name: "query only", target: "/v1/articles/search?q=climate", wantCode: http.StatusOK},
		{name: "legacy query parameter", target: "/v1/articles/search?query=climate", wantCode: http.StatusOK},
		{name: "english", target: "/v1/articles/search?q=climate&lang=eng", wantCode: http.StatusOK},
		{name: "serbian is not supported yet", target: "/v1/articles/search?q=klima&lang=srp", wantCode: http.StatusBadRequest},
		{name: "unsupported language", target: "/v1/articles/search?q=climate&lang=klingon", wantCode: http.StatusBadRequest},
		{name: "missing query", target: "/v1/articles/search", wantCode: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			e.HTTPErrorHandler = apperr.GlobalErrorHandler()
			r := &SearchRouter{e: e, searcher: stubFtsSearcher{caps: elasticsearchCaps}}
			r.Bind()

			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.target, nil))

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d (body: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
		})
	}
}
