package main

import (
	"log/slog"
	"os"

	"github.com/tadjov/tusker/internal/embedding"
	"github.com/tadjov/tusker/internal/storage/factory"
	"github.com/tadjov/tusker/pkg/config/env"
)

type AppConfig struct {
	ENV string
}

func NewAppConfig() *AppConfig {
	return &AppConfig{
		ENV: os.Getenv("ENV"),
	}
}

type NewsSearchConfig struct {
	StorageConfig   factory.StorageConfig
	EmbeddingConfig embedding.Config
}

func (as *AppConfig) Load() (*NewsSearchConfig, error) {

	err := env.LoadDotEnv(as.ENV, "cmd/news_api/.env")
	if err != nil {
		slog.Info("Failed to .env load environment variables, continuing with existing environment variables", "error", err)
	}

	storageCfg, err := factory.LoadEnv()
	if err != nil {
		slog.Error("Failed to load storage configuration from environment", "error", err)
		return nil, err
	}

	embed, err := embedding.LoadConfigFromEnv()
	if err != nil {
		slog.Error("Failed to load embedding configuration from environment", "error", err)
		return nil, err
	}

	return &NewsSearchConfig{
		StorageConfig:   *storageCfg,
		EmbeddingConfig: *embed,
	}, nil
}
