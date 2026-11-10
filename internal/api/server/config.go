package server

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/gommon/bytes"
	"github.com/tadjov/tusker/pkg/config/env"
	"github.com/tadjov/tusker/pkg/utils"
)

const (
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 30 * time.Second
	// DefaultRequestTimeout outlasts the slowest search: a 60s query embedding
	// followed by an Elasticsearch call bounded at 30s.
	DefaultRequestTimeout = 90 * time.Second
	DefaultWriteTimeout   = 120 * time.Second
	DefaultIdleTimeout    = 120 * time.Second
	// DefaultBodyLimit is 1M as HTTP_BODY_LIMIT reads it: 1,048,576 bytes.
	DefaultBodyLimit = 1 << 20
)

type Config struct {
	Port        string
	UseHttp2    bool
	CorsOrigins []string

	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	// RequestTimeout cancels the handler's context, which WriteTimeout does not,
	// so it must be shorter than WriteTimeout.
	RequestTimeout time.Duration
	// BodyLimit is the largest request body in bytes; larger bodies get 413.
	BodyLimit int64
}

func LoadConfig() (*Config, error) {
	err := env.LoadDotEnv("cmd/news_api/.env")
	if err != nil {
		slog.Info("Skipping .env ...", "error", err)
	}

	useHttp2Str := os.Getenv("USE_HTTP2")
	useHttp2 := useHttp2Str == "true"

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if err := validatePort(port); err != nil {
		return nil, fmt.Errorf("invalid port: %w", err)
	}

	var origins []string
	corsOriginsEnv := os.Getenv("CORS_ORIGINS")
	if corsOriginsEnv != "" {
		origins = strings.Split(corsOriginsEnv, ",")
		for i, origin := range origins {
			origins[i] = strings.TrimSpace(origin)
		}
		origins = utils.RemoveEmptyStrings(origins)
	}

	if len(origins) == 0 {
		origins = []string{"*"}
	}

	cfg := &Config{
		Port:        port,
		UseHttp2:    useHttp2,
		CorsOrigins: origins,
	}

	timeouts := map[string]*time.Duration{
		"HTTP_READ_HEADER_TIMEOUT": &cfg.ReadHeaderTimeout,
		"HTTP_READ_TIMEOUT":        &cfg.ReadTimeout,
		"HTTP_WRITE_TIMEOUT":       &cfg.WriteTimeout,
		"HTTP_IDLE_TIMEOUT":        &cfg.IdleTimeout,
		"HTTP_REQUEST_TIMEOUT":     &cfg.RequestTimeout,
	}
	for name, target := range timeouts {
		if *target, err = env.PositiveDuration(name); err != nil {
			return nil, err
		}
	}

	if cfg.BodyLimit, err = byteSizeFromEnv("HTTP_BODY_LIMIT"); err != nil {
		return nil, err
	}

	return cfg.validated()
}

// validated returns a copy of cfg with every unset bound defaulted, so no
// Config leaves the server unbounded, or an error if a bound is invalid.
func (cfg Config) validated() (*Config, error) {
	defaults := []struct {
		name     string
		value    *time.Duration
		fallback time.Duration
	}{
		{name: "read header timeout", value: &cfg.ReadHeaderTimeout, fallback: DefaultReadHeaderTimeout},
		{name: "read timeout", value: &cfg.ReadTimeout, fallback: DefaultReadTimeout},
		{name: "write timeout", value: &cfg.WriteTimeout, fallback: DefaultWriteTimeout},
		{name: "idle timeout", value: &cfg.IdleTimeout, fallback: DefaultIdleTimeout},
		{name: "request timeout", value: &cfg.RequestTimeout, fallback: DefaultRequestTimeout},
	}
	for _, d := range defaults {
		if *d.value < 0 {
			return nil, fmt.Errorf("invalid %s %v: must be positive", d.name, *d.value)
		}
		if *d.value == 0 {
			*d.value = d.fallback
		}
	}
	if cfg.RequestTimeout >= cfg.WriteTimeout {
		return nil, fmt.Errorf("request timeout %v (HTTP_REQUEST_TIMEOUT) must be shorter than write timeout %v (HTTP_WRITE_TIMEOUT)",
			cfg.RequestTimeout, cfg.WriteTimeout)
	}
	if cfg.BodyLimit < 0 {
		return nil, fmt.Errorf("invalid body limit %d: must be positive", cfg.BodyLimit)
	}
	if cfg.BodyLimit == 0 {
		cfg.BodyLimit = DefaultBodyLimit
	}
	return &cfg, nil
}

func byteSizeFromEnv(name string) (int64, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return 0, nil
	}
	size, err := bytes.Parse(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", name, raw, err)
	}
	if size <= 0 {
		return 0, fmt.Errorf("invalid %s %q: must be positive", name, raw)
	}
	return size, nil
}

func validatePort(port string) error {
	portNum, err := strconv.Atoi(port)

	if err != nil {
		return errors.New("port must be a number")
	}

	if portNum < 1 || portNum > 65535 {
		return errors.New("port must be between 1 and 65535")
	}

	return nil
}
