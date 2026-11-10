package pg

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/DjordjeVuckovic/tusker/db/migrations"
	"github.com/DjordjeVuckovic/tusker/internal/embedding"
	"github.com/DjordjeVuckovic/tusker/internal/storage"
	"github.com/DjordjeVuckovic/tusker/internal/types/document"
	"github.com/DjordjeVuckovic/tusker/internal/types/language"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pgvector/pgvector-go"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

var _ storage.CorpusLoader = (*CorpusLoader)(nil)

// CorpusLoader migrates the schema, copies the corpus, then times each index build.
type CorpusLoader struct {
	pool    *ConnectionPool
	engine  storage.Engine
	schema  migrations.Set
	indexes []migrations.IndexStatement
}

// NewCorpusLoader needs a pool that skips vector types; the schema migrations create them.
func NewCorpusLoader(pool *ConnectionPool, engine storage.Engine) (*CorpusLoader, error) {
	schema, err := migrations.Schema(engine)
	if err != nil {
		return nil, err
	}
	indexes, err := migrations.SearchIndexes(engine)
	if err != nil {
		return nil, err
	}
	return &CorpusLoader{pool: pool, engine: engine, schema: schema, indexes: indexes}, nil
}

// LoadCorpus refuses a database that already holds a corpus or its search indexes.
func (l *CorpusLoader) LoadCorpus(ctx context.Context, c storage.Corpus, opts storage.LoadOptions) (storage.LoadRecord, error) {
	if err := validateCorpus(c, opts); err != nil {
		return storage.LoadRecord{}, err
	}
	record := storage.LoadRecord{
		Engine:     string(l.engine),
		StartedAt:  now(),
		Canonical:  c.Canonical,
		Embeddings: c.Embeddings,
		ModelTag:   c.ModelTag,
		BatchSize:  opts.BatchSize,
		Stages:     []storage.StageTiming{},
		Settings:   map[string]string{},
	}

	// One session, so index settings read back from it and never reach the pool.
	pooled, err := l.pool.conn.Acquire(ctx)
	if err != nil {
		return storage.LoadRecord{}, fmt.Errorf("acquire connection: %w", err)
	}
	conn := pooled.Hijack()
	defer conn.Close(context.WithoutCancel(ctx))

	if err := lockLoad(ctx, conn); err != nil {
		return storage.LoadRecord{}, err
	}
	if err := timed(&record, "schema", func() error { return l.schema.Up(ctx, conn) }); err != nil {
		return storage.LoadRecord{}, err
	}
	if err := l.refuseLoaded(ctx, conn); err != nil {
		return storage.LoadRecord{}, err
	}
	if err := pgxvec.RegisterTypes(ctx, conn); err != nil {
		return storage.LoadRecord{}, fmt.Errorf("register vector types: %w", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return storage.LoadRecord{}, fmt.Errorf("begin load: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var ids []uuid.UUID
	err = timed(&record, "copy_articles", func() error {
		ids, err = copyArticles(ctx, tx, c, opts.BatchSize)
		return err
	})
	if err != nil {
		return storage.LoadRecord{}, err
	}
	if len(ids) == 0 {
		return storage.LoadRecord{}, errors.New("corpus has no articles")
	}
	record.Articles = int64(len(ids))

	if c.Vectors != nil {
		err = timed(&record, "copy_embeddings", func() error {
			record.Vectors, err = copyVectors(ctx, tx, c, ids, opts.BatchSize)
			return err
		})
		if err != nil {
			return storage.LoadRecord{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return storage.LoadRecord{}, fmt.Errorf("commit load: %w", err)
	}

	if !opts.SkipIndex {
		if err := l.buildIndexes(ctx, conn, &record); err != nil {
			return storage.LoadRecord{}, err
		}
	}
	err = timed(&record, "vacuum_analyze", func() error {
		defer storage.WatchStage(ctx, []any{"engine", record.Engine, "stage", "vacuum_analyze"}, nil)()
		_, err := conn.Exec(ctx, "VACUUM (ANALYZE) articles, article_embeddings")
		return err
	})
	if err != nil {
		return storage.LoadRecord{}, fmt.Errorf("vacuum analyze: %w", err)
	}
	if inspect := engineKnobs[l.engine].inspect; inspect != nil && !opts.SkipIndex {
		if err := inspect(ctx, conn, record.Settings); err != nil {
			return storage.LoadRecord{}, err
		}
	}
	if record.EngineVersion, err = engineVersion(ctx, conn); err != nil {
		return storage.LoadRecord{}, err
	}

	record.FinishedAt = now()
	// An unindexed corpus is not what a bench run measures, so corpus_load skips it.
	if opts.SkipIndex {
		return record, nil
	}
	if _, err := conn.Exec(ctx, "INSERT INTO corpus_load (finished_at, record) VALUES ($1, $2)", record.FinishedAt, record); err != nil {
		return storage.LoadRecord{}, fmt.Errorf("record the load in corpus_load: %w", err)
	}
	return record, nil
}

func engineVersion(ctx context.Context, conn *pgx.Conn) (string, error) {
	var version strings.Builder
	var server string
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version')").Scan(&server); err != nil {
		return "", fmt.Errorf("read server version: %w", err)
	}
	version.WriteString("PostgreSQL " + server)
	extensions, err := readSearchExtensions(ctx, conn)
	if err != nil {
		return "", err
	}
	for _, e := range extensions {
		fmt.Fprintf(&version, ", %s %s", e.Name, e.Version)
	}
	return version.String(), nil
}

func validateCorpus(c storage.Corpus, opts storage.LoadOptions) error {
	switch {
	case c.Language != language.English:
		return fmt.Errorf("corpus language %q: the Postgres schemas analyse only %q", c.Language, language.English)
	case c.Articles == nil:
		return errors.New("corpus has no articles")
	case (c.Vectors == nil) != (c.Embeddings == nil):
		return errors.New("corpus vectors and their embeddings package must come together")
	case c.Vectors != nil && c.ModelTag == "":
		return errors.New("corpus vectors carry no model tag")
	case opts.BatchSize <= 0:
		return fmt.Errorf("batch size %d: want at least 1", opts.BatchSize)
	}
	return nil
}

const loadLockKey = 0x7475736b6572 // "tusker"

func lockLoad(ctx context.Context, conn *pgx.Conn) error {
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", int64(loadLockKey)).Scan(&locked); err != nil {
		return fmt.Errorf("lock the database for loading: %w", err)
	}
	if !locked {
		return errors.New("another corpus load is running on this database")
	}
	return nil
}

func (l *CorpusLoader) refuseLoaded(ctx context.Context, conn *pgx.Conn) error {
	var names []string
	for _, s := range l.indexes {
		if s.Index != "" {
			names = append(names, s.Index)
		}
	}
	var built []string
	err := conn.QueryRow(ctx, `SELECT coalesce(array_agg(indexname ORDER BY indexname), '{}') FROM pg_indexes
		WHERE schemaname = current_schema() AND indexname = ANY($1)`, names).Scan(&built)
	if err != nil {
		return fmt.Errorf("check for existing search indexes: %w", err)
	}
	if len(built) > 0 {
		return fmt.Errorf("search indexes %s already exist; drop the schema before loading", strings.Join(built, ", "))
	}
	var loaded bool
	err = conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM articles) OR EXISTS (SELECT 1 FROM corpus_load)").Scan(&loaded)
	if err != nil {
		return fmt.Errorf("check for an existing corpus: %w", err)
	}
	if loaded {
		return errors.New("the database already holds a corpus; drop its schema before loading another")
	}
	return nil
}

var indexBuildSettings = []string{"maintenance_work_mem", "max_parallel_maintenance_workers"}

// buildIndexes times each search index as its own stage; a SET runs untimed.
func (l *CorpusLoader) buildIndexes(ctx context.Context, conn *pgx.Conn, record *storage.LoadRecord) error {
	for _, s := range l.indexes {
		run := func() error {
			if _, err := conn.Exec(ctx, s.SQL); err != nil {
				return fmt.Errorf("build search index %s: %w", s, err)
			}
			return nil
		}
		if s.Index == "" {
			if err := run(); err != nil {
				return err
			}
			continue
		}
		err := timed(record, s.Index, func() error {
			defer storage.WatchStage(ctx, []any{"engine", record.Engine, "stage", s.Index}, l.indexBuildProgress(conn.PgConn().PID()))()
			return run()
		})
		if err != nil {
			return err
		}
	}
	for _, name := range slices.Concat(indexBuildSettings, engineKnobs[l.engine].settings) {
		var value string
		if err := conn.QueryRow(ctx, "SELECT current_setting($1)", name).Scan(&value); err != nil {
			return fmt.Errorf("read setting %s: %w", name, err)
		}
		record.Settings[name] = value
	}
	return nil
}

// indexBuildProgress reads how far the session's CREATE INDEX has got, or nothing when the index type reports none.
func (l *CorpusLoader) indexBuildProgress(pid uint32) func(context.Context) []any {
	return func(ctx context.Context) []any {
		var phase string
		var blocksDone, blocksTotal, tuplesDone, tuplesTotal int64
		err := l.pool.conn.QueryRow(ctx, `SELECT phase, blocks_done, blocks_total, tuples_done, tuples_total
			FROM pg_stat_progress_create_index WHERE pid = $1`, pid).Scan(&phase, &blocksDone, &blocksTotal, &tuplesDone, &tuplesTotal)
		if err != nil {
			return nil
		}
		attrs := []any{"phase", phase}
		if blocksTotal > 0 {
			attrs = append(attrs, "blocks", fmt.Sprintf("%d/%d", blocksDone, blocksTotal))
		}
		if tuplesTotal > 0 {
			attrs = append(attrs, "tuples", fmt.Sprintf("%d/%d", tuplesDone, tuplesTotal))
		}
		return attrs
	}
}

// now truncates to timestamptz's microseconds, so records round-trip through corpus_load.
func now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

func timed(record *storage.LoadRecord, stage string, run func() error) error {
	slog.Info("corpus load stage started", "engine", record.Engine, "stage", stage)
	start := time.Now()
	if err := run(); err != nil {
		return err
	}
	seconds := time.Since(start).Seconds()
	record.Stages = append(record.Stages, storage.StageTiming{Name: stage, Seconds: seconds})
	slog.Info("corpus load stage finished", "engine", record.Engine, "stage", stage, "seconds", seconds)
	return nil
}

// copyArticles returns the ids in corpus order, which the vectors must follow.
func copyArticles(ctx context.Context, tx pgx.Tx, c storage.Corpus, batchSize int) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	toRow := func(a document.Article) ([]any, error) {
		if a.ID == uuid.Nil {
			return nil, fmt.Errorf("article %d has no id", len(ids))
		}
		if a.Language != c.Language {
			return nil, fmt.Errorf("article %s is in %q, the corpus in %q", a.ID, a.Language, c.Language)
		}
		ids = append(ids, a.ID)
		return insertRow(a)
	}
	plan := copyPlan[document.Article]{Table: "articles", Columns: ArticleColumns, Items: c.Articles, ToRow: toRow, BatchSize: batchSize}
	if err := copyBatches(ctx, tx, plan); err != nil {
		return nil, fmt.Errorf("copy articles: %w", err)
	}
	return ids, nil
}

// copyVectors refuses any vector out of step with the articles, orphans included.
func copyVectors(ctx context.Context, tx pgx.Tx, c storage.Corpus, ids []uuid.UUID, batchSize int) (int64, error) {
	var n int64
	toRow := func(v embedding.Vec) ([]any, error) {
		if n >= int64(len(ids)) {
			return nil, fmt.Errorf("vector %d for article %s is past the last of %d articles", n, v.ID, len(ids))
		}
		if v.ID != ids[n] {
			return nil, fmt.Errorf("vector %d is for article %s, article %d is %s", n, v.ID, n, ids[n])
		}
		n++
		return []any{v.ID, c.ModelTag, pgvector.NewVector(v.Embedding)}, nil
	}
	plan := copyPlan[embedding.Vec]{
		Table:     "article_embeddings",
		Columns:   []string{"article_id", "model_name", "embedding"},
		Items:     c.Vectors,
		ToRow:     toRow,
		BatchSize: batchSize,
	}
	if err := copyBatches(ctx, tx, plan); err != nil {
		return 0, fmt.Errorf("copy embeddings: %w", err)
	}
	return n, nil
}

type copyPlan[T any] struct {
	Table     string
	Columns   []string
	Items     iter.Seq2[T, error]
	ToRow     func(T) ([]any, error)
	BatchSize int
}

func copyBatches[T any](ctx context.Context, tx pgx.Tx, plan copyPlan[T]) error {
	next, stop := iter.Pull2(plan.Items)
	defer stop()
	src := &batchSource[T]{next: next, toRow: plan.ToRow}
	var copied, reported int64
	for !src.exhausted {
		src.left = plan.BatchSize
		n, err := tx.CopyFrom(ctx, pgx.Identifier{plan.Table}, plan.Columns, src)
		if err != nil {
			return err
		}
		copied += n
		if copied-reported >= storage.ProgressRows {
			slog.Info("corpus load progress", "table", plan.Table, "rows", copied)
			reported = copied
		}
	}
	return nil
}

// batchSource ends after left rows, so one iterator feeds consecutive COPYs.
type batchSource[T any] struct {
	next      func() (T, error, bool)
	toRow     func(T) ([]any, error)
	left      int
	exhausted bool
	row       []any
	err       error
}

func (s *batchSource[T]) Next() bool {
	if s.left == 0 || s.exhausted || s.err != nil {
		return false
	}
	item, err, ok := s.next()
	if !ok {
		s.exhausted = true
		return false
	}
	if err == nil {
		s.row, err = s.toRow(item)
	}
	if err != nil {
		s.err = err
		return false
	}
	s.left--
	return true
}

func (s *batchSource[T]) Values() ([]any, error) { return s.row, nil }

func (s *batchSource[T]) Err() error { return s.err }
