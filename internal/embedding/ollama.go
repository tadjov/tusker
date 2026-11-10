package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tadjov/tusker/internal/apperr"
)

type OllamaConfig func(client *OllamaClient)

type OllamaClient struct {
	base url.URL
	http *http.Client
}

const defaultTimeout = 60 * time.Second

func NewOllamaClient(baseUrl string, opts ...OllamaConfig) (*OllamaClient, error) {
	base, err := url.Parse(baseUrl)
	if err != nil {
		return nil, err
	}

	client := &OllamaClient{
		base: *base,
		http: &http.Client{
			Timeout: defaultTimeout,
		},
	}

	for _, cfg := range opts {
		cfg(client)
	}

	return client, nil
}

func WithHttpClient(httpClient *http.Client) OllamaConfig {
	return func(client *OllamaClient) {
		client.http = httpClient
	}
}

type Duration struct {
	time.Duration
}
type OllamaRequest struct {
	// Model is the model name.
	Model string `json:"model"`

	// Prompt is the textual prompt to embed.
	Prompt string `json:"prompt"`

	// KeepAlive controls how long the model will stay loaded in memory following
	// this request.
	KeepAlive *Duration `json:"keep_alive,omitempty"`

	// Options lists model-specific options.
	Options map[string]any `json:"options"`
}

func (oc *OllamaClient) Generate(ctx context.Context, req Request) (*Response, error) {
	if req.Prompt == "" {
		return nil, apperr.ValidationError{Err: fmt.Errorf("missing text to embed")}
	}
	if req.Model == "" {
		return nil, apperr.ValidationError{Err: fmt.Errorf("missing model name")}
	}

	oReq := OllamaRequest{
		Model:   req.Model,
		Prompt:  req.Prompt,
		Options: req.Options,
	}

	var resp Response
	if err := oc.do(ctx, http.MethodPost, "/api/embeddings", oReq, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

func (oc *OllamaClient) GenerateBatch(ctx context.Context, req BatchRequest) (*BatchResponse, error) {
	if len(req.Prompts) == 0 {
		return nil, apperr.ValidationError{Err: fmt.Errorf("missing prompts to embed")}
	}
	if req.Model == "" {
		return nil, apperr.ValidationError{Err: fmt.Errorf("missing model name")}
	}

	oReq := OllamaBatchRequest{
		Model:   req.Model,
		Input:   req.Prompts,
		Options: req.Options,
	}

	var resp BatchResponse
	if err := oc.do(ctx, http.MethodPost, "/api/embed", oReq, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

type OllamaBatchRequest struct {
	Model   string         `json:"model"`
	Input   []string       `json:"input"`
	Options map[string]any `json:"options,omitempty"`
}

func (oc *OllamaClient) do(ctx context.Context, method, path string, reqData, respData any) error {
	reqDataBytes, err := json.Marshal(reqData)
	if err != nil {
		return err
	}

	reqURL := oc.base.JoinPath(path)
	request, err := http.NewRequestWithContext(ctx, method, reqURL.String(), bytes.NewReader(reqDataBytes))
	if err != nil {
		return err
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")

	resp, err := oc.http.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d, body: %s", resp.StatusCode, string(respBody))
	}

	if err := json.Unmarshal(respBody, respData); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}

	return nil
}
