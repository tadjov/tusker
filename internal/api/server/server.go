package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	echoSwagger "github.com/swaggo/echo-swagger"
	openapi "github.com/tadjov/tusker/api/openapi-spec"
	mw "github.com/tadjov/tusker/internal/api/middleware"
	"github.com/tadjov/tusker/internal/apperr"
	"github.com/tadjov/tusker/pkg/server"
)

const (
	DefaultGracefulShutdownTimeout = 10 * time.Second
	healthCheckTimeout             = 2 * time.Second
)

type Server struct {
	*echo.Echo

	cfg *Config

	checker server.HealthChecker

	gracefulShutdownTimeout time.Duration
}

func New(cfg *Config, checker server.HealthChecker) (*Server, error) {
	cfg, err := cfg.validated()
	if err != nil {
		return nil, fmt.Errorf("server config: %w", err)
	}
	e := echo.New()

	e.DisableHTTP2 = !cfg.UseHttp2
	e.Server.ReadHeaderTimeout = cfg.ReadHeaderTimeout
	e.Server.ReadTimeout = cfg.ReadTimeout
	e.Server.WriteTimeout = cfg.WriteTimeout
	e.Server.IdleTimeout = cfg.IdleTimeout

	s := &Server{
		Echo:                    e,
		cfg:                     cfg,
		checker:                 checker,
		gracefulShutdownTimeout: DefaultGracefulShutdownTimeout,
	}

	return s, nil
}

// Start serves until SIGINT or SIGTERM, then shuts down gracefully. It returns
// instead of exiting so the caller can release what the handlers used.
func (s *Server) Start() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	served := make(chan error, 1)
	go func() { served <- s.Echo.Start(":" + s.cfg.Port) }()

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.gracefulShutdownTimeout)
	defer cancel()
	if err := s.Echo.Shutdown(shutdownCtx); err != nil {
		// Closing the connections cancels the handlers still running, so their
		// backend calls return and the caller can release the pool.
		return errors.Join(fmt.Errorf("graceful shutdown: %w", err), s.Echo.Close())
	}
	slog.Info("Server shut down gracefully ...")

	return nil
}

func (s *Server) SetupMiddlewares() *Server {
	s.Echo.Use(middleware.RequestID())
	s.Echo.Use(mw.Logger())
	s.Echo.Use(middleware.Recover())
	s.Echo.Use(middleware.CORSWithConfig(middleware.CORSConfig{
		AllowOrigins: s.cfg.CorsOrigins,
		AllowMethods: []string{http.MethodGet, http.MethodPut, http.MethodPost, http.MethodDelete},
	}))
	s.Echo.Use(middleware.BodyLimit(strconv.FormatInt(s.cfg.BodyLimit, 10)))
	s.Echo.Use(middleware.ContextTimeout(s.cfg.RequestTimeout))

	return s
}

func (s *Server) SetupHealthChecks(path string) *Server {
	s.Echo.GET(path, s.handleHealthCheck)

	return s
}

func (s *Server) handleHealthCheck(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), healthCheckTimeout)
	defer cancel()
	if !s.checker.Healthy(ctx) {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "unhealthy"})
	}

	return c.JSON(http.StatusOK, map[string]string{"status": "healthy"})
}

func (s *Server) SetupErrorHandler() *Server {
	s.Echo.HTTPErrorHandler = apperr.GlobalErrorHandler()

	return s
}

func (s *Server) SetupOpenApi(path string) *Server {
	openapi.SwaggerInfo.Host = fmt.Sprintf("localhost:%s", s.cfg.Port)

	s.Echo.GET(path, echoSwagger.WrapHandler)

	return s
}
