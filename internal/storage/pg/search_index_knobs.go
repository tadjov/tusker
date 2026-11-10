package pg

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tadjov/tusker/internal/storage"
)

// searchIndexKnobs is what an engine's search index declares beyond the shared build settings.
type searchIndexKnobs struct {
	settings []string
	inspect  func(ctx context.Context, conn *pgx.Conn, settings map[string]string) error
}

var engineKnobs = map[storage.Engine]searchIndexKnobs{
	storage.ParadeDB: {inspect: inspectParadeIndex},
	storage.Tiger: {
		settings: []string{"pg_textsearch.memory_limit", "pg_textsearch.segments_per_level", "pg_textsearch.memtable_spill_threshold"},
		inspect:  inspectTigerIndex,
	},
}

const (
	paradeSearchIndex = "idx_articles_search"
	tigerSearchIndex  = "idx_articles_bm25"
)

// inspectParadeIndex refuses more than one segment, matching Elasticsearch's force-merge to one.
func inspectParadeIndex(ctx context.Context, conn *pgx.Conn, settings map[string]string) error {
	var options []string
	err := conn.QueryRow(ctx, "SELECT coalesce(reloptions, '{}') FROM pg_class WHERE oid = to_regclass($1)", paradeSearchIndex).Scan(&options)
	if err != nil {
		return fmt.Errorf("read options of %s: %w", paradeSearchIndex, err)
	}
	for _, key := range []string{"target_segment_count", "mutable_segment_rows"} {
		value, ok := optionValue(options, key)
		if !ok {
			return fmt.Errorf("index %s leaves %s to the pg_search default", paradeSearchIndex, key)
		}
		settings[paradeSearchIndex+"."+key] = value
	}

	var segments int
	err = conn.QueryRow(ctx, "SELECT count(*) FROM paradedb.index_info($1::regclass, false)", paradeSearchIndex).Scan(&segments)
	if err != nil {
		return fmt.Errorf("count segments of %s: %w", paradeSearchIndex, err)
	}
	settings[paradeSearchIndex+".segments"] = strconv.Itoa(segments)
	if segments != 1 {
		return fmt.Errorf("index %s has %d segments after the build, want 1", paradeSearchIndex, segments)
	}
	return nil
}

// pg_textsearch 1.1.0 states the segment total only in bm25_summarize_index.
var tigerSegmentTotal = regexp.MustCompile(`Total: (\d+) segments`)

func inspectTigerIndex(ctx context.Context, conn *pgx.Conn, settings map[string]string) error {
	var summary string
	if err := conn.QueryRow(ctx, "SELECT bm25_summarize_index($1)", tigerSearchIndex).Scan(&summary); err != nil {
		return fmt.Errorf("summarize %s: %w", tigerSearchIndex, err)
	}
	match := tigerSegmentTotal.FindStringSubmatch(summary)
	if match == nil {
		return fmt.Errorf("summary of %s states no segment total", tigerSearchIndex)
	}
	settings[tigerSearchIndex+".segments"] = match[1]
	return nil
}

func optionValue(options []string, key string) (string, bool) {
	for _, option := range options {
		if name, value, found := strings.Cut(option, "="); found && name == key {
			return value, true
		}
	}
	return "", false
}
