package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	pkgserver "github.com/tadjov/tusker/pkg/server"
)

func newServer(t *testing.T, cfg *Config, checker pkgserver.HealthChecker) *Server {
	t.Helper()
	s, err := New(cfg, checker)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func startOnLoopback(t *testing.T, s *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.Echo.Listener = ln
	s.Echo.HideBanner = true
	s.Echo.HidePort = true
	go func() { _ = s.Echo.Start("") }()
	t.Cleanup(func() { _ = s.Echo.Close() })
	return ln.Addr().String()
}

func TestServer_DropsConnectionThatNeverFinishesHeaders(t *testing.T) {
	s := newServer(t, &Config{Port: "0", ReadHeaderTimeout: 50 * time.Millisecond}, pkgserver.NewOkHealthChecker()).
		SetupHealthChecks("/health")
	addr := startOnLoopback(t, s)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	if _, err := io.WriteString(conn, "GET /health HTTP/1.1\r\nHost: localhost\r\n"); err != nil {
		t.Fatalf("write partial header: %v", err)
	}

	// The deadline only stops a hanging server from hanging the test.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	_, err = conn.Read(make([]byte, 1))

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatal("server kept a connection with a partial header open")
	}
	if err == nil {
		t.Fatal("server answered a request whose headers never finished")
	}
}

func TestServer_RejectsBodyOverLimit(t *testing.T) {
	tests := []struct {
		name     string
		bodySize int
		wantCode int
	}{
		{name: "under the limit", bodySize: 512, wantCode: http.StatusOK},
		{name: "over the limit", bodySize: 4096, wantCode: http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t, &Config{Port: "0", BodyLimit: 1000}, pkgserver.NewOkHealthChecker()).
				SetupMiddlewares().
				SetupErrorHandler()
			s.Echo.POST("/echo", func(c echo.Context) error {
				body, err := io.ReadAll(c.Request().Body)
				if err != nil {
					return err
				}
				return c.String(http.StatusOK, string(body))
			})

			req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader(strings.Repeat("a", tt.bodySize)))
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

func TestLoadConfig_ServerBounds(t *testing.T) {
	t.Run("env overrides", func(t *testing.T) {
		t.Setenv("HTTP_READ_HEADER_TIMEOUT", "2s")
		t.Setenv("HTTP_WRITE_TIMEOUT", "3m")
		t.Setenv("HTTP_REQUEST_TIMEOUT", "150s")
		t.Setenv("HTTP_BODY_LIMIT", "65536")

		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.ReadHeaderTimeout != 2*time.Second {
			t.Errorf("ReadHeaderTimeout = %v, want 2s", cfg.ReadHeaderTimeout)
		}
		if cfg.WriteTimeout != 3*time.Minute {
			t.Errorf("WriteTimeout = %v, want 3m", cfg.WriteTimeout)
		}
		if cfg.RequestTimeout != 150*time.Second {
			t.Errorf("RequestTimeout = %v, want 150s", cfg.RequestTimeout)
		}
		if cfg.BodyLimit != 65536 {
			t.Errorf("BodyLimit = %d, want 65536", cfg.BodyLimit)
		}
	})

	t.Run("unset leaves no timeout unbounded", func(t *testing.T) {
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		for name, d := range map[string]time.Duration{
			"ReadHeaderTimeout": cfg.ReadHeaderTimeout,
			"ReadTimeout":       cfg.ReadTimeout,
			"WriteTimeout":      cfg.WriteTimeout,
			"IdleTimeout":       cfg.IdleTimeout,
			"RequestTimeout":    cfg.RequestTimeout,
		} {
			if d <= 0 {
				t.Errorf("%s = %v, want a positive default", name, d)
			}
		}
		if cfg.BodyLimit <= 0 {
			t.Errorf("BodyLimit = %d, want a positive default", cfg.BodyLimit)
		}
	})

	invalid := []struct {
		name  string
		env   string
		value string
	}{
		{name: "unparseable timeout", env: "HTTP_READ_TIMEOUT", value: "soon"},
		{name: "zero timeout", env: "HTTP_IDLE_TIMEOUT", value: "0s"},
		{name: "unparseable body limit", env: "HTTP_BODY_LIMIT", value: "lots"},
		{name: "zero request timeout", env: "HTTP_REQUEST_TIMEOUT", value: "0s"},
		{name: "request timeout past the write timeout", env: "HTTP_REQUEST_TIMEOUT", value: "10m"},
		{name: "zero body limit", env: "HTTP_BODY_LIMIT", value: "0"},
		{name: "negative body limit", env: "HTTP_BODY_LIMIT", value: "-1M"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.env, tt.value)
			if _, err := LoadConfig(); err == nil {
				t.Errorf("LoadConfig with %s=%q succeeded, want an error", tt.env, tt.value)
			}
		})
	}
}

func TestNew_RejectsInvalidBounds(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "negative body limit", cfg: Config{Port: "0", BodyLimit: -1}},
		{name: "negative request timeout", cfg: Config{Port: "0", RequestTimeout: -time.Second}},
		{name: "request timeout equal to the write timeout", cfg: Config{Port: "0", RequestTimeout: time.Minute, WriteTimeout: time.Minute}},
		{name: "write timeout below the default request timeout", cfg: Config{Port: "0", WriteTimeout: time.Minute}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(&tt.cfg, pkgserver.NewOkHealthChecker()); err == nil {
				t.Errorf("New(%+v) succeeded, want an error", tt.cfg)
			}
		})
	}
}

type blockingHealthChecker struct{}

func (blockingHealthChecker) Healthy(ctx context.Context) bool {
	<-ctx.Done()
	return false
}

func TestServer_HealthAnswersWhenBackendHangs(t *testing.T) {
	s := newServer(t, &Config{Port: "0"}, blockingHealthChecker{}).SetupHealthChecks("/health")

	rec := httptest.NewRecorder()
	answered := make(chan struct{})
	go func() {
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		close(answered)
	}()

	// The guard only stops an unbounded health check from hanging the test.
	select {
	case <-answered:
	case <-time.After(30 * time.Second):
		t.Fatal("/health never answered while the backend hung")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/health = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// serve runs s.Start on a loopback listener and returns once the server answers,
// which also means Start has registered its signal handlers.
func serve(t *testing.T, s *Server) (baseURL string, stopped <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.Echo.Listener = ln
	s.Echo.HideBanner = true
	s.Echo.HidePort = true
	s.SetupHealthChecks("/health")

	errs := make(chan error, 1)
	go func() { errs <- s.Start() }()
	t.Cleanup(func() { _ = s.Echo.Close() })

	baseURL = "http://" + ln.Addr().String()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(baseURL + "/health")
	if err != nil {
		t.Fatalf("server never answered: %v", err)
	}
	_ = resp.Body.Close()
	return baseURL, errs
}

func waitForStart(t *testing.T, stopped <-chan error) error {
	t.Helper()
	// The guard only stops a Start that never returns from hanging the test.
	select {
	case err := <-stopped:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Start did not return after the shutdown signal")
		return nil
	}
}

func TestServer_StartReturnsOnShutdownSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			s := newServer(t, &Config{Port: "0"}, pkgserver.NewOkHealthChecker())
			_, stopped := serve(t, s)

			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Fatalf("send %v: %v", sig, err)
			}

			if err := waitForStart(t, stopped); err != nil {
				t.Errorf("Start after %v = %v, want nil", sig, err)
			}
		})
	}
}

func TestServer_StartReturnsErrorWhenRequestOutlivesGracePeriod(t *testing.T) {
	s := newServer(t, &Config{Port: "0"}, pkgserver.NewOkHealthChecker())
	s.gracefulShutdownTimeout = 50 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	s.Echo.GET("/slow", func(c echo.Context) error {
		close(entered)
		<-release
		return c.NoContent(http.StatusOK)
	})
	baseURL, stopped := serve(t, s)

	go func() {
		if resp, err := http.Get(baseURL + "/slow"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	if err := waitForStart(t, stopped); err == nil {
		t.Error("Start = nil after a request outlived the grace period, want an error")
	}
}

func TestServer_StartCancelsRequestsThatOutliveTheGracePeriod(t *testing.T) {
	s := newServer(t, &Config{Port: "0"}, pkgserver.NewOkHealthChecker())
	s.gracefulShutdownTimeout = 50 * time.Millisecond
	entered, cancelled := make(chan struct{}), make(chan struct{})
	s.Echo.GET("/slow", func(c echo.Context) error {
		close(entered)
		<-c.Request().Context().Done()
		close(cancelled)
		return nil
	})
	baseURL, stopped := serve(t, s)

	go func() {
		if resp, err := http.Get(baseURL + "/slow"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}
	if err := waitForStart(t, stopped); err == nil {
		t.Error("Start = nil after a request outlived the grace period, want an error")
	}

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Error("a request still running after the grace period kept its context, so the backend pool cannot close until it finishes")
	}
}

func TestNew_RejectsNegativeBounds(t *testing.T) {
	tests := map[string]Config{
		"read header timeout": {Port: "0", ReadHeaderTimeout: -time.Second},
		"read timeout":        {Port: "0", ReadTimeout: -time.Second},
		"write timeout":       {Port: "0", WriteTimeout: -time.Second},
		"idle timeout":        {Port: "0", IdleTimeout: -time.Second},
		"request timeout":     {Port: "0", RequestTimeout: -time.Second},
		"body limit":          {Port: "0", BodyLimit: -1},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(&cfg, pkgserver.NewOkHealthChecker()); err == nil {
				t.Errorf("New accepted a negative %s, which net/http reads as no bound", name)
			}
		})
	}
}

func TestServer_StartReturnsErrorWhenPortIsTaken(t *testing.T) {
	taken, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	_, port, err := net.SplitHostPort(taken.Addr().String())
	if err != nil {
		t.Fatalf("split address: %v", err)
	}
	s := newServer(t, &Config{Port: port}, pkgserver.NewOkHealthChecker())
	s.Echo.HideBanner = true

	stopped := make(chan error, 1)
	go func() { stopped <- s.Start() }()

	if err := waitForStart(t, stopped); err == nil {
		t.Error("Start = nil on a taken port, want an error")
	}
}
