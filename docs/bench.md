# bench — IR Benchmark CLI

`bench` evaluates full-text, vector, and hybrid search queries against multiple engines (PostgreSQL variants, Elasticsearch, the tusker API), computes IR quality metrics and latency statistics, and writes self-attesting JSON and HTML reports.

## Track convention

Everything lives in a self-contained **track folder**:

```
tracks/<name>/
  spec.yaml                         # engines, jobs, metrics config, defaults
  suite.yaml                        # queries and per-engine SQL/JSON templates
  trec/
    pool.yaml                       # candidate docs (bench pool output)
    annotations.<strategy>.yaml     # relevance grades (bench judge output)
    qrels.<strategy>.tsv            # TREC qrels (bench export --format qrels)
  reports/
    <run_id>.json                   # one per bench run
    latest.json                     # pointer to most recent report
    <run_id>.html                   # optional HTML (bench export --format html)
    <run_id>.md                     # optional Markdown (bench export --format markdown)
```

One track, multiple judgment strategies living side by side.  
Switch strategies with `--judgments <name>` on `bench run` — no YAML editing required.

### Track kind

`spec.yaml` may declare the IR paradigm via `kind: fts | structured | fuzzy | semantic | hybrid`.
It is primarily a taxonomy/provenance label (one per track); requirements are *derived*
from it rather than declared separately. `kind` is **optional**, but omitting it emits a
load-time warning.

`semantic` and `hybrid` derive `RequiresEmbedder = true`: their queries take the reserved
`query_vector` arg, so `validate`/`pool`/`run` need a live query embedder
(`EMBEDDING_BASE_URL` + a postgres engine). Without one, **`bench validate` fails up front**
for these kinds instead of stubbing a fake vector and reporting a misleading OK.

### Query categories

A suite query may carry `category:` (free-form; the fts track uses keyword, phrase, boolean,
multi-field, single-field). When a job's queries span more than one category, `bench run`
prints the aggregated table per category alongside the overall one.

Result-list length is the reason. A strict phrase query legitimately matches two documents
where a keyword query matches thousands, and AP, recall and the NDCG ideal all normalise
against the full judged relevant set, so an engine returning the correct two scores as
though it missed forty. One mean over both measures list length as much as ranking quality.

The tag also reaches the judge: `bench pool` copies it into `pool.yaml`, and the LLM prompt
turns `phrase` and `boolean` into an explicit rule, so an on-topic article that fails the
constraint grades 0.

### Engine connection settings

A postgres engine may pin GUCs that hold for every connection it opens:

```yaml
engines:
  pgvector-cosine:
    type: postgres
    connection: "postgresql://…"
    connection_settings:
      hnsw.ef_search: "200"
```

They are applied in `AfterConnect`, once per connection, and a setting that will not apply
fails the run. Per-statement `SET LOCAL` is not an option: the pool runs each query as its
own implicit transaction on whichever connection is free.

`hnsw.ef_search` fixes the ANN operating point, which decides how many candidates a vector
scan considers and so both its recall and its latency. Turning the planner's index access
off gives an unindexed arm to compare against: `enable_indexscan` plus `enable_bitmapscan`
for the GIN A/B in `fts_quality`, `enable_indexscan` alone for the exact-scan vector
baseline in `news_semantic`.

`hnsw.ef_search` is only the query-time half of that knob. The build-time half, `m` and
`ef_construction`, is fixed when the index is created and cannot be set per connection, so it
is not a spec setting: Postgres declares it in the `CREATE INDEX` of each engine's
`db/migrations/<engine>/indexes.sql`, and Elasticsearch in the `index_options` of
its `dense_vector` mapping, both at the values in `internal/storage/vector.go`. Left undeclared
the products disagree (pgvector builds at `ef_construction = 64`, Elasticsearch at `100`),
which puts part of any recall gap in the graph rather than the engine. Both are held to 64,
pgvector's default, so the loaded Postgres graphs stand as built. Changing the value rebuilds
the graph and is therefore a new run.

### Query templates

A suite query reaches each engine through that engine's own parameter mechanism. The bench
routes values to it and never edits query text. Each engine type has one dialect, the syntax
its blocks are written in: `postgres` is positional (`$1 … $n`), `elasticsearch` is Mustache,
and `api` is literal. `validate`, `pool` and `run` check every block a job engine runs against
its dialect before any query runs, and a template read by engines of two dialects fails too.

**Postgres.** A template is plain SQL with `$1 … $n`, as in a prepared statement, plus an
`args` list naming the param behind each position:

```yaml
templates:
  - id: pg_idx_multi_field
    args: [terms, rank_norm, limit]
    query: |
      SELECT id FROM articles
      WHERE search_vector @@ plainto_tsquery('english', $1)
      ORDER BY ts_rank(search_vector, plainto_tsquery('english', $1), $2::int) DESC
      LIMIT $3::int
queries:
  - id: qs-climate
    engines:
      pg-gin:
        template: pg_idx_multi_field
        params: { terms: "climate change", limit: 100 }
```

The bench builds the arguments in `args` order from the query's params, the engine's params
and the run's query vector. Every value is sent as text, so the SQL casts where it needs
another type (`$3::int`); a `null` param binds as SQL NULL, and a list or map param is
rejected because a placeholder binds one value. The statement reads as it runs and can be pasted into psql:
`PREPARE q AS …; EXECUTE q('climate change', 0, 100);`. An inline or `file:` Postgres query
follows the same rule, with `args` beside `query`, or is fully literal SQL with no params.

Postgres binds values, not identifiers, so a shape that differs by a column, a field list or a
function name is its own template rather than a param.

`args` must name exactly `$1 … $n`: a statement that uses more placeholders than `args`
names, fewer, or skips one fails the check. A suite fails to load when a query gives a param
that none of its args uses, and the check also fails when no layer supplies an arg an
engine's queries take.

**Elasticsearch.** A template is the Mustache source of a native search template:

```yaml
templates:
  - id: es_hybrid
    args: [terms, query_vector]
    query: |
      {
        "query": {"multi_match": {"query": "{{terms}}", "fields": ["title^3", "description^2", "content"]}},
        "knn": {"field": "embedding", "query_vector": {{#toJson}}query_vector{{/toJson}}, "k": 50, "num_candidates": 200},
        "size": 50
      }
```

`validate`, `pool` and `run` first store every template an Elasticsearch engine's queries use
with `PUT _scripts/<spec id>-<template id>`, then send each query to `_search/template` with
its params. Two different templates that would land on one stored id fail the command before
anything is stored.

Quote a string param inside the JSON, as in `"{{terms}}"`: Elasticsearch escapes quotes and
backslashes in the value, so `Ukraine's voters don't trust the "election"` arrives intact. Use
`{{#toJson}}…{{/toJson}}` for the query vector, which has to arrive as an array. On a string
it renders the text without quotes and breaks the body.

`args` lists the params the template reads, in any order. Mustache renders a name it is not
given as empty text, so the check fails when the source reads a name outside every section
that `args` leaves out, or when `args` names one the source never reads. A name inside a
section may be a field of each list element, so it need not be an arg.

An inline or `file:` Elasticsearch block is plain Query DSL, runs through `_search` as written
and takes no args or `{{…}}` names. An API block is a literal request descriptor the same way:
the values are typed into it, and it takes no args or `{{…}}` names.

**The query vector.** `query_vector` is a reserved arg. The run embeds the query and fills it,
as pgvector text for Postgres (`$2::vector`) and as a float array for a search template. A
query needs the vector exactly when one of its statements lists `query_vector` in `args`, and
a query may not set it in its own `params`.

A bound value still reaches the engine's own query language wherever a function parses it as
query syntax. `to_tsquery` rejects input its syntax doesn't allow, such as a stray double quote
inside a phrase. ParadeDB's `field @@@ $1` and `paradedb.parse($1)` fail on an apostrophe, so
ParadeDB templates take text through `paradedb.match('<field>', $1)`, which tokenizes it as
plain text. A phrase wrapped in quotes for `paradedb.parse` takes an apostrophe but not a
double quote.

Binding changes how Postgres plans the query. Every query that uses a template shares one
prepared statement per connection, and after five executions the planner may switch it to a
generic plan, so a query's latency would depend on where it sits in the suite. Every postgres
engine therefore sets `plan_cache_mode: force_custom_plan` in its `connection_settings`, which
plans each execution for its own values. Latency from before query text was bound is not
comparable with latency after it.

### Engine params and shared query blocks

An engine may declare template params of its own, and may take its per-query block from
another engine:

```yaml
engines:
  pg-gin:
    type: postgres
    connection: "postgresql://…"
    params:
      rank_norm: "0"
  pg-gin-norm:
    type: postgres
    connection: "postgresql://…"
    queries_from: pg-gin
    params:
      rank_norm: "1"
```

Params merge widest first, engine defaults and then the query's own `params:`, so an engine
default fills what a query omits and never overrides what it states. The run's query vector
fills `query_vector`, which neither layer may set.

`queries_from` is why the two arms above stay comparable. Written out per query, an arm that
differs only in a ranking argument is 30 duplicated blocks, and the first one edited on its
own stops isolating the argument under test without anything erroring. Aliasing is one level
deep: an alias must name a real engine of its own type that is not itself an alias, and the
spec fails to load otherwise. `bench validate` still dry-runs every engine separately, so both arms are
checked even though one block backs them.

An alias borrows the query block, not the params — it contributes only what it declares
itself, which is what lets `pg-gin-norm` set `rank_norm` without inheriting `pg-gin`'s. So an
alias states the full set it needs; omitting one the template requires fails the load rather
than falling back.

An arm has to exist before `bench pool`, not just before `bench run`. Each engine contributes
its own top-K to the pool; added afterwards, the documents only it ranks highly come back
Unjudged and its NDCG reads low for a reason that has nothing to do with its ranking.

#### `embedding_model`

`embedding_model` is the param a vector track has to declare:

```yaml
engines:
  pgvector-cosine:
    type: postgres
    connection: "postgresql://…"
    params:
      embedding_model: "qwen3-embedding:0.6b@1024"
```

The value is the tag stored vectors carry: the model with its width. `article_embeddings` is
keyed `(article_id, model_name)`, so vectors from two models coexist in it by design, and the
vector templates take `embedding_model` as an arg and filter on it (`WHERE model_name = $1`)
rather than ranking across both spaces. The declared value is checked against the tag of the
query vectors before `validate`, `pool` or `run` touches an engine, and a disagreement fails:
comparing vectors from two models is arithmetic that succeeds and ranks nothing.

#### Query vectors

A track that holds a query-vectors package at `trec/query-vectors/` is served from it, with no
query embedder running; this is how the bench gets query vectors from the same runtime as the
documents. `scripts/embed_queries.py` writes the package. The bench refuses it when:

- its resource no longer matches the descriptor's hash
- a query in the `suite.yaml` the jobs run has no row, or its row's `text` differs from the query's `description`; the error names the query id. Edits that leave every description as it was don't need re-embedding
- its `embedding.tag` differs from the engines' `embedding_model`, or no engine declares one

A query whose text the package doesn't hold fails with the text in the error. Without a
package, queries are embedded through Ollama (`EMBEDDING_BASE_URL`, and `EMBEDDING_MODEL`
defaulting to `qwen3-embedding:0.6b`) at width `EMBEDDING_DIM`, default 1024, tagged `qwen3-embedding:0.6b@1024`.

The Elasticsearch arm needs no such filter — its vector is a `dense_vector` field on the
article document, so re-embedding overwrites rather than accumulates — but it declares
`embedding_model` too, so the spec records what every arm was measured against.

## Pipeline

```
bench init    <name>               1. scaffold tracks/<name>/
bench validate [<name>]            2. dry-run all queries through each engine
bench pool     [<name>] [--depth N]
                                   3. gather candidate docs → trec/pool.yaml
bench judge    [<name>] --strategy <S>
                                   4. grade pool → trec/annotations.<S>.yaml
bench run      [<name>]            5. execute suite + compute metrics → reports/
bench export   [<name>] --format <F>
                                   6. export HTML / Markdown / TREC qrels
```

Inspect at any point:

```
bench status [<name>]              one-glance pipeline state
bench show   report|pool|judgments|spec [<name>]
bench diff   [<name>]              compare latest two runs
bench clean  [<name>]              remove old report files
```

Every command accepts a track path as a positional arg (`bench run tracks/global-news-dataset/fts_quality`), a `--track` flag, or resolves from the current directory when you `cd` into a track. Relative paths start at `--track-root` / `BENCH_TRACK_ROOT` — see [Track resolution](#track-resolution--the-track-root).

## Strategy taxonomy

| Strategy     | Class     | Status   | Description                                              |
|--------------|-----------|----------|----------------------------------------------------------|
| `lexical`    | Heuristic | ✅        | Token-overlap baseline — fast, deterministic, no network |
| `bm25`       | Heuristic | ✅        | Pool-local Okapi BM25, normalised → grade (no network)   |
| `vector`     | Heuristic | ✅        | Cosine similarity; doc vectors from the store → grade    |
| `hybrid`     | Heuristic | ✅        | Weighted BM25 + vector fusion → grade                    |
| `claude-cli` | LLM       | ✅        | `claude -p` subprocess per batch                         |
| `claude-api` | LLM       | ✅        | Anthropic Messages API per batch                         |
| `manual`     | Human     | ✅        | Emits `grade: -1` placeholders for hand-grading          |

`vector`/`hybrid` are storage-agnostic: document vectors are read from a
`storage.VectorStore` (Postgres `article_embeddings` today, ES later — PG takes
precedence), and they never re-embed documents. Configure with `--pg`/`PG_CONNECTION_STRING`.
`judge`, `validate`, `pool` and `run` all take query vectors from the track's package when
it has one, from Ollama otherwise (`--embedding-base`/`EMBEDDING_BASE_URL`, + optional
`EMBEDDING_MODEL`); vector queries get them as the reserved `query_vector` arg. `bm25` computes
term statistics over each query's candidate pool, so it runs with no external
services.

File convention: `trec/annotations.<strategy>.yaml`, `trec/qrels.<strategy>.tsv`.

## Schema v1

Every produced artifact carries `schema_version: 1` and a `meta:` block. The meta block records `run_id`, `tool` (with git sha), `generated_at`, and artifact-specific provenance (spec_id, strategy, judge_model, judge_prompt_version, sources).

Loading any artifact without `schema_version: 1` is a hard error — there is no silent tolerance.

## Command reference

### `bench init <name>`

Scaffolds `tracks/<name>/` with `spec.yaml`, `suite.yaml`, `trec/`, `reports/`, and `README.md`.

### `bench validate [<name>]`

Dry-runs every query through every engine using the engine's native validation endpoint: PostgreSQL `EXPLAIN` with the ordered args, and for Elasticsearch `_validate/query` on the body, rendered first with `_render/template` when the query is a search template. Reports per-query pass/fail with colored status. No documents are written; the search templates are stored, as they are before `pool` and `run`.

For `semantic`/`hybrid` kinds it first requires an embedder (fails fast if `EMBEDDING_BASE_URL` is unset) and embeds each query for real, so dimension mismatches surface here. It also warns when the declared `kind` and the queries' use of `query_vector` disagree.

### `bench pool [<name>] [--depth N]`

Runs all queries in parallel, gathers the top-N results per engine, deduplicates by doc ID, and writes `trec/pool.yaml`. Default depth is from `spec.defaults.pool_depth`.

Each pool entry records, under `request_digests`, a sha256 per engine of the request that engine was sent: the statement or body, its args, its params (the query vector included) and a stored template's source. The stored template's id is left out. Warmup, iterations, timeout and K are not part of a request, so changing them needs no re-pool.

### `bench judge [<name>] --strategy <S>`

Grades every `(query, doc)` pair in the pool using the chosen strategy. Output: `trec/annotations.<S>.yaml`, with `graded_count` and `unjudged_count` in its meta block. The command writes the file and then exits non-zero when any pooled doc is left unjudged (`manual` excepted).

Key flags:
- `--resume` — skip docs already graded (errors if the strategy, provider, model, prompt version or rubric version changed). `bm25` and `hybrid` skip only fully graded queries and regrade any other query whole, since their grades are relative to the pool
- `--batch N` — override LLM batch size (`bm25` and `hybrid` grade each query's whole pool at once and refuse it)
- `--concurrency N` — parallel Grade calls (per-doc mode)
- `--allow-unjudged` — exit 0 even when some pooled docs are left unjudged. A query with no usable terms still fails the run
- `--force` — `manual` only: rewrite the placeholders from scratch. Without it, `manual` keeps every existing hand grade, adds `-1` placeholders for new pool docs, and refuses a file another judge wrote

### `bench run [<name>] [--judgments <S|path>] [--jobs <name,...>]`

Executes the suite against all engines, computes IR metrics and latency, prints a styled table with per-engine NDCG/MAP/MRR/Bpref + latency percentiles + statistical significance, then writes `reports/<run_id>.json`.

The report is always written. A run fails, exits non-zero and leaves `reports/latest.json` alone when:
- a (query, engine) execution errored
- it measured a (query, engine) cell the pool dropped (`bench pool --allow-partial`)
- it ran a query the pool holds no entry for
- it measured an engine the pool never saw
- a (query, engine) cell's request differs from the one the pool recorded, or the pool records none, as after editing a template, a declared param or a query's params; the error names the cells (`q07/pg`). `--latency-only` skips this check
- the judgments leave pooled docs unjudged
- an engine ranks a doc with no grade within the pool depth (`meta.pool_depth`). With requests unchanged this happens only when the corpus changed or a rank tie broke differently; the error counts the docs and names the cells

`latest.json` also stays put, with exit 0, unless the run took every spec job at the spec's warmup, iterations and K, from the track's own `spec.yaml` and `spec.defaults.judgments`. Every reason a run is not the track's result is listed under `settings.incomplete` in its report.

Judgments resolution order:
1. `--judgments <strategy|path>` (CLI flag)
2. `spec.defaults.judgments` (per-track default; a relative path starts at the spec file's folder, like the spec's suite paths)
3. None → the run fails before measuring anything

A judgments file that does not exist fails the run the same way, as do judgments whose `pool_run_id` is not the current pool's `run_id`. `--latency-only` measures latency without judgments; its report is marked `settings.latency_only` and never becomes `latest.json`. A report's `provenance.unjudged_count` is the number of pooled docs the run found ungraded, not the judge's own count. `provenance.pool_run_id` and `provenance.judgments_run_id` name the pool and the judgments it scored against.

Flags:
- `--jobs pg,es` — run only the named job(s) from the spec (useful during development)
- `--k 3,5,10` — NDCG/P cut-off values; each must be positive, distinct and no deeper than the pool's `meta.pool_depth`, or the run fails before measuring. The spec loader holds `metrics.k_values` to the same rule against `defaults.pool_depth` when that is set
- `--warmup N`, `--iterations N` — override spec settings
- `--latency-only` — measure latency without judgments

Retrieval depth is the suite's own `limit` param (or ES `size`); there is no run flag for it. Every ranking is cut at the pool's `meta.pool_depth` before it is scored and stored, so MAP and bpref count only docs the pool could have judged, and the run files `bench export --format run` writes hold the rankings bench scored. A `--latency-only` run keeps the rankings whole.

Elapsed time is printed after the results table.

#### Measurement

Every engine is measured under the same rules, so a latency or a failure means the same thing whichever engine produced it.

Each execution, warmup and measured alike, runs under one deadline: `runs.query_timeout` in the spec, 30 s when unset, for every engine type. The runner sets it and no engine client carries a timeout of its own, so a postgres query that runs past it fails the same way an elasticsearch one does. Each report records it as `settings.query_timeout`. Storing the suite's search templates on an engine is bounded by the same deadline. `bench validate` checks each query under the same deadline. Elasticsearch calls made with no deadline, such as the corpus count, stop at the 30 s default.

```yaml
runs:
  warmup: 1
  iterations: 3
  query_timeout: 30s
```

`bench pool` runs a job's queries all at once, at most as many executions at a time as the job has engines, and they may all land on one postgres engine. Waiting for a pool connection counts against the deadline, so a postgres engine queues there only when the job has more engines than its `pool_max_conns` (pgx default: 4 or the CPU count, whichever is larger); otherwise a pool timeout means the query itself ran past `query_timeout`.

The clock covers sending the query and receiving the whole result. For postgres that means reading every row; for elasticsearch and the API it means reading the full response body. Decoding the result into document ids happens after the clock stops, on every engine.

One failed measured iteration fails the query with that iteration's error, so a timeout can never vanish from p95, p99 and max behind the iterations that succeeded. A failed warmup does not fail the query; it is logged with the query and the engine.

Every engine ships the same payload: document ids only. Postgres templates `SELECT id`, so elasticsearch templates set `"_source": false`. Without it each hit carries the full article and its 1024-dim embedding, and a `size: 100` search times shipping a hundred articles against postgres shipping a hundred ids. The executor reads the id from `hit._id`, which the indexer sets to the article id, and falls back to `_source.id` only when `_id` is not a UUID.

```json
{"query": {"multi_match": {"query": "{{terms}}", "fields": ["title^3", "description^2", "content"]}}, "size": 100, "_source": false, "track_total_hits": false}
```

Every engine in a job answers every query of its suite. If an engine has no block for one of them (its own, or the one its `queries_from` names), `bench validate`, `bench pool` and `bench run` reject the track when its suites load, before any template is stored or query runs, and name the query and the engine.

Measured elasticsearch templates should set `"track_total_hits": false` until its cost has been measured. By default elasticsearch counts matches up to 10,000 to fill `hits.total`, which delays its early termination of a top-k search, while postgres reports no total at all. Leaving it on makes elasticsearch do work postgres does not, by an amount nobody has sized yet. With it off, elasticsearch omits `hits.total` and `corpus_matches` is left empty for those queries.

#### Index provenance

Before the first query, `bench run` reads from each live engine how it indexed the corpus and records it under `environment.engines.<name>.index` in the report. It also fills `version`.

| Engine        | Recorded                                                                                                                                                                                                                                        |
|---------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| postgres      | `server_version`; the versions of `vector`, `pg_search`, `pg_textsearch`, `pg_trgm` and `fuzzystrmatch`, where installed; `indexdef` and `reloptions` of every index on `articles` and `article_embeddings`; the live value of each `connection_settings` entry |
| elasticsearch | the version from `GET /`; the concrete index the spec's `index` resolves to; its `settings.index.analysis`; analyzer, `index_options`, dims and similarity of every text and `dense_vector` field                                                      |

Provenance is metadata, so a failed read never fails the run: it prints a warning and the block carries `error` instead. An `api` engine cannot describe its index, so its block carries only that error. Reports written before provenance was recorded have no `index` block and still load.

`bench show report` prints one line per engine summarising the block. `bench diff` lists the provenance fields that differ between the two runs before the metric deltas. The analysis block is compared as one field and shown as a digest.

### `bench export [<name>] --format <F>`

| Format               | Output                  | Description                                                                          |
|----------------------|-------------------------|--------------------------------------------------------------------------------------|
| `qrels` (or `tsv`)   | `trec/qrels.<S>.tsv`    | TREC qrels TSV for `trec_eval`, R, pytrec_eval                                       |
| `html`               | `reports/<run_id>.html` | Self-contained HTML with sortable tables, SVG charts, significance table, provenance |
| `markdown` (or `md`) | `reports/<run_id>.md`   | GitHub-Flavored Markdown tables for thesis writing and PRs                           |
| `run`                | `reports/<run_id>.run/` | TREC run files, one per job and engine (`<job>.<engine>.run`), for `trec_eval`        |

Examples:
```bash
bench export tracks/global-news-dataset/fts_quality --format qrels
bench export tracks/global-news-dataset/fts_quality --format qrels --strategy claude-api
bench export tracks/global-news-dataset/fts_quality --format html
bench export tracks/global-news-dataset/fts_quality --format markdown
bench export tracks/global-news-dataset/fts_quality --format markdown --output /tmp/results.md
```

### `bench status [<name>]`

Prints a one-glance dashboard showing which artifacts exist, when they were last generated, and what the natural next step is — like `git status` for the pipeline.

### `bench diff [<name>]`

Compares the report `latest.json` points at with the newest earlier complete report: judged metrics, no failed queries and nothing in `settings.incomplete`. When no earlier report qualifies, it warns and falls back to the newest earlier one. It warns when the judge, relevance threshold or judgments file differ, lists the index provenance fields that changed, then shows per-engine metric deltas (NDCG, MAP, MRR, latency) and per-query NDCG regressions sorted by magnitude. NDCG is compared at the largest K both runs computed. Pass `--a` / `--b` to compare specific run IDs.

### `bench show <subcommand> [<name>|path]`

Pretty-prints a one-page summary of any artifact:

| Subcommand | Reads |
|-----------|-------|
| `show spec` | `spec.yaml` |
| `show pool` | `trec/pool.yaml` |
| `show judgments [--strategy S]` | `trec/annotations.<S>.yaml` |
| `show report` | `reports/latest.json` → actual report |

`bench report [<name>]` is a top-level shorthand for `bench show report`.

### `bench clean [<name>] [--keep N]`

Removes old JSON, HTML, and Markdown files from `reports/`, keeping the `--keep` most-recent (default 5). `latest.json` is never deleted, and neither is the JSON report it points at. Of the JSON reports, complete runs fill the remaining slots before incomplete ones (failed, partial or latency-only), so those go first. HTML and Markdown exports keep only the `--keep` newest, even the latest run's export. A deleted report takes its `reports/<run_id>.run/` run files with it.

```bash
bench clean tracks/global-news-dataset/fts_quality            # keep 5 most recent
bench clean tracks/global-news-dataset/fts_quality --keep 2
bench clean tracks/global-news-dataset/fts_quality --dry-run  # show what would be deleted
```

## Metrics

Metrics are computed per query, then averaged over the job's judged query set: every query with grades. A query an engine failed, or had no block for, scores 0 for that engine, as `trec_eval -c` does, so every engine in a job shares one denominator. Unjudged retrieved docs count as non-relevant.

| Metric   | Description                                                                                  |
|----------|----------------------------------------------------------------------------------------------|
| `NDCG@k` | Normalized Discounted Cumulative Gain, gain `2^rel-1`, over grades at or above `relevance_threshold` |
| `NDCG@k (gain rel)` | trec_eval's `ndcg_cut`: gain `rel`, every grade above 0, whatever `relevance_threshold` is |
| `P@k`    | Precision at k                                                                               |
| `R@k`    | Recall at k                                                                                  |
| `F1@k`   | Harmonic mean of P@k and R@k                                                                 |
| `MAP`    | Mean Average Precision                                                                       |
| `MRR`    | Mean Reciprocal Rank                                                                         |
| `Bpref`  | Binary preference, each relevant doc scoring `1 - min(nonrel above, R) / min(R, N)` as in trec_eval |

`Coverage@K` is the share of each engine's retrieved docs within the largest K, over the judged queries, that carry a grade. A low value means the pool missed what the engine returns and its scores undercount.

Significance is a two-tailed Wilcoxon signed-rank test per engine pair for NDCG@K, MAP and MRR over the judged query set. Differences are rounded to 1e-12 before zeros are dropped and ranks assigned. The p-value is exact for up to 50 untied pairs, otherwise the normal approximation with tie and continuity correction. They match `scipy.stats.wilcoxon(d, method="exact")` and `scipy.stats.wilcoxon(d, method="approx", correction=True)` respectively, on the rounded differences `d`; a plain `wilcoxon(d)` goes exact even with ties and skips the correction, so it differs. It needs ≥4 non-zero differences. p-values are Holm-adjusted across the job's engine pairs on each metric; reports keep the raw `P` next to `AdjustedP`, and `*` (p<0.05) / `**` (p<0.01) mark the adjusted value.

Latency (min/p50/p75/p90/p95/p99/max/mean/stddev) covers only the queries every engine in the job answered; `LatencyExcludedQueries` counts the rest.

Each report records every (query, engine) ranking, so `trec_eval` can reproduce its MAP, P@k and linear-gain NDCG:

```bash
bench export <track> --format qrels --strategy <S>
bench export <track> --format run
trec_eval -c -l <relevance_threshold> -m map -m P.5,10 -m ndcg_cut.5,10 trec/qrels.<S>.tsv reports/<run_id>.run/<job>.<engine>.run
```

The qrels must hold exactly the job's queries: `-c` averages over every qrels topic, so a topic the job never ran counts as a zero. Filter the qrels to the suite's query ids before running `trec_eval`. Engines return no scores, so run-file scores are derived from rank. `ndcg_cut` matches the `gain rel` column and ignores `-l`; the `2^rel-1` NDCG has no stock trec_eval measure.

## Artifacts

All artifacts are self-attesting. A report's `provenance.sources` block records the exact paths of the spec, suite, pool, and judgments files used — you can reconstruct any run from the report alone.

For per-track documentation, see `tracks/<name>/README.md`.

## Track organisation

A track is **any folder** holding `spec.yaml` + `suite.yaml` + `trec/`. Nothing
above it is enforced — `bench` never assumes a `tracks/` directory — so how you
group tracks is your call. This repo uses one folder per dataset, one track per
(dataset × IR paradigm):

| Track                                       | Paradigm            | Engines                                 |
|---------------------------------------------|---------------------|-----------------------------------------|
| `tracks/global-news-dataset/fts_quality`    | Full-text search    | pg-seq, pg-gin, paradedb, elasticsearch |
| `tracks/global-news-dataset/news_fuzzy`     | Fuzzy / approximate | pg_trgm, ES fuzziness                   |
| `tracks/global-news-dataset/news_semantic`  | Semantic / vector   | pgvector, ES dense_vector kNN           |
| `tracks/global-news-dataset/news_hybrid`    | Hybrid (RRF fusion) | pgvector+BM25, ES hybrid                |

This decomposition ensures that pools and judgments are paradigm-specific (different query types, different relevance criteria) and that statistical comparisons are between equivalent systems.

### Track resolution & the track root

A track arg is an ordinary filesystem path:

1. **Absolute** — used as-is.
2. **Relative** — resolved against the track root (below).
3. **Glob** — fans out across every track-shaped match; `validate`, `pool`, `judge`, `run`, and `status` run once per matched track.
4. **Omitted** — walks up from the current directory to the nearest track folder, so `cd`-ing into a track and running `bench status` works.

`--track-root <dir>`, or `BENCH_TRACK_ROOT`, is where relative paths start. It
defaults to the current directory, so from the repo root:

```bash
bench run tracks/global-news-dataset/fts_quality
bench run 'tracks/global-news-dataset/*'          # the whole dataset
```

Point it at a dataset folder and the same tracks get short names — it is pure
convenience, identical to `cd`-ing there:

```bash
export BENCH_TRACK_ROOT=tracks/global-news-dataset
bench run fts_quality
bench run '*'                                     # the whole dataset
```

When a path misses, the error names what it tried and, if a folder of that name
sits deeper, points at it — the usual sign the root is unset or too shallow.

Grouping is **explicit**: only a glob expands. A single path always means exactly one track — a directory of tracks never implicitly becomes a group. Glob mode forbids the single-track path overrides (`--spec`/`--pool`/`--output`). A per-track failure is logged and the run continues; the command exits non-zero listing the tracks that failed. Quote a glob so the shell doesn't expand it first.
