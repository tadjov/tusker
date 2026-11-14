import json
from pathlib import Path

import numpy as np
import pyarrow.parquet as pq
import pytest
from conftest import (
    article,
    model_args,
    run_script,
    write_canonical_package,
)

pytestmark = pytest.mark.usefixtures("descriptors_match_the_schema")

SUITE = """\
schema_version: 1
id: cc-news-semantic
version: "1.0.0"
queries:
  - id: q-climate
    text: "  climate change "
    description: Coverage of climate change
    engines: {}
  - id: q-rates
    text: central bank interest rates
    description: Central banks setting interest rates
    engines: {}
"""


def write_suite(track: Path, text: str = SUITE) -> Path:
    track.mkdir(parents=True, exist_ok=True)
    suite = track / "suite.yaml"
    suite.write_text(text, encoding="utf-8")
    return suite


def query_vectors(track: Path) -> Path:
    return track / "trec" / "query-vectors" / "datapackage.json"


def read_query_vectors(descriptor: Path) -> dict[str, list]:
    return pq.read_table(descriptor.parent / "query-vectors.parquet").to_pydict()


def test_writes_each_query_with_the_text_the_runner_embeds(tmp_path, tiny_model_args):
    suite = write_suite(tmp_path / "track")

    result = run_script("embed_queries.py", suite, *tiny_model_args)

    assert result.returncode == 0, result.stderr
    rows = read_query_vectors(query_vectors(tmp_path / "track"))
    assert rows["query_id"] == ["q-climate", "q-rates"]
    assert rows["text"] == ["  climate change ", "central bank interest rates"]
    norms = np.linalg.norm(np.array(rows["embedding"], dtype=np.float32), axis=1)
    np.testing.assert_allclose(norms, 1.0, atol=1e-6)


def test_refuses_a_query_with_only_a_description(tmp_path, tiny_model_args):
    suite = write_suite(
        tmp_path / "track", SUITE.replace('    text: "  climate change "\n', "")
    )

    result = run_script("embed_queries.py", suite, *tiny_model_args)

    assert result.returncode != 0
    assert "q-climate" in result.stderr
    assert not query_vectors(tmp_path / "track").exists()


def test_descriptor_records_the_tag_query_recipe_and_suite_id(
    tmp_path, tiny_model, tiny_model_args
):
    suite = write_suite(tmp_path / "track")

    run_script("embed_queries.py", suite, *tiny_model_args, "--dim", "64")

    descriptor = json.loads(query_vectors(tmp_path / "track").read_text())
    assert descriptor["embedding"] == {
        "model": "qwen3-embedding:tiny",
        "hfModel": str(tiny_model),
        "revision": "test-rev",
        "tag": "qwen3-embedding:tiny@64",
        "dim": 64,
        "recipe": "query-instruct-v1",
        "pooling": "last_token",
        "normalized": True,
        "maxTokens": 512,
        "batchSize": 1,
        "dtype": "float32",
    }
    assert descriptor["suite"] == {"id": "cc-news-semantic"}


def test_query_instruct_v1_embeds_the_recipe_string_around_the_stripped_query(
    tmp_path, tiny_model_args
):
    suite = write_suite(tmp_path / "track")
    wrapped = "Instruct: Given a web search, retrieve all relevant news documents\nQuery:climate change"
    canonical = write_canonical_package(
        tmp_path / "canonical",
        [article("11111111-1111-4111-8111-111111111111", wrapped)],
    )

    run_script("embed_queries.py", suite, *tiny_model_args)
    run_script(
        "embed_corpus.py",
        canonical.descriptor,
        "--output-root",
        tmp_path / "out",
        *tiny_model_args,
        "--batch-size",
        "1",
    )

    query = read_query_vectors(query_vectors(tmp_path / "track"))["embedding"][0]
    (document_package,) = (tmp_path / "out").glob("embeddings/*/*/datapackage.json")
    document = (
        pq.read_table(document_package.parent / "embeddings.parquet")
        .column("embedding")
        .to_pylist()[0]
    )
    np.testing.assert_array_equal(
        np.array(query, dtype=np.float32), np.array(document, dtype=np.float32)
    )


def test_re_embedding_an_edited_suite_replaces_its_query_vectors(
    tmp_path, tiny_model_args
):
    suite = write_suite(tmp_path / "track")
    run_script("embed_queries.py", suite, *tiny_model_args)
    write_suite(tmp_path / "track", SUITE.replace("interest rates", "rate decision"))

    result = run_script("embed_queries.py", suite, *tiny_model_args)

    assert result.returncode == 0, result.stderr
    rows = read_query_vectors(query_vectors(tmp_path / "track"))
    assert rows["text"] == ["  climate change ", "central bank rate decision"]


def test_a_text_yaml_could_read_as_a_number_keeps_its_literal_text(
    tmp_path, tiny_model_args
):
    suite = write_suite(
        tmp_path / "track", SUITE.replace("central bank interest rates", "2024")
    )

    result = run_script("embed_queries.py", suite, *tiny_model_args)

    assert result.returncode == 0, result.stderr
    assert read_query_vectors(query_vectors(tmp_path / "track"))["text"][1] == "2024"


def test_refuses_to_publish_non_finite_query_vectors(tmp_path, overflowing_tiny_model):
    suite = write_suite(tmp_path / "track")

    result = run_script("embed_queries.py", suite, *model_args(overflowing_tiny_model))

    assert result.returncode != 0
    assert "q-climate" in result.stderr
    assert not query_vectors(tmp_path / "track").exists()


def test_query_vectors_carry_no_version(tmp_path, tiny_model_args):
    suite = write_suite(tmp_path / "track")

    run_script("embed_queries.py", suite, *tiny_model_args)

    assert "version" not in json.loads(query_vectors(tmp_path / "track").read_text())


def test_refuses_a_query_with_an_empty_text(tmp_path, tiny_model_args):
    suite = write_suite(
        tmp_path / "track", SUITE.replace('"  climate change "', '"  "')
    )

    result = run_script("embed_queries.py", suite, *tiny_model_args)

    assert result.returncode != 0
    assert "q-climate" in result.stderr
    assert not query_vectors(tmp_path / "track").exists()


def test_refuses_a_suite_that_repeats_a_query_id(tmp_path, tiny_model_args):
    suite = write_suite(
        tmp_path / "track", SUITE.replace("id: q-rates", "id: q-climate")
    )

    result = run_script("embed_queries.py", suite, *tiny_model_args)

    assert result.returncode != 0
    assert "q-climate" in result.stderr
    assert not query_vectors(tmp_path / "track").exists()


def test_descriptor_names_the_producing_tool(tmp_path, tiny_model_args):
    suite = write_suite(tmp_path / "track")

    run_script("embed_queries.py", suite, *tiny_model_args)

    assert json.loads(query_vectors(tmp_path / "track").read_text())["producedBy"] == {
        "tool": "embed_queries.py"
    }
