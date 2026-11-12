package queryvectors_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
	"github.com/tadjov/tusker/internal/datapackage"
	"github.com/tadjov/tusker/internal/storage/pg"
	"github.com/tadjov/tusker/internal/storage/queryvectors"
	pkgtesting "github.com/tadjov/tusker/pkg/testing"
)

// The fixture holds two queries at width 1024; q2 is the unit vector e0.
const fixtureTag = "qwen3-embedding:0.6b@1024"

func copyFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("fixtures", "track")
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

func openTrack(t *testing.T, track string, opts queryvectors.Options) (*queryvectors.Store, error) {
	t.Helper()
	opts.Package = filepath.Join(track, "trec", "query-vectors")
	opts.Suite = filepath.Join(track, "suite.yaml")
	return queryvectors.Open(context.Background(), opts)
}

func TestQueryVector_ServesTheStoredVectorForAKnownText(t *testing.T) {
	store, err := openTrack(t, copyFixture(t), queryvectors.Options{Model: fixtureTag})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got, err := store.QueryVector(context.Background(), "central bank interest rates")
	if err != nil {
		t.Fatalf("QueryVector: %v", err)
	}

	if len(got) != 1024 || got[0] != 1 {
		t.Fatalf("vector = %d wide starting %v, want the 1024-wide unit vector e0", len(got), got[:min(len(got), 2)])
	}
	for i, v := range got[1:] {
		if v != 0 {
			t.Fatalf("component %d = %v, want 0", i+1, v)
		}
	}
	if store.Model() != fixtureTag {
		t.Errorf("Model() = %q, want %q", store.Model(), fixtureTag)
	}
}

func TestQueryVector_AnUnknownTextFailsNamingIt(t *testing.T) {
	store, err := openTrack(t, copyFixture(t), queryvectors.Options{Model: fixtureTag})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	_, err = store.QueryVector(context.Background(), "central bank interest rate")

	var unknown *queryvectors.UnknownQueryError
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want an UnknownQueryError", err)
	}
	if !strings.Contains(err.Error(), `"central bank interest rate"`) {
		t.Errorf("err = %q, want it to name the text", err)
	}
}

func editSuite(t *testing.T, track, from, to string) {
	t.Helper()
	suitePath := filepath.Join(track, "suite.yaml")
	suite, err := os.ReadFile(suitePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(suite), from) {
		t.Fatalf("fixture suite has no %q", from)
	}
	if err := os.WriteFile(suitePath, []byte(strings.Replace(string(suite), from, to, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

const fixtureQ2 = `  - id: q2
    text: "central bank interest rates"
    description: "Central banks setting interest rates"
`

func TestOpen_RefusesASuiteQueryThePackageHoldsNoVectorFor(t *testing.T) {
	tests := []struct {
		name   string
		from   string
		to     string
		wantID string
	}{
		{name: "a query's text changed", from: `"climate change policy"`, to: `"climate change Policy"`, wantID: "q1"},
		{name: "a query's text gained a trailing space", from: `"central bank interest rates"`, to: `"central bank interest rates "`, wantID: "q2"},
		{name: "a query's text removed", from: "    text: \"climate change policy\"\n", to: "", wantID: "q1"},
		{
			name:   "a query added",
			from:   fixtureQ2,
			to:     fixtureQ2 + "    engines:\n      pg: { template: knn }\n  - id: q3\n    text: \"housing prices\"\n",
			wantID: "q3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			track := copyFixture(t)
			editSuite(t, track, tt.from, tt.to)

			_, err := openTrack(t, track, queryvectors.Options{Model: fixtureTag})

			var stale *queryvectors.StaleQueryError
			if !errors.As(err, &stale) {
				t.Fatalf("err = %v, want a StaleQueryError", err)
			}
			if stale.QueryID != tt.wantID || !strings.Contains(err.Error(), tt.wantID) {
				t.Errorf("err = %q naming query %q, want it to name %s", err, stale.QueryID, tt.wantID)
			}
		})
	}
}

func TestOpen_AcceptsASuiteEditedOnlyOutsideItsQueryTexts(t *testing.T) {
	tests := []struct {
		name string
		from string
		to   string
	}{
		{name: "a suite name added", from: "id: fixture-semantic\n", to: "id: fixture-semantic\nname: Fixture semantic\n"},
		{name: "the template's statement changed", from: "LIMIT 10", to: "LIMIT 100"},
		{name: "a query category added", from: fixtureQ2, to: fixtureQ2 + "    category: keyword\n"},
		{name: "a query description changed", from: `"Policy responses to climate change"`, to: `"How governments answer climate change"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			track := copyFixture(t)
			editSuite(t, track, tt.from, tt.to)

			store, err := openTrack(t, track, queryvectors.Options{Model: fixtureTag})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}

			if _, err := store.QueryVector(context.Background(), "central bank interest rates"); err != nil {
				t.Errorf("QueryVector: %v", err)
			}
		})
	}
}

func TestOpen_RefusesAPackageTaggedOtherThanTheSpecDeclares(t *testing.T) {
	tests := []struct {
		name     string
		declared string
	}{
		{name: "the tag without its width", declared: "qwen3-embedding:0.6b"},
		{name: "another width", declared: "qwen3-embedding:0.6b@512"},
		{name: "nothing declared", declared: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := openTrack(t, copyFixture(t), queryvectors.Options{Model: tt.declared})

			if err == nil || !strings.Contains(err.Error(), fixtureTag) {
				t.Errorf("err = %v, want a refusal naming the package tag %s", err, fixtureTag)
			}
		})
	}
}

func TestOpen_RefusesAnEmbeddingBlockTheVectorsDoNotMatch(t *testing.T) {
	tests := []struct {
		name string
		from string
		to   string
	}{
		{name: "document recipe", from: `"recipe": "query-instruct-v1"`, to: `"recipe": "lead-v1"`},
		{name: "rows wider than the declared dim", from: `"dim": 1024`, to: `"dim": 512`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			track := copyFixture(t)
			descriptor := filepath.Join(track, "trec", "query-vectors", "datapackage.json")
			content, err := os.ReadFile(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(content), tt.from) {
				t.Fatalf("fixture descriptor has no %s", tt.from)
			}
			if err := os.WriteFile(descriptor, []byte(strings.Replace(string(content), tt.from, tt.to, 1)), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err = openTrack(t, track, queryvectors.Options{Model: fixtureTag})

			if err == nil {
				t.Error("Open accepted the package")
			}
		})
	}
}

func TestOpen_RefusesVectorsThatNoLongerMatchTheirHash(t *testing.T) {
	track := copyFixture(t)
	resource := filepath.Join(track, "trec", "query-vectors", "query-vectors.parquet")
	content, err := os.ReadFile(resource)
	if err != nil {
		t.Fatal(err)
	}
	content[len(content)/2] ^= 0xff
	if err := os.WriteFile(resource, content, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = openTrack(t, track, queryvectors.Options{Model: fixtureTag})

	var mismatch *datapackage.HashMismatchError
	if !errors.As(err, &mismatch) {
		t.Errorf("err = %v, want a HashMismatchError", err)
	}
}

func TestDocVectors_ServesThePackageTagsVectorsFromPostgres(t *testing.T) {
	ctx := context.Background()
	container := pkgtesting.NewPGContainerWithCleanup(ctx, t)
	pool, err := pg.NewConnectionPool(ctx, pg.PoolConfig{ConnStr: container.ConnString, RegisterVecTypes: true})
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	t.Cleanup(pool.Close)

	tagged, untagged := uuid.New(), uuid.New()
	taggedVector := make([]float32, 1024)
	taggedVector[3] = 1
	untaggedVector := make([]float32, 1024)
	untaggedVector[5] = 1
	for id, stored := range map[uuid.UUID]struct {
		model  string
		vector []float32
	}{
		tagged:   {model: fixtureTag, vector: taggedVector},
		untagged: {model: "qwen3-embedding:0.6b", vector: untaggedVector},
	} {
		if _, err := pool.GetConn().Exec(ctx,
			`INSERT INTO articles (id, url, title, content, language) VALUES ($1, $2, $3, $4, $5)`,
			id, "https://example.com/"+id.String(), "title", "content", "eng"); err != nil {
			t.Fatalf("insert article: %v", err)
		}
		if _, err := pool.GetConn().Exec(ctx,
			`INSERT INTO article_embeddings (article_id, model_name, embedding) VALUES ($1, $2, $3)`,
			id, stored.model, pgvector.NewVector(stored.vector)); err != nil {
			t.Fatalf("insert embedding: %v", err)
		}
	}
	store, err := openTrack(t, copyFixture(t), queryvectors.Options{Model: fixtureTag, Postgres: pool})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got, err := store.DocVectors(ctx, []uuid.UUID{tagged, untagged})
	if err != nil {
		t.Fatalf("DocVectors: %v", err)
	}

	if _, ok := got[untagged]; ok {
		t.Errorf("DocVectors served %s, stored under the plain model name", untagged)
	}
	if v, ok := got[tagged]; !ok || len(v) != 1024 || v[3] != 1 {
		t.Errorf("DocVectors[%s] = %v (present %v), want its stored e3 vector", tagged, v[:min(len(v), 4)], ok)
	}
}
