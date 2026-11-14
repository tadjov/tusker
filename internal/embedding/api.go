package embedding

import (
	"context"
)

// DefaultModel is the Ollama model used when none is configured.
const DefaultModel = "qwen3-embedding:0.6b"

type Request struct {
	Model string `json:"model"`

	// Prompt is the textual prompt to embed.
	Prompt string `json:"prompt"`

	// Options lists model-specific options.
	Options map[string]any `json:"options"`
}

type Response struct {
	Embedding []float32 `json:"embedding"`
}

type BatchRequest struct {
	Model   string         `json:"model"`
	Prompts []string       `json:"prompts"`
	Options map[string]any `json:"options,omitempty"`
}

type BatchResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

type Client interface {
	Generate(ctx context.Context, req Request) (*Response, error)
	GenerateBatch(ctx context.Context, req BatchRequest) (*BatchResponse, error)
}
