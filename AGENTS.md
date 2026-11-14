# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Tusker is a full-text and semantic search engine for exploring multilingual news headlines and articles. The project uses Go 1.25 and focuses on importing, processing, and storing news data from various sources (currently Kaggle datasets).

## Research Goals

**Master Thesis**: "PostgreSQL as a Search Engine"

This project is part of a master thesis research that explores PostgreSQL's comprehensive search capabilities and compares its performance with Elasticsearch through extensive benchmarks across multiple search paradigms.

**Search Paradigms to Explore**:
1. **Full-Text Search**: Traditional keyword-based full-text search with relevance ranking
2. **Structured Search**: Boolean operators and field-specific queries
3. **Fuzzy/Approximate Search**: Trigram similarity (pg_trgm), Levenshtein distance for typo tolerance
4. **Semantic Search**: Vector-based similarity search using embeddings
5. **Hybrid Search**: Combining multiple search approaches for optimal results

**Key Goal**: Evaluate PostgreSQL as a comprehensive alternative to dedicated search engines like Elasticsearch, exploring capabilities beyond basic exact matching and simple filtering.


## Design Principles
- You MUST write clean, idiomatic Go code following best practices.
- You MUST organize code into clear packages with single responsibility.
- You MUST NOT write unnecessary comments; code should be self-explanatory.
- You MUST write unit tests for all core logic with high coverage.

## Architecture

The project follows a layered architecture pattern:

- **cmd/**: Entry points for different operations
    - `datapipe/`: Unified data-pipeline CLI. The core path is **preprocess → embed → load**; every stage after the first reads the canonical package, which is what keeps article IDs identical across engines.
        - `datapipe preprocess`: Maps a raw data package (`.csv`, `.jsonl`, `.parquet` resources) through `datasets/<ds>/mapping.yaml` into a canonical data package at `datasets/<ds>/canonical/<version>/` (`--version`, default `v1`), deduplicated by id; a rerun that yields the same articles is a no-op, and one that yields different articles under an existing version is refused
        - `datapipe load --engine <native|paradedb|tiger|es> --canonical <descriptor> [--embeddings <descriptor>]`: Verifies the canonical package and the embeddings package built from it, loads both into an empty engine, builds the indexes afterwards as timed stages, and writes the load record to the engine and to `datasets/<ds>/loads/<engine>/` — see [docs/embeddings.md](docs/embeddings.md)
    - `news_api/`: HTTP API server for search functionality
    - `schemagen/`: Schema generation utilities
    - `bench/`: IR benchmark CLI — see [docs/bench.md](docs/bench.md)

- **internal/**: Core business logic organized by domain
    - `types/`: Core type definitions organized by bounded contexts
        - `document/`: Document types (Article, Metadata). A field is promoted onto `Article` — and gets its own column — only when an engine must filter, sort or rank on it; `PublishedAt` and `PublisherDomain` are the current examples. Everything else lives in `Metadata`, which pairs structured fields (`PublisherName`, `Authors`, `Section`, `Keywords`, `Image`, `DateModified`) with an open `Extra map[string]any` for dataset-specific values.
        - `language/`: The ISO 639-3 corpus language (`language.Code`, only `eng` today) and the text analysis each engine runs for it.
        - `query/`: Query types (search query types, language, scoring, cursor)
        - `operator/`: Operator value object (AND/OR logic)
    - `ingest/`: Data ingestion pipeline
        - `reader/`: CSV/JSONL/Parquet/YAML data reading and parsing
    - `api/`: API layer for HTTP server
    - `storage/`: Storage abstractions and implementations
        - `factory/`: Storage factory for creating storage instances
        - `pg/`: PostgreSQL storage implementation with full-text search
        - `es/`: Elasticsearch storage implementation

- **pkg/**: Shared packages and APIs
    - `apis/datamapping/`: Data mapping type definitions
    - `schema/`: Schema generation utilities

- **api/**: API schemas and examples
    - Data mapping configuration examples and JSON schemas


- **tracks/**: Benchmark tracks — each a self-contained folder with `spec.yaml`, `suite.yaml`, `trec/` (pool + judgments), and `reports/`
- **db/**: Database-related files
    - `migrations/`: SQL migrations per Postgres engine, `<engine>/schema/` (golang-migrate) and `<engine>/indexes.sql` (the search indexes, built after the load), embedded by the `migrations` Go package
    - `query/`: SQL query files for database operations
- **dataset/**: Sample datasets and documentation
- **scripts/**: Utility scripts for setup and maintenance

## Key Components

### Type System Organization

The type system is organized into bounded contexts, each with its own package:

**Benefits of this organization**:
- Clear separation of concerns between document and query types
- Reduced import cycles through proper package boundaries
- Clean namespacing (e.g., `query.QueryString`, `document.Article`)
- Easier to understand and navigate type definitions
- Follows DDD bounded context principles
- Query API design follows industry standards (Elasticsearch `query_string` terminology)

### Data Mapping System
The project uses YAML configuration files to map source data fields to internal Article structure. Configuration files follow the DataMapping schema (`api/mapping-spec/datamapping-v1.json`) with fieldMappings that specify source/target fields and their source types, a `constants:` block (`Language: eng`) and `output.format` (`parquet` or `jsonl`). An unknown key, two sources on one target, or a target outside the list fails the load. `Title` and `URL` are required on every mapping, and `PublisherDomain` is always derived from the url.

Article ids are `uuidv5` of the normalised url, the only `idStrategy` kind, and the default when `idStrategy` is left out; its `source`, if given, must be the column mapped to `URL`. The same input always yields the same ids, so preprocess can be re-run without orphaning pools, qrels, or embeddings. No mapping sets the id directly: a dataset's own id, or any column unique per record, maps to `Metadata.ExternalID` so a canonical row can be traced back to its raw record.

```yaml
idStrategy:
  kind: uuidv5
  source: url
```

An optional `identity.ignore` block lists the url parts that do not change which article a url names: `scheme`, `www`, `trailingSlash`, `fragment`, and `queryParams` by name or by a `prefix*`. The id then hashes the url with those parts left out, so preprocess keeps one article per page; the stored url stays as written. Left out, ids follow the url as written, and the canonical descriptor records the block when it is set.

```yaml
identity:
  ignore:
    scheme: true
    www: true
    trailingSlash: true
    fragment: true
    queryParams: ["utm_*"]
```

### Storage Layer
Follows idiomatic Go patterns with sub-package organization:
- **Interfaces**: Storage contracts define clear responsibilities
    - `FTSSearcher`: Full-text search operations
    - `Indexer`: Document indexing operations
    - `Reader`: Document retrieval operations
- **Factory Pattern**: `storage/factory` package provides centralized creation logic
- **PostgreSQL**: `storage/pg` - Full-text search with tsvector, ranking, and pagination
- **Elasticsearch**: `storage/es` - Multilingual search with advanced indexing
- **In-memory**: Built-in implementation for development/testing

**Key Features**:
- SearchResult with pagination metadata (total, hasMore, page info)
- PostgreSQL uses native tsvector with ts_rank for relevance scoring
- Factory pattern avoids import cycles while maintaining clean separation
- Separate interfaces for search and indexing operations
- Clean file naming without redundant prefixes

### Pipeline Architecture
Uses a pipeline pattern for data processing with common interfaces:
1. Reader loads and parses source data
2. Mapper transforms data according to configuration
3. Collector orchestrates the process
4. Factory creates storage instances based on configuration
5. Storage persists the articles with bulk operations support

### Bench CLI (`cmd/bench/`)

TREC-style IR evaluation pipeline. Full docs: [docs/bench.md](docs/bench.md).

**Package layout** (`internal/bench/`):
- `trackctx/` — resolves track folder + all artifact paths; single source of truth for every subcommand
- `spec/` — `BenchSpec` YAML: engines, jobs, metrics config, `defaults.judgments`
- `suite/` — `TestSuite` YAML: queries, per-engine templates
- `dialect/` — the syntax each engine type reads suite params in (postgres positional, elasticsearch Mustache, api literal): load-time block checks and the request each executor receives
- `pool/` — TREC-style candidate pooling
- `judgment/` — strategy taxonomy (keyword / claude-cli / claude-api / manual); batched grading; incremental writer
- `runner/` — orchestration: warmup + measured iterations, per-query metrics
- `report/` — aggregation, JSON output, `bench show report`
- `metrics/` — NDCG, MAP, MRR, Bpref, P/R/F1
- `meta/` — provenance block embedded in every artifact (run_id, tool, generated_at, sources)
- `version/` — schema version constant + checker

**Track convention**: a track is any folder holding `spec.yaml` + `suite.yaml` + `trec/` (+ `reports/`) — the layout above it is not enforced; this repo groups them as `tracks/<dataset>/<paradigm>/`. Track args are ordinary paths: absolute, relative to the track root (`--track-root` / `BENCH_TRACK_ROOT`, default cwd), a glob, or omitted to walk up from CWD.

**Strategy taxonomy**: `keyword` (token-overlap), `bm25` (pool-local Okapi BM25), `vector` (embedding cosine), `hybrid` (BM25 + vector fusion), `claude-cli` / `claude-api` (LLM batched), `manual` (human placeholders). `vector`/`hybrid` read doc vectors from a storage-agnostic `storage.VectorStore` (PG `article_embeddings` now, ES stubbed; PG precedence) and take query vectors from the track's `trec/query-vectors/` package when it has one, from Ollama otherwise (`--pg` + `EMBEDDING_BASE_URL`). `validate`/`pool`/`run` fill the reserved `query_vector` arg from the same source.

**Schema v1**: every artifact has `schema_version: 1` + `meta:` block. Loading without it is a hard error — no backward-compat tolerance.

### HTTP API Server
Built with Echo framework providing:
- **Search API**: Comprehensive search capabilities with multiple query types
- **Health Checks**: Database connectivity and service health monitoring
- **Middleware**: CORS support, request logging, and error recovery
- **Configuration**: Environment-based config with validation
- **Graceful Shutdown**: Proper resource cleanup on termination

**Search API Endpoints**:

1. **Simple Query String API** (`GET /v1/articles/search`)
    - Simple, Google-like search experience
    - Application determines optimal fields and weights
    - Query parameter: `?query=climate change`
    - Best for: End-user search boxes, simple text queries

2. **Structured Match API** (`POST /v1/articles/_search/match`)
    - Explicit single-field control
    - User specifies field, operator (AND/OR), fuzziness
    - Request body:
      ```json
      {
        "field": "title",
        "query": "climate change",
        "operator": "AND",
        "fuzziness": 1
      }
      ```
    - Best for: Advanced users needing precise control

**Search Features**:
- Full-text search with relevance ranking
- Single-field and multi-field search support
- Cursor-based pagination with total count and hasMore indicators
- Field-level weight control for relevance tuning
- Operator control (AND/OR) for term combination
- Fuzziness support for typo tolerance (Elasticsearch)
- Query language as an ISO 639-3 code: `eng` only (the default); any other value returns 400
- Input validation and comprehensive error handling
- PostgreSQL: tsvector with ts_rank scoring
- Elasticsearch: match/multi_match queries with BM25 scoring

**API Design Pattern**:
- **Simple API**: Application-optimized (QueryString)
- **Explicit API**: User-controlled (Match, MultiMatch)
- **DTO Layer**: Clean separation between API and domain
- **Optional Interfaces**: Storage backends can optionally implement Match/MultiMatch
- **Extensible**: Easy to add new query types in the future

## Development Commands

### Database
```bash
# Start PostgreSQL container
docker-compose up -d

# Database runs on port 54320 with:
# - Database: news_db
# - User: news_user
# - Password: news_password
```

### Building and Running
```bash
# Build specific command
go build -o bin/datapipe ./cmd/datapipe

# Run with environment variables (stages: preprocess, load)
ENV_PATHS=cmd/datapipe/pg.env go run ./cmd/datapipe load --engine native --canonical <descriptor>

# Build other commands
go build -o bin/schemagen ./cmd/schemagen
go build -o bin/news-search ./cmd/news_api

# Run tests
go test ./...

# Run tests for specific package
go test ./internal/reader

# Format code
go fmt ./...

# Vet code
go vet ./...
```

### Environment Setup
Commands expect environment variables (typically in `.env` files):

**Data Import (`cmd/data_import/.env`)**:
- `STORAGE_TYPE`: Storage backend (`pg`, `es`, `in_mem`)
- `MAPPING_CONFIG_PATH`: Path to YAML mapping configuration
- `PG_CONNECTION_STRING`: PostgreSQL connection string
- `ES_ADDRESSES`: Elasticsearch cluster addresses (comma-separated)
- `ES_INDEX_NAME`: Elasticsearch index name
- `ES_DIAL_TIMEOUT`, `ES_RESPONSE_HEADER_TIMEOUT`: Go durations bounding each Elasticsearch request, bulk flushes included (default 5s, 30s); a timed-out request is not retried

**Search API (`cmd/news_search/.env`)**:
- `PORT`: HTTP server port (default: 8080)
- `USE_HTTP2`: Enable HTTP/2 support (`true`/`false`)
- `CORS_ORIGINS`: Allowed CORS origins (comma-separated)
- `HTTP_READ_HEADER_TIMEOUT`, `HTTP_READ_TIMEOUT`, `HTTP_WRITE_TIMEOUT`, `HTTP_IDLE_TIMEOUT`: Go durations (default 5s, 30s, 120s, 120s)
- `HTTP_REQUEST_TIMEOUT`: Go duration after which a handler's context is cancelled (default 90s); must be shorter than `HTTP_WRITE_TIMEOUT`
- `HTTP_BODY_LIMIT`: Largest request body, e.g. `1M` (default); larger bodies get 413

## Testing
Tests are located alongside source files with `_test.go` suffix. Use standard Go testing patterns:
- `go test ./...` - Run all tests
- `go test -v ./internal/reader` - Run specific package tests with verbose output

**Type System Purity**:
- Type definitions should represent search concepts, not implementation details
- Storage layer translates query types to engine-specific queries
- Type validation ensures type safety before reaching storage layer