package main

import (
	"context"
	"testing"

	"github.com/tadjov/tusker/internal/embedding"
)

type recordingClient struct {
	models []string
}

func (c *recordingClient) Generate(_ context.Context, req embedding.Request) (*embedding.Response, error) {
	c.models = append(c.models, req.Model)
	unit := make([]float32, 1024)
	unit[0] = 1
	return &embedding.Response{Embedding: unit}, nil
}

func TestNewQueryEmbedder_AsksForThePlainModelAndTagsTheWidth(t *testing.T) {
	tests := []struct {
		name        string
		cfg         embedding.Config
		wantRequest string
		wantTag     string
		wantWidth   int
	}{
		{
			name:        "configured model and width",
			cfg:         embedding.Config{Model: "bge-m3", Dim: 512},
			wantRequest: "bge-m3",
			wantTag:     "bge-m3@512",
			wantWidth:   512,
		},
		{
			name:        "nothing configured",
			cfg:         embedding.Config{},
			wantRequest: "qwen3-embedding:0.6b",
			wantTag:     "qwen3-embedding:0.6b@1024",
			wantWidth:   1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &recordingClient{}

			vec, err := newQueryEmbedder(client, tt.cfg).EmbedQuery(context.Background(), "climate")
			if err != nil {
				t.Fatalf("EmbedQuery: %v", err)
			}

			if len(client.models) != 1 || client.models[0] != tt.wantRequest {
				t.Errorf("Ollama asked for %v, want %q", client.models, tt.wantRequest)
			}
			if vec.Model != tt.wantTag {
				t.Errorf("query vector tagged %q, want %q", vec.Model, tt.wantTag)
			}
			if len(vec.Embedding) != tt.wantWidth {
				t.Errorf("query vector width = %d, want %d", len(vec.Embedding), tt.wantWidth)
			}
		})
	}
}
