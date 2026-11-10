// Package migrations holds each Postgres engine's schema, in golang-migrate's format, and its search indexes.
package migrations

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tadjov/tusker/internal/storage"
)

//go:embed native paradedb tiger
var files embed.FS

const schemaTable = "schema_migrations"

// Set is one directory of migrations and its version table.
type Set struct {
	files fs.FS
	table string
}

func Schema(e storage.Engine) (Set, error) {
	if e.Type() != storage.PG {
		return Set{}, fmt.Errorf("engine %q has no Postgres migrations", e)
	}
	sub, err := fs.Sub(files, path.Join(string(e), "schema"))
	if err != nil {
		return Set{}, fmt.Errorf("open the schema migrations of %s: %w", e, err)
	}
	return Set{files: sub, table: schemaTable}, nil
}

// IndexStatement is one statement of an engine's indexes.sql; Index is empty for a SET.
type IndexStatement struct {
	Index string
	SQL   string
}

func (s IndexStatement) String() string {
	if s.Index != "" {
		return s.Index
	}
	return s.SQL
}

var createIndex = regexp.MustCompile(`(?i)^CREATE\s+INDEX\s+(\w+)`)

// SearchIndexes splits indexes.sql on ';', which no statement in it contains.
func SearchIndexes(e storage.Engine) ([]IndexStatement, error) {
	if e.Type() != storage.PG {
		return nil, fmt.Errorf("engine %q has no Postgres search indexes", e)
	}
	body, err := fs.ReadFile(files, path.Join(string(e), "indexes.sql"))
	if err != nil {
		return nil, fmt.Errorf("read the search indexes of %s: %w", e, err)
	}
	var statements []IndexStatement
	for _, sql := range strings.Split(string(body), ";") {
		sql = strings.TrimSpace(sql)
		if sql == "" {
			continue
		}
		statement := IndexStatement{SQL: sql}
		if match := createIndex.FindStringSubmatch(sql); match != nil {
			statement.Index = match[1]
		}
		statements = append(statements, statement)
	}
	return statements, nil
}

// Build migrates the schema and creates every search index, untimed, as a database without a load needs.
func Build(ctx context.Context, conn *pgx.Conn, e storage.Engine) error {
	schema, err := Schema(e)
	if err != nil {
		return err
	}
	if err := schema.Up(ctx, conn); err != nil {
		return fmt.Errorf("migrate the schema of %s: %w", e, err)
	}
	indexes, err := SearchIndexes(e)
	if err != nil {
		return err
	}
	for _, s := range indexes {
		if _, err := conn.Exec(ctx, s.SQL); err != nil {
			return fmt.Errorf("run %s on %s: %w", s, e, err)
		}
	}
	return nil
}

// Migration is one up file; Name drops the version and suffix.
type Migration struct {
	Version uint64
	Name    string
	file    string
}

var upFile = regexp.MustCompile(`^([0-9]+)_(.*)\.up\.sql$`)

func (s Set) Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(s.files, ".")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	var list []Migration
	for _, entry := range entries {
		match := upFile.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		version, err := strconv.ParseUint(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", entry.Name(), err)
		}
		list = append(list, Migration{Version: version, Name: match[2], file: entry.Name()})
	}
	slices.SortFunc(list, func(a, b Migration) int { return cmp.Compare(a.Version, b.Version) })
	return list, nil
}

// DirtyError reports a migration that failed midway.
type DirtyError struct {
	Table   string
	Version uint64
}

func (e *DirtyError) Error() string {
	return fmt.Sprintf("%s is dirty at version %d: a migration failed midway; repair it, then force the version", e.Table, e.Version)
}

// Version returns the last applied version; ok is false when none is.
func (s Set) Version(ctx context.Context, conn *pgx.Conn) (version uint64, ok bool, err error) {
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", s.table).Scan(&exists); err != nil {
		return 0, false, fmt.Errorf("look up %s: %w", s.table, err)
	}
	if !exists {
		return 0, false, nil
	}
	var dirty bool
	err = conn.QueryRow(ctx, "SELECT version, dirty FROM "+s.identifier()+" LIMIT 1").Scan(&version, &dirty)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read %s: %w", s.table, err)
	}
	if dirty {
		return 0, false, &DirtyError{Table: s.table, Version: version}
	}
	return version, true, nil
}

// Pending refuses a database at a version the set does not hold.
func (s Set) Pending(ctx context.Context, conn *pgx.Conn) ([]Migration, error) {
	all, err := s.Migrations()
	if err != nil {
		return nil, err
	}
	version, ok, err := s.Version(ctx, conn)
	if err != nil || !ok {
		return all, err
	}
	at := slices.IndexFunc(all, func(m Migration) bool { return m.Version == version })
	if at < 0 {
		return nil, fmt.Errorf("%s is at version %d, which no migration in this set has", s.table, version)
	}
	return all[at+1:], nil
}

// Apply leaves the version dirty when the migration fails, as golang-migrate does.
func (s Set) Apply(ctx context.Context, conn *pgx.Conn, m Migration) error {
	body, err := fs.ReadFile(s.files, m.file)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", m.file, err)
	}
	if _, err := conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+s.identifier()+
		" (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)"); err != nil {
		return fmt.Errorf("create %s: %w", s.table, err)
	}
	if err := s.setVersion(ctx, conn, m.Version, true); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.file, err)
	}
	return s.setVersion(ctx, conn, m.Version, false)
}

func (s Set) Up(ctx context.Context, conn *pgx.Conn) error {
	pending, err := s.Pending(ctx, conn)
	if err != nil {
		return err
	}
	for _, m := range pending {
		if err := s.Apply(ctx, conn, m); err != nil {
			return err
		}
	}
	return nil
}

func (s Set) setVersion(ctx context.Context, conn *pgx.Conn, version uint64, dirty bool) error {
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "TRUNCATE "+s.identifier()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO "+s.identifier()+" (version, dirty) VALUES ($1, $2)", int64(version), dirty)
		return err
	})
	if err != nil {
		return fmt.Errorf("record version %d in %s: %w", version, s.table, err)
	}
	return nil
}

func (s Set) identifier() string {
	return pgx.Identifier{s.table}.Sanitize()
}
