package embedding

import (
	"errors"
	"fmt"
	"os"
	"strconv"
)

type Config struct {
	Enabled bool
	// Model is the Ollama model name, without the @dim tag.
	Model   string
	Dim     int
	BaseURL string
}

func LoadConfigFromEnv() (*Config, error) {
	enabled := os.Getenv("EMBEDDING_ENABLED")
	model := os.Getenv("EMBEDDING_MODEL")
	baseUrl := os.Getenv("EMBEDDING_BASE_URL")

	if enabled == "true" && baseUrl == "" {
		return nil, errors.New("EMBEDDING_BASE_URL environment variable not set")
	}

	// Ignoring the old name would quietly store vectors at DefaultDim.
	if _, set := os.LookupEnv("EMBEDDING_MAX_LENGTH"); set {
		return nil, errors.New("EMBEDDING_MAX_LENGTH is renamed EMBEDDING_DIM")
	}
	dim, err := DimFromEnv()
	if err != nil {
		return nil, err
	}

	return &Config{
		Enabled: enabled == "true",
		Model:   model,
		Dim:     dim,
		BaseURL: baseUrl,
	}, nil
}

// DimFromEnv reads EMBEDDING_DIM, the width vectors are sliced to and tagged with.
func DimFromEnv() (int, error) {
	return parseDim(os.Getenv("EMBEDDING_DIM"))
}

func parseDim(raw string) (int, error) {
	if raw == "" {
		return DefaultDim, nil
	}
	dim, err := strconv.Atoi(raw)
	if err != nil || dim <= 0 {
		return 0, fmt.Errorf("EMBEDDING_DIM must be a positive integer, got %q", raw)
	}
	return dim, nil
}
