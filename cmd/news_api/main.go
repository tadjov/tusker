// Package main Tusker API
// @title Tusker API
// @version 1.0
// @description A full-text and semantic search engine for exploring multilingual news headlines and articles
// @termsOfService http://swagger.io/terms/
// @contact.name API Support
// @contact.email support@tusker.sh
// @license.name Apache 2.0
// @license.url https://opensource.org/licenses/Apache-2.0
// @BasePath /
package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/labstack/echo/v4"
	"github.com/tadjov/tusker/internal/api/router"
	server2 "github.com/tadjov/tusker/internal/api/server"
	"github.com/tadjov/tusker/internal/embedding"
)

func main() {
	slog.SetLogLoggerLevel(slog.LevelDebug)

	sCfg, err := server2.LoadConfig()
	if err != nil {
		slog.Error("Failed to load config", "error", err)
		os.Exit(1)
	}

	appSettings := NewAppConfig()
	cfg, err := appSettings.Load()
	if err != nil {
		slog.Error("Failed to load app configuration", "error", err)
		os.Exit(1)
	}

	backend, err := openSearchBackend(context.Background(), cfg)
	if err != nil {
		slog.Error("Failed to open search backend", "error", err)
		os.Exit(1)
	}

	s, err := server2.New(sCfg, backend.health)
	if err != nil {
		backend.close()
		slog.Error("Failed to create server", "error", err)
		os.Exit(1)
	}
	s.SetupMiddlewares().
		SetupValidator().
		SetupErrorHandler().
		SetupHealthChecks("/health").
		SetupOpenApi("/swagger/*")

	s.Echo.GET("/", func(c echo.Context) error {
		return c.String(200, "Tusker API is running")
	})

	var routerOpts []router.SearchRouterOption
	if backend.semantic != nil {
		routerOpts = append(routerOpts, router.WithSemanticSearcher(backend.semantic))
		slog.Info("Semantic search enabled")
	} else {
		slog.Info("Semantic search disabled")
	}
	if backend.hybrid != nil {
		routerOpts = append(routerOpts, router.WithHybridSearcher(backend.hybrid))
		slog.Info("Hybrid search enabled")
	}

	router.NewSearchRouter(s.Echo, backend.fts, routerOpts...).Bind()

	err = s.Start()
	backend.close()
	slog.Info("Search backend closed")
	if err != nil {
		slog.Error("Server stopped with an error", "error", err)
		os.Exit(1)
	}
}

// newQueryEmbedder must use the model and width the corpus was loaded with.
func newQueryEmbedder(client embedding.Client, cfg embedding.Config) *embedding.Embedder {
	var opts []embedding.EmbedderOption
	if cfg.Model != "" {
		opts = append(opts, embedding.WithExecutorModel(cfg.Model))
	}
	if cfg.Dim != 0 {
		opts = append(opts, embedding.WithDim(cfg.Dim))
	}
	return embedding.NewEmbedder(client, opts...)
}
