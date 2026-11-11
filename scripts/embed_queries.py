#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "torch>=2.4",
#   "transformers==4.51.3",
#   "numpy>=1.26",
#   "pyarrow>=16",
#   "pyyaml>=6",
# ]
# ///
"""Embed a track's suite queries into trec/query-vectors/ beside the suite.

  uv run scripts/embed_queries.py tracks/<dataset>/<paradigm>/suite.yaml

With no argument it runs as a Kaggle kernel, importing the embed_corpus.py in its input.
"""

import argparse
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import pyarrow as pa
import pyarrow.parquet as pq
import yaml

KAGGLE_INPUT = Path("/kaggle/input")
KAGGLE_WORKING = Path("/kaggle/working")

try:
    import embed_corpus
except ModuleNotFoundError:
    # A kernel pushes one code file; embed_corpus.py rides in the input dataset.
    sys.path.extend(
        sorted({str(path.parent) for path in KAGGLE_INPUT.glob("**/embed_corpus.py")})
    )
    import embed_corpus


QUERY_RECIPE = "query-instruct-v1"
# One query per pass, so no vector depends on padding to its neighbours.
QUERY_BATCH_SIZE = 1
QUERY_TASK = "Given a web search, retrieve all relevant news documents"

QUERY_VECTORS_SCHEMA = pa.schema(
    [
        pa.field("query_id", pa.string()),
        pa.field("text", pa.string()),
        pa.field("embedding", pa.list_(pa.float32())),
    ]
)


@dataclass(frozen=True, slots=True)
class SuiteQuery:
    id: str
    text: str


@dataclass(frozen=True, slots=True)
class Suite:
    id: str
    queries: list[SuiteQuery]


def instruct_input(text: str) -> str:
    """The query-instruct-v1 recipe."""
    return f"Instruct: {QUERY_TASK}\nQuery:{text.strip()}"


def load_suite(path: Path) -> Suite:
    """BaseLoader keeps `yes` or `2024` as the text the runner sees."""
    try:
        document = yaml.load(path.read_text(encoding="utf-8"), Loader=yaml.BaseLoader)
    except (OSError, yaml.YAMLError) as error:
        raise embed_corpus.PackageError(f"read suite {path}: {error}") from error
    if not isinstance(document, dict):
        raise embed_corpus.PackageError(f"{path} is not a suite mapping")

    queries: list[SuiteQuery] = []
    for entry in document.get("queries") or []:
        if not isinstance(entry, dict):
            raise embed_corpus.PackageError(f"{path}: every query must be a mapping")
        query = SuiteQuery(id=entry.get("id") or "", text=entry.get("text") or "")
        check_query(query)
        queries.append(query)
    if not queries:
        raise embed_corpus.PackageError(f"{path} has no queries")
    ids = [query.id for query in queries]
    duplicates = sorted({query_id for query_id in ids if ids.count(query_id) > 1})
    if duplicates:
        raise embed_corpus.PackageError(f"{path} repeats query ids {duplicates}")
    return Suite(id=document.get("id", ""), queries=queries)


def check_query(query: SuiteQuery) -> None:
    if not query.id or not query.text.strip():
        raise embed_corpus.PackageError(
            f"query {query.id!r} needs an id and a non-empty text"
        )


def write_query_vectors(
    directory: Path, rows: pa.Table, descriptor: dict[str, Any]
) -> None:
    resource_path = directory / "query-vectors.parquet"
    pq.write_table(rows, resource_path, compression="zstd")
    resource = embed_corpus.describe_parquet_resource(
        resource_path, name="query-vectors"
    )
    embed_corpus.write_descriptor(directory, {**descriptor, "resources": [resource]})


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    # A stale flag such as --out must not match --output-root.
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
        allow_abbrev=False,
    )
    parser.add_argument("suite", nargs="?", type=Path, help="the track's suite.yaml")
    parser.add_argument(
        "--out", type=Path, help="default: <suite dir>/trec/query-vectors"
    )
    parser.add_argument(
        "--model",
        default=embed_corpus.QWEN_HF_MODEL,
        help="Hugging Face model id or local directory",
    )
    parser.add_argument(
        "--revision",
        help=f"model commit; required unless --model is {embed_corpus.QWEN_HF_MODEL}",
    )
    parser.add_argument(
        "--model-name",
        help=f"tag stem; required unless --model is {embed_corpus.QWEN_HF_MODEL}",
    )
    parser.add_argument(
        "--dim",
        type=embed_corpus.positive_int,
        help="keep the first N dimensions, then L2-normalise (default: the model's width)",
    )
    parser.add_argument(
        "--max-tokens",
        type=embed_corpus.positive_int,
        default=512,
        help="input tokens per query; the rest is cut",
    )
    parser.add_argument(
        "--device",
        default="auto",
        choices=["auto", "cuda", "cpu", "mps"],
        help="auto never picks mps",
    )
    return parser.parse_args(argv)


def kaggle_suite() -> Path:
    matches = (
        sorted(KAGGLE_INPUT.glob("**/suite.yaml")) if KAGGLE_INPUT.is_dir() else []
    )
    if len(matches) != 1:
        raise embed_corpus.PackageError(
            f"no suite argument, and {len(matches)} suite.yaml files under {KAGGLE_INPUT}"
        )
    return matches[0]


def run(args: argparse.Namespace) -> Path:
    if args.suite is None:
        suite_path, default_out = kaggle_suite(), KAGGLE_WORKING / "query-vectors"
    else:
        suite_path, default_out = (
            args.suite,
            args.suite.parent / "trec" / "query-vectors",
        )
    out: Path = args.out or default_out
    revision, model_name = embed_corpus.resolve_model_identity(args)
    suite = load_suite(suite_path)

    device = embed_corpus.pick_device(args.device)
    encoder = embed_corpus.load_encoder(
        hf_model=args.model, revision=revision, device=device
    )
    spec = embed_corpus.EmbeddingSpec(
        hf_model=args.model,
        model_name=model_name,
        revision=revision,
        recipe=QUERY_RECIPE,
        dim=embed_corpus.resolve_dim(args.dim, encoder.native_dim),
        max_tokens=args.max_tokens,
    )
    embedding = embed_corpus.embedding_descriptor(
        spec,
        embed_corpus.Inference(
            batch_size=QUERY_BATCH_SIZE, dtype=embed_corpus.dtype_name(device)
        ),
    )
    vectors = embed_corpus.encode(
        encoder,
        [instruct_input(query.text) for query in suite.queries],
        batch_size=QUERY_BATCH_SIZE,
        max_tokens=spec.max_tokens,
        dim=spec.dim,
    )
    broken = embed_corpus.non_finite_ids([query.id for query in suite.queries], vectors)
    if broken:
        raise embed_corpus.PackageError(
            f"queries {broken} embedded to non-finite vectors"
        )
    rows = pa.Table.from_arrays(
        [
            pa.array([query.id for query in suite.queries], type=pa.string()),
            pa.array([query.text for query in suite.queries], type=pa.string()),
            embed_corpus.vector_column(vectors),
        ],
        schema=QUERY_VECTORS_SCHEMA,
    )
    descriptor: dict[str, Any] = {
        "$schema": embed_corpus.DATAPACKAGE_PROFILE,
        "name": f"{suite.id}-query-vectors",
        "producedBy": {"tool": "embed_queries.py"},
        "embedding": embedding,
        "suite": {"id": suite.id},
    }
    embed_corpus.publish_replacing(
        out, lambda directory: write_query_vectors(directory, rows, descriptor)
    )
    print(f"wrote {len(suite.queries)} query vectors to {out}")
    return out


def main() -> None:
    try:
        run(parse_args())
    except embed_corpus.PackageError as error:
        sys.exit(f"embed_queries: {error}")


if __name__ == "__main__":
    main()
