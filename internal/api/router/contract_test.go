package router

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	apiserver "github.com/tadjov/tusker/internal/api/server"
	"github.com/tadjov/tusker/internal/apperr"
	"github.com/tadjov/tusker/internal/storage/es"
	"github.com/tadjov/tusker/internal/storage/pg/native"
	"github.com/tadjov/tusker/internal/types/language"
	"github.com/tadjov/tusker/internal/types/operator"
	dquery "github.com/tadjov/tusker/internal/types/query"
)

// The capabilities the real searchers declare, so a change to either shows up
// in these tests. Capabilities reads no connection.
var (
	postgresCaps      = new(native.Searcher).Capabilities()
	elasticsearchCaps = new(es.Searcher).Capabilities()
)

type contractResponse struct {
	Effective dquery.Effective `json:"effective"`
	Ignored   []dquery.Ignored `json:"ignored"`
}

type searchCall struct {
	caps   dquery.Capabilities
	method string
	target string
	body   string
	prefer string
}

func serveSearch(t *testing.T, call searchCall) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	(&apiserver.Server{Echo: e}).SetupValidator()
	e.HTTPErrorHandler = apperr.GlobalErrorHandler()
	r := &SearchRouter{e: e, searcher: stubFtsSearcher{caps: call.caps}, hybridSearcher: stubHybridSearcher{}}
	r.Bind()

	method, target := call.method, call.target
	if method == "" {
		method, target = http.MethodPost, "/v1/articles/_search"
	}
	req := httptest.NewRequest(method, target, strings.NewReader(call.body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if call.prefer != "" {
		req.Header.Set("Prefer", call.prefer)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func decodeContract(t *testing.T, rec *httptest.ResponseRecorder) contractResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, key := range []string{"effective", "ignored"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("response has no %q (body: %s)", key, rec.Body.String())
		}
	}
	var got contractResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return got
}

func TestEveryEngineMatchesEachSearchableFieldApart(t *testing.T) {
	for _, caps := range []dquery.Capabilities{postgresCaps, elasticsearchCaps} {
		if !slices.Equal(caps.Fields, dquery.SearchableFields()) {
			t.Errorf("%s fields = %v, want %v", caps.Engine, caps.Fields, dquery.SearchableFields())
		}
	}
}

func TestStrictSearchRejectsAParamTheEngineCannotHonour(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantParam string
	}{
		{
			name:      "fuzziness",
			body:      `{"query":{"match":{"field":"title","query":"climate","fuzziness":"auto"}}}`,
			wantParam: "fuzziness",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, prefer := range []string{"", "handling=strict", "handling=relaxed"} {
				rec := serveSearch(t, searchCall{caps: postgresCaps, body: tt.body, prefer: prefer})
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("Prefer %q: status = %d, want 400 (body: %s)", prefer, rec.Code, rec.Body.String())
				}
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				if !strings.Contains(body["error"], tt.wantParam) || !strings.Contains(body["error"], "postgres") {
					t.Errorf("Prefer %q: error = %q, want it to name %s and postgres", prefer, body["error"], tt.wantParam)
				}
			}

			if rec := serveSearch(t, searchCall{caps: elasticsearchCaps, body: tt.body}); rec.Code != http.StatusOK {
				t.Errorf("on elasticsearch: status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestLenientSearchRunsAndReportsWhatItIgnored(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantIgnored []string
		wantFields  []dquery.Field
	}{
		{
			name:        "fuzziness",
			body:        `{"query":{"match":{"field":"title","query":"climate","fuzziness":"auto"}}}`,
			wantIgnored: []string{"fuzziness"},
			wantFields:  []dquery.Field{dquery.FieldTitle},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveSearch(t, searchCall{caps: postgresCaps, body: tt.body, prefer: "respond-async, handling=lenient"})
			got := decodeContract(t, rec)

			var params []string
			for _, ig := range got.Ignored {
				if ig.Engine != "postgres" || ig.Reason == "" {
					t.Errorf("ignored = %+v, want it to name postgres and a reason", ig)
				}
				params = append(params, ig.Param)
			}
			slices.Sort(params)
			if !slices.Equal(params, tt.wantIgnored) {
				t.Errorf("ignored params = %v, want %v", params, tt.wantIgnored)
			}
			if !slices.Equal(got.Effective.Fields, tt.wantFields) {
				t.Errorf("effective fields = %v, want %v", got.Effective.Fields, tt.wantFields)
			}
			if applied := rec.Header().Get("Preference-Applied"); applied != "handling=lenient" {
				t.Errorf("Preference-Applied = %q, want handling=lenient", applied)
			}
		})
	}
}

func TestSearchEchoesTheEffectiveContract(t *testing.T) {
	contractFields := []dquery.Field{dquery.FieldTitle, dquery.FieldDescription, dquery.FieldContent}
	tests := []struct {
		name string
		call searchCall
		want dquery.Effective
	}{
		{
			name: "an omitted operator is the postgres default",
			call: searchCall{caps: postgresCaps, body: `{"query":{"match":{"field":"title","query":"climate change"}}}`},
			want: dquery.Effective{Operator: operator.And, Fields: []dquery.Field{dquery.FieldTitle}, Language: language.English},
		},
		{
			name: "an omitted operator is the elasticsearch default",
			call: searchCall{caps: elasticsearchCaps, body: `{"query":{"match":{"field":"title","query":"climate change"}}}`},
			want: dquery.Effective{Operator: operator.Or, Fields: []dquery.Field{dquery.FieldTitle}, Language: language.English},
		},
		{
			name: "a stated operator overrides the engine default",
			call: searchCall{caps: postgresCaps, body: `{"query":{"match":{"field":"title","query":"climate change","operator":"or"}}}`},
			want: dquery.Effective{Operator: operator.Or, Fields: []dquery.Field{dquery.FieldTitle}, Language: language.English},
		},
		{
			name: "multi_match fields without their boosts",
			call: searchCall{caps: postgresCaps, body: `{"query":{"multi_match":{"fields":["title^3","content"],"query":"climate","operator":"and"}}}`},
			want: dquery.Effective{Operator: operator.And, Fields: []dquery.Field{dquery.FieldTitle, dquery.FieldContent}, Language: language.English},
		},
		{
			name: "a phrase has no operator",
			call: searchCall{caps: postgresCaps, body: `{"query":{"phrase":{"fields":["title"],"query":"climate change"}}}`},
			want: dquery.Effective{Fields: []dquery.Field{dquery.FieldTitle}, Language: language.English},
		},
		{
			name: "boolean joins adjacent words with the engine default",
			call: searchCall{caps: elasticsearchCaps, body: `{"query":{"boolean":{"expression":"climate change"}}}`},
			want: dquery.Effective{Operator: operator.Or, Fields: contractFields, Language: language.English},
		},
		{
			name: "the search box is free text over the contract fields",
			call: searchCall{caps: elasticsearchCaps, method: http.MethodGet, target: "/v1/articles/search?q=climate+change"},
			want: dquery.Effective{Operator: operator.Or, Fields: contractFields, Language: language.English},
		},
		{
			name: "hybrid leaves the fields to the engine",
			call: searchCall{caps: postgresCaps, body: `{"query":{"hybrid":{"query":"climate change"}}}`},
			want: dquery.Effective{Operator: operator.And, Language: language.English},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decodeContract(t, serveSearch(t, tt.call))
			if got.Effective.Operator != tt.want.Operator || got.Effective.Language != tt.want.Language ||
				!slices.Equal(got.Effective.Fields, tt.want.Fields) {
				t.Errorf("effective = %+v, want %+v", got.Effective, tt.want)
			}
			if len(got.Ignored) != 0 {
				t.Errorf("ignored = %+v, want none", got.Ignored)
			}
		})
	}
}

func TestSearchRejectsAParamItDoesNotKnow(t *testing.T) {
	tests := []struct {
		name    string
		call    searchCall
		wantKey string
	}{
		{
			name:    "fuzziness on multi_match",
			call:    searchCall{body: `{"query":{"multi_match":{"fields":["title"],"query":"climate","fuzziness":"AUTO"}}}`},
			wantKey: "fuzziness",
		},
		{
			name:    "field_weights on multi_match",
			call:    searchCall{body: `{"query":{"multi_match":{"fields":["title"],"query":"climate","field_weights":{"title":3}}}}`},
			wantKey: "field_weights",
		},
		{
			name:    "a misspelt operator on match",
			call:    searchCall{body: `{"query":{"match":{"field":"title","query":"climate change","operater":"or"}}}`},
			wantKey: "operater",
		},
		{
			name:    "operator on phrase",
			call:    searchCall{body: `{"query":{"phrase":{"fields":["title"],"query":"climate change","operator":"or"}}}`},
			wantKey: "operator",
		},
		{
			name:    "operator on boolean",
			call:    searchCall{body: `{"query":{"boolean":{"expression":"climate change","operator":"and"}}}`},
			wantKey: "operator",
		},
		{
			name:    "fields on boolean",
			call:    searchCall{body: `{"query":{"boolean":{"expression":"climate","fields":["title"]}}}`},
			wantKey: "fields",
		},
		{
			name:    "operator on hybrid",
			call:    searchCall{body: `{"query":{"hybrid":{"query":"climate","operator":"or"}}}`},
			wantKey: "operator",
		},
		{
			name:    "an unknown top-level key",
			call:    searchCall{body: `{"query":{"match":{"field":"title","query":"climate"}},"query_extra":1}`},
			wantKey: "query_extra",
		},
		{
			name:    "operator on the search box",
			call:    searchCall{method: http.MethodGet, target: "/v1/articles/search?q=climate+change&operator=or"},
			wantKey: "operator",
		},
		{
			name:    "fields on the search box",
			call:    searchCall{method: http.MethodGet, target: "/v1/articles/search?q=climate&fields=title"},
			wantKey: "fields",
		},
		{
			name:    "fuzziness on the search box",
			call:    searchCall{method: http.MethodGet, target: "/v1/articles/search?q=climate&fuzziness=auto"},
			wantKey: "fuzziness",
		},
		{
			name:    "an unknown parameter on the search box",
			call:    searchCall{method: http.MethodGet, target: "/v1/articles/search?q=climate&sort=date"},
			wantKey: "sort",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.call.caps = elasticsearchCaps
			rec := serveSearch(t, tt.call)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if !strings.Contains(body["error"], `"`+tt.wantKey+`"`) {
				t.Errorf("error = %q, want it to name %q", body["error"], tt.wantKey)
			}
		})
	}
}

func TestStructuredSearchRejectsDataAfterTheBody(t *testing.T) {
	rec := serveSearch(t, searchCall{caps: elasticsearchCaps, body: `{"query":{"match":{"field":"title","query":"climate"}}} {"size":1}`})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestSearchBoxAcceptsTheParamsItDocuments(t *testing.T) {
	rec := serveSearch(t, searchCall{
		caps:   elasticsearchCaps,
		method: http.MethodGet,
		target: "/v1/articles/search?q=climate&size=5&lang=eng",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestStructuredSearchOverTheBodyLimitKeepsItsStatus(t *testing.T) {
	e := echo.New()
	(&apiserver.Server{Echo: e}).SetupValidator()
	e.HTTPErrorHandler = apperr.GlobalErrorHandler()
	e.Use(middleware.BodyLimit("64"))
	(&SearchRouter{e: e, searcher: stubFtsSearcher{caps: elasticsearchCaps}}).Bind()

	body := `{"query":{"match":{"field":"title","query":"` + strings.Repeat("a", 200) + `"}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/articles/_search", io.NopCloser(strings.NewReader(body)))
	req.ContentLength = -1
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body: %s)", rec.Code, rec.Body.String())
	}
}
