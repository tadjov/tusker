package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tadjov/tusker/internal/bench/spec"
)

// fakeValidateAPI answers <index>/_validate/query the way Elasticsearch does:
// it rejects knn and any top-level key besides query, and explains an unknown
// query type.
func fakeValidateAPI(t *testing.T) *httptest.Server {
	t.Helper()
	es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.True(t, strings.HasSuffix(r.URL.Path, "/_validate/query"), r.URL.Path)
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		assert.NoError(t, json.Unmarshal(raw, &body))
		switch {
		case body["knn"] != nil:
			_, _ = w.Write([]byte(`{"valid": false, "explanations": [{"error": "request does not support [knn]"}]}`))
		case len(body) > 1:
			_, _ = w.Write([]byte(`{"valid": false}`))
		case strings.Contains(string(body["query"]), "bogus"):
			_, _ = w.Write([]byte(`{"valid": false, "explanations": [{"error": "unknown query [bogus]"}]}`))
		default:
			_, _ = w.Write([]byte(`{"valid": true}`))
		}
	}))
	t.Cleanup(es.Close)
	return es
}

func TestEsExecutor_Validate(t *testing.T) {
	const vector = `"field": "embedding", "query_vector": [0.1, 0.2], "k": 10`
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "query DSL", body: `{"query": {"match_all": {}}}`},
		{name: "query with search options", body: `{"query": {"match_all": {}}, "size": 10}`},
		{name: "unknown query type", body: `{"query": {"bogus": {}}}`, wantErr: "bogus"},
		{name: "knn only", body: `{"knn": {` + vector + `}}`},
		{name: "query and knn", body: `{"query": {"match_all": {}}, "knn": {` + vector + `}}`},
		{name: "knn array", body: `{"knn": [{` + vector + `}, {` + vector + `}]}`},
		{name: "knn embeds the query itself", body: `{"knn": {"field": "embedding", "query_vector_builder": {"text_embedding": {}}}}`},
		{name: "knn without a field", body: `{"knn": {"query_vector": [0.1]}}`, wantErr: `"field"`},
		{name: "knn with an empty field", body: `{"knn": {"field": "", "query_vector": [0.1]}}`, wantErr: "non-empty"},
		{name: "knn without a vector", body: `{"knn": {"field": "embedding", "k": 10}}`, wantErr: "query_vector"},
		{name: "empty knn array", body: `{"knn": []}`, wantErr: "empty"},
		{name: "bad entry in a knn array", body: `{"knn": [{` + vector + `}, {"field": "e"}]}`, wantErr: "knn[1]"},
		{name: "knn beside a broken query", body: `{"query": {"bogus": {}}, "knn": {` + vector + `}}`, wantErr: "bogus"},
	}
	es := fakeValidateAPI(t)
	exec := NewEsExecutor("es", es.URL, "articles")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := exec.Validate(context.Background(), Request{Query: tt.body})

			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestEsExecutor_Execute_ReportsCorpusMatchesOnlyWhenCounted(t *testing.T) {
	docID := uuid.New()
	tests := []struct {
		name  string
		total string
		want  *int64
	}{
		{name: "counted every match", total: `"total": {"value": 2466, "relation": "eq"}, `, want: ptr(int64(2466))},
		{name: "stopped counting at track_total_hits", total: `"total": {"value": 10000, "relation": "gte"}, `},
		{name: "relation absent", total: `"total": {"value": 42}, `, want: ptr(int64(42))},
		{name: "total absent with track_total_hits false"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"hits": {` + tt.total + `"hits": [{"_source": {"id": "` + docID.String() + `"}}]}}`))
			}))
			defer es.Close()

			got, err := NewEsExecutor("es", es.URL, "articles").Execute(context.Background(), Request{Query: `{"query": {"match_all": {}}}`})

			require.NoError(t, err)
			assert.Equal(t, []uuid.UUID{docID}, got.RankedDocIDs)
			assert.Equal(t, tt.want, got.CorpusMatches)
		})
	}
}

func TestEsExecutor_Execute_ReadsTheDocumentIDFromTheHitID(t *testing.T) {
	articleID, otherID := uuid.New(), uuid.New()
	tests := []struct {
		name    string
		hit     string
		want    uuid.UUID
		wantErr string
	}{
		{name: "id only, source disabled", hit: `{"_id": "` + articleID.String() + `"}`, want: articleID},
		{name: "hit id wins over the source id", hit: `{"_id": "` + articleID.String() + `", "_source": {"id": "` + otherID.String() + `"}}`, want: articleID},
		{name: "source id when the hit id is not a uuid", hit: `{"_id": "Xk3a9pQB", "_source": {"id": "` + articleID.String() + `"}}`, want: articleID},
		{name: "neither parses", hit: `{"_id": "Xk3a9pQB"}`, wantErr: "Xk3a9pQB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			es := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"hits": {"total": {"value": 1, "relation": "eq"}, "hits": [` + tt.hit + `]}}`))
			}))
			defer es.Close()

			got, err := NewEsExecutor("es", es.URL, "articles").Execute(context.Background(), Request{Query: `{"query": {"match_all": {}}, "_source": false}`})

			if tt.wantErr != "" {
				assert.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []uuid.UUID{tt.want}, got.RankedDocIDs)
		})
	}
}

func ptr[T any](v T) *T { return &v }

// stallingServer answers only after the request is abandoned, or after a
// fallback long past any test deadline so a missing deadline fails the test
// instead of hanging it.
func stallingServer(t *testing.T, answer string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices a dropped connection only once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
			_, _ = w.Write([]byte(answer))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// esCallsWithoutADeadlineOfTheirOwn are the executor calls a caller may make
// with a context that carries no deadline.
var esCallsWithoutADeadlineOfTheirOwn = []struct {
	name   string
	answer string
	call   func(ctx context.Context, exec *EsExecutor) error
}{
	{
		name:   "validate",
		answer: `{"valid": true}`,
		call: func(ctx context.Context, exec *EsExecutor) error {
			return exec.Validate(ctx, Request{Query: `{"query": {"match_all": {}}}`})
		},
	},
	{
		name:   "validate a stored template",
		answer: `{"template_output": {"query": {"match_all": {}}}}`,
		call: func(ctx context.Context, exec *EsExecutor) error {
			return exec.Validate(ctx, Request{SearchTemplateID: "t-es_match", Params: map[string]any{"terms": "climate"}})
		},
	},
	{
		name:   "corpus count",
		answer: `{"count": 1}`,
		call: func(ctx context.Context, exec *EsExecutor) error {
			_, err := exec.CorpusCount(ctx)
			return err
		},
	},
}

func TestEsExecutor_CallsWithoutACallerDeadlineStopAtTheDefaultOne(t *testing.T) {
	for _, tt := range esCallsWithoutADeadlineOfTheirOwn {
		t.Run(tt.name, func(t *testing.T) {
			exec := NewEsExecutor("es", stallingServer(t, tt.answer).URL, "articles")
			exec.defaultTimeout = 50 * time.Millisecond

			err := tt.call(context.Background(), exec)

			assert.ErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}

// deadlineRecordingTransport answers every request with answer and records the
// deadline each request's context carried.
type deadlineRecordingTransport struct {
	answer    string
	deadlines []time.Time
}

func (rt *deadlineRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline, _ := req.Context().Deadline()
	rt.deadlines = append(rt.deadlines, deadline)
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(rt.answer)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

func TestEsExecutor_CallsKeepALongerCallerDeadline(t *testing.T) {
	for _, tt := range esCallsWithoutADeadlineOfTheirOwn {
		t.Run(tt.name, func(t *testing.T) {
			transport := &deadlineRecordingTransport{answer: tt.answer}
			exec := NewEsExecutor("es", "http://es.invalid", "articles")
			exec.client = &http.Client{Transport: transport}
			callerDeadline := time.Now().Add(spec.DefaultQueryTimeout + time.Hour)
			ctx, cancel := context.WithDeadline(context.Background(), callerDeadline)
			defer cancel()

			_ = tt.call(ctx, exec)

			require.NotEmpty(t, transport.deadlines)
			for _, got := range transport.deadlines {
				assert.True(t, got.Equal(callerDeadline), "request deadline %s, caller's %s", got, callerDeadline)
			}
		})
	}
}
