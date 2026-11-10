package pool

import (
	"maps"
	"slices"

	"github.com/DjordjeVuckovic/tusker/internal/bench/engine"
	"github.com/DjordjeVuckovic/tusker/internal/bench/meta"
	"github.com/google/uuid"
)

type PoolFile struct {
	SchemaVersion int         `yaml:"schema_version"`
	Meta          meta.Meta   `yaml:"meta"`
	SuiteName     string      `yaml:"suite_name,omitempty"`
	Queries       []PoolEntry `yaml:"queries"`
}

type PoolEntry struct {
	QueryID   string      `yaml:"query_id"`
	QueryDesc string      `yaml:"query_desc"`
	Category  string      `yaml:"category,omitempty"`
	Docs      []PooledDoc `yaml:"docs"`
	// RequestDigests hold, by engine, the digest of the request it was sent.
	RequestDigests map[string]string `yaml:"request_digests,omitempty"`
}

type PooledDoc struct {
	DocID   uuid.UUID `yaml:"doc_id"`
	Sources []string  `yaml:"sources"`
}

func PoolResults(results map[string]*engine.Execution, depth int) []PooledDoc {
	seen := make(map[uuid.UUID]*PooledDoc)
	var order []uuid.UUID

	for _, engineName := range slices.Sorted(maps.Keys(results)) {
		exec := results[engineName]
		if exec == nil {
			continue
		}
		limit := depth
		if limit > len(exec.RankedDocIDs) {
			limit = len(exec.RankedDocIDs)
		}
		for _, docID := range exec.RankedDocIDs[:limit] {
			if pd, ok := seen[docID]; ok {
				pd.Sources = append(pd.Sources, engineName)
			} else {
				seen[docID] = &PooledDoc{
					DocID:   docID,
					Sources: []string{engineName},
				}
				order = append(order, docID)
			}
		}
	}

	docs := make([]PooledDoc, 0, len(order))
	for _, id := range order {
		docs = append(docs, *seen[id])
	}
	return docs
}

// ShallowEngine is an engine whose deepest contribution to the pool fell short
// of the requested depth.
type ShallowEngine struct {
	Name        string
	MaxReturned int
}

// ShallowEngines reports the largest number of documents each engine
// contributed to any one query, for engines that never reached depth. A pool
// stamped meta.pool_depth: D overstates those engines: nothing they ranked
// went D deep.
//
// It reports the count and nothing more. A template capping its LIMIT below D
// and a query that genuinely matches few documents are indistinguishable here,
// and which one it is belongs to the reader.
func ShallowEngines(pf *PoolFile, depth int) []ShallowEngine {
	if pf == nil || depth <= 0 {
		return nil
	}
	maxByEngine := map[string]int{}
	for _, entry := range pf.Queries {
		perQuery := map[string]int{}
		for _, doc := range entry.Docs {
			for _, src := range doc.Sources {
				perQuery[src]++
			}
		}
		for name, n := range perQuery {
			if n > maxByEngine[name] {
				maxByEngine[name] = n
			}
		}
	}

	var shallow []ShallowEngine
	for _, name := range slices.Sorted(maps.Keys(maxByEngine)) {
		if maxByEngine[name] < depth {
			shallow = append(shallow, ShallowEngine{Name: name, MaxReturned: maxByEngine[name]})
		}
	}
	return shallow
}
