package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeOllamaEmbed answers /api/embed the way Ollama does: one vector per text in
// "input", and an empty list when the request carries its texts under any other key.
func fakeOllamaEmbed(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		embeddings := make([][]float32, 0, len(body.Input))
		for _, text := range body.Input {
			embeddings = append(embeddings, []float32{float32(len(text))})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestOllamaClient_GenerateBatch_OneEmbeddingPerPromptInOrder(t *testing.T) {
	server := fakeOllamaEmbed(t)
	client, err := NewOllamaClient(server.URL)
	if err != nil {
		t.Fatalf("NewOllamaClient: %v", err)
	}

	resp, err := client.GenerateBatch(context.Background(), BatchRequest{
		Model:   DefaultModel,
		Prompts: []string{"a", "abc"},
	})
	if err != nil {
		t.Fatalf("GenerateBatch: %v", err)
	}

	if len(resp.Embeddings) != 2 {
		t.Fatalf("got %d embeddings, want one per prompt", len(resp.Embeddings))
	}
	if resp.Embeddings[0][0] != 1 || resp.Embeddings[1][0] != 3 {
		t.Errorf("embeddings = %v, want them aligned with the prompts", resp.Embeddings)
	}
}
