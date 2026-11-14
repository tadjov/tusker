#!/usr/bin/env bash
# Tusker search benchmark — canonical workflow (bench v1, track-first).
#
# A "track" is a self-contained folder under tracks/<dataset>/ holding spec.yaml,
# suite.yaml, generated pool/annotations/qrels in trec/, and reports/.
# The folder IS the track — no hidden state.

set -euo pipefail

# Relative track paths resolve under this root, so TRACK is just the folder name.
# Unset it and pass a full path (tracks/<ds>/<track>) if you prefer.
export BENCH_TRACK_ROOT="${BENCH_TRACK_ROOT:-tracks/global-news-dataset}"
TRACK="${TRACK:-fts_quality}"
# The model the LLM judge grades with; bench judge needs one named.
JUDGE_MODEL="${JUDGE_MODEL:-sonnet}"

# 0. Build
make build-bench

# 0a. (Optional) Scaffold a new track from templates.
# ./bin/bench init my_new_track

# 1. Sanity-check spec + suite ahead of real engine time.
./bin/bench validate "$TRACK"

# 2. Eyeball the configuration.
./bin/bench show spec "$TRACK"

# 3. Generate the candidate pool from all configured engines.
./bin/bench pool "$TRACK"
./bin/bench show pool "$TRACK"

# 4a. Grade with the deterministic keyword baseline (no LLM, no API).
./bin/bench judge "$TRACK" --strategy keyword

# 4b. ... OR with the Claude CLI, one doc per request (cheap: bills through your
#     Claude subscription), on the model JUDGE_MODEL names.
./bin/bench judge "$TRACK" --strategy llm --provider claude-cli --model "$JUDGE_MODEL" --resume

# 4c. ... OR with the Anthropic API in batches (set ANTHROPIC_API_KEY).
./bin/bench judge "$TRACK" --strategy llm --provider claude-api --model "$JUDGE_MODEL" --batch 20 --resume

# 4d. ... OR write -1 placeholders for hand grading.
./bin/bench judge "$TRACK" --strategy manual

# 5. Inspect grades before running the benchmark.
./bin/bench show judgments "$TRACK" --strategy keyword

# 6. Run the benchmark. Defaults to spec.defaults.judgments; override with
#    --judgments <name> to swap which judgment set scores this run.
./bin/bench run "$TRACK"
./bin/bench run "$TRACK" --judgments claude-cli

# 7. (Optional) Export TREC qrels for trec_eval / external tooling.
#    Judgment sets are addressed by name: keyword, claude-cli, claude-api, ...
./bin/bench export "$TRACK" --format qrels --strategy claude-cli
