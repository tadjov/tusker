package runner

import (
	"fmt"

	"github.com/DjordjeVuckovic/tusker/internal/storage"
)

var DefaultKValues = []int{3, 5, 10}

const (
	DefaultRelevanceThreshold = 1
	DefaultWarmupRuns         = 1
	DefaultRuns               = 3
	DefaultEngineParallelism  = 4

	// QueryParallelismSerial runs one query at a time. Use for bench run so
	// each query gets the engine's undivided attention and latency numbers are
	// clean (no cross-query resource contention).
	QueryParallelismSerial = 1

	// QueryParallelismUnlimited fans out all queries concurrently. Use for
	// bench pool and bench validate where latency measurement doesn't matter
	// and throughput does.
	QueryParallelismUnlimited = 0

	// EngineParallelismSerial runs one engine at a time within a query. Use for
	// bench run: a job's engines share a host, so a fan-out lands one engine's
	// warmup in the same window as another's measured iterations.
	EngineParallelismSerial = 1

	// EngineParallelismUnlimited fans out to every engine in the job at once.
	// Use for bench pool, where only the returned doc IDs matter.
	EngineParallelismUnlimited = 0
)

type Config struct {
	KValues            []int
	RelevanceThreshold int
	WarmupRuns         int
	Runs               int
	// RankingDepth cuts every ranking before it is scored or stored, so scores
	// count only docs a pool that deep could have judged. Zero keeps it whole.
	RankingDepth int
	// QueryParallelism bounds concurrent queries. QueryParallelismSerial (1)
	// = one at a time; QueryParallelismUnlimited (0) = all at once.
	QueryParallelism int
	// EngineParallelism bounds concurrent engine calls within a single query.
	// EngineParallelismSerial (1) = one at a time; EngineParallelismUnlimited
	// (0) = fan out to all engines simultaneously.
	EngineParallelism int
	// Judgments[queryID][docID]grade — pre-loaded by the CLI from the
	// resolved annotations file. When nil, queries are scored without
	// relevance grades and the report flags them as unjudged.
	Judgments map[string]map[string]int
	// VectorStore, when set, embeds queries that reference the reserved
	// query-vector placeholder and injects the result before execution. nil for
	// non-vector tracks (and validate, which is structural only).
	VectorStore storage.VectorStore
}

func DefaultConfig() Config {
	return Config{
		KValues:            DefaultKValues,
		RelevanceThreshold: DefaultRelevanceThreshold,
		WarmupRuns:         DefaultWarmupRuns,
		Runs:               DefaultRuns,
		QueryParallelism:   QueryParallelismSerial,
		EngineParallelism:  DefaultEngineParallelism,
	}
}

func (c Config) validate() error {
	if c.Runs < 1 {
		return fmt.Errorf("measured iterations must be at least 1, got %d", c.Runs)
	}
	if c.WarmupRuns < 0 {
		return fmt.Errorf("warmup iterations must not be negative, got %d", c.WarmupRuns)
	}
	return nil
}
