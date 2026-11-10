package testing

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tadjov/tusker/db/migrations"
	"github.com/tadjov/tusker/internal/storage"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

type PGContainer struct {
	Container  testcontainers.Container
	ConnString string
}

// PGConfig defaults Engine to storage.PostgresNative and migrates unless Unmigrated is set.
type PGConfig struct {
	Database   string
	Username   string
	Password   string
	Engine     storage.Engine
	Unmigrated bool
}

var engineImages = map[storage.Engine]string{
	storage.PostgresNative: "pgvector/pgvector:pg18",
	storage.ParadeDB:       "paradedb/paradedb:0.21.5-pg18",
	storage.Tiger:          "timescale/timescaledb-ha:pg18",
}

var indexBuildFlags = []string{"-c", "maintenance_work_mem=4GB", "-c", "max_parallel_maintenance_workers=4"}

// engineServerFlags mirrors the server-start settings in docker-compose.yml.
var engineServerFlags = map[storage.Engine][]string{
	storage.PostgresNative: indexBuildFlags,
	storage.ParadeDB:       indexBuildFlags,
	storage.Tiger:          append(slices.Clone(indexBuildFlags), "-c", "pg_textsearch.memory_limit=2GB"),
}

func NewPGContainer(ctx context.Context, cfg PGConfig) (*PGContainer, error) {
	return createPGContainer(ctx, cfg)
}

func NewPGContainerWithCleanup(ctx context.Context, tb testing.TB) *PGContainer {
	tb.Helper()
	return NewPGContainerWithConfig(ctx, tb, PGConfig{})
}

// NewPGContainerWithConfig defaults an empty Database to news_test_db as test/test.
func NewPGContainerWithConfig(ctx context.Context, tb testing.TB, cfg PGConfig) *PGContainer {
	tb.Helper()
	if testing.Short() {
		tb.Skip("skipping testcontainer-backed test in -short mode")
	}
	if cfg.Database == "" {
		cfg.Database, cfg.Username, cfg.Password = "news_test_db", "test", "test"
	}

	container, err := createPGContainer(ctx, cfg)
	if err != nil {
		tb.Fatalf("failed to create postgres container: %v", err)
	}

	tb.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container.Container); err != nil {
			tb.Logf("failed to terminate postgres container: %v", err)
		}
	})

	return container
}

func createPGContainer(ctx context.Context, cfg PGConfig) (*PGContainer, error) {
	if cfg.Engine == "" {
		cfg.Engine = storage.PostgresNative
	}
	image, ok := engineImages[cfg.Engine]
	if !ok {
		return nil, fmt.Errorf("no postgres image for engine %q", cfg.Engine)
	}

	pgContainer, err := postgres.Run(ctx,
		image,
		postgres.WithDatabase(cfg.Database),
		postgres.WithUsername(cfg.Username),
		postgres.WithPassword(cfg.Password),
		testcontainers.WithCmdArgs(engineServerFlags[cfg.Engine]...),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(2*time.Minute),
		),
	)
	if err != nil {
		_ = testcontainers.TerminateContainer(pgContainer)
		return nil, fmt.Errorf("failed to start %s container: %w", cfg.Engine, err)
	}

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err == nil && !cfg.Unmigrated {
		err = migrate(ctx, connStr, cfg.Engine)
	}
	if err != nil {
		_ = testcontainers.TerminateContainer(pgContainer)
		return nil, err
	}

	return &PGContainer{
		Container:  pgContainer,
		ConnString: connStr,
	}, nil
}

func migrate(ctx context.Context, connStr string, engine storage.Engine) error {
	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		return fmt.Errorf("connect to migrate %s: %w", engine, err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	return migrations.Build(ctx, conn, engine)
}
