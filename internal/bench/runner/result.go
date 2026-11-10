package runner

import (
	"github.com/DjordjeVuckovic/tusker/internal/bench/metrics"
	"github.com/google/uuid"
)

type QueryResult struct {
	QueryID       string
	Category      string
	EngineName    string
	RequestDigest string
	Scores        metrics.ScoreSet
	RankedDocIDs  []uuid.UUID
	ReturnedCount int
	CorpusMatches *int64
	Latency       LatencyStats
	Error         error
}

type JobResult struct {
	JobName     string
	Results     map[string]map[string]QueryResult // [queryID][engineName]
	QueryOrder  []string
	EngineNames []string
}

type BenchmarkResult struct {
	Jobs   []*JobResult
	Config Config
}

func (br *BenchmarkResult) AllEngineNames() []string {
	seen := make(map[string]bool)
	var names []string
	for _, jr := range br.Jobs {
		for _, name := range jr.EngineNames {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
	}
	return names
}
