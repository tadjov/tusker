import json

import pyarrow.parquet as pq
import pytest
from conftest import article, run_script, sha256_of, write_canonical_package

pytestmark = pytest.mark.usefixtures("descriptors_match_the_schema")

ROWS = [
    article(
        "6f1c2b8e-0d5a-4a57-9a0e-1b2c3d4e5f60",
        "Markets rally",
        "Stocks rose on Monday.",
        "long body " * 50,
    ),
    article(
        "0a9b8c7d-6e5f-4a3b-8c2d-1e0f9a8b7c6d",
        "Storm hits coast",
        "",
        "another long body " * 50,
    ),
    article(
        "d4c3b2a1-9f8e-4d7c-b6a5-0f1e2d3c4b5a",
        "Election results",
        "Polls closed at eight.",
        "third body",
    ),
]


def only_package(output_root):
    (descriptor,) = output_root.glob("embeddings/*/*/datapackage.json")
    return descriptor


def slim_package(out):
    (descriptor,) = out.glob("*/slim.datapackage.json")
    return descriptor.parent


def test_embedding_the_slim_projection_reproduces_the_canonical_package(
    tmp_path, tiny_model_args
):
    canonical = write_canonical_package(tmp_path / "canonical", ROWS)
    slim = tmp_path / "payload"

    sliming = run_script("slim_corpus.py", canonical.descriptor, "--out", slim)
    run_script(
        "embed_corpus.py",
        canonical.descriptor,
        "--output-root",
        tmp_path / "direct",
        *tiny_model_args,
    )
    run_script(
        "embed_corpus.py",
        slim_package(slim) / "slim.datapackage.json",
        "--output-root",
        tmp_path / "slim",
        *tiny_model_args,
    )

    assert sliming.returncode == 0, sliming.stderr
    direct, projected = (
        only_package(tmp_path / "direct"),
        only_package(tmp_path / "slim"),
    )
    assert projected.parent.name == direct.parent.name
    assert (
        json.loads(projected.read_text())["canonical"]
        == json.loads(direct.read_text())["canonical"]
    )
    assert sha256_of(projected.parent / "embeddings.parquet") == sha256_of(
        direct.parent / "embeddings.parquet"
    )


def test_the_slim_payload_carries_only_what_the_embedder_reads(tmp_path):
    canonical = write_canonical_package(tmp_path / "canonical", ROWS, "jsonl")
    slim = tmp_path / "payload"

    run_script("slim_corpus.py", canonical.descriptor, "--out", slim)

    descriptor = json.loads((slim_package(slim) / "slim.datapackage.json").read_text())
    (resource,) = descriptor["resources"]
    table = pq.read_table(slim_package(slim) / resource["path"])
    assert table.column_names == ["id", "title", "description"]
    assert table.column("id").to_pylist() == [row["id"] for row in ROWS]
    assert descriptor["canonical"] == {
        "name": "cc-news",
        "version": "article-v2-0badcafe",
        "resourceHash": sha256_of(canonical.resource),
    }


def test_refuses_to_slim_a_canonical_resource_that_does_not_verify(tmp_path):
    canonical = write_canonical_package(tmp_path / "canonical", ROWS, "jsonl")
    canonical.resource.write_bytes(
        canonical.resource.read_bytes().replace(b"Markets", b"Murkets")
    )

    result = run_script(
        "slim_corpus.py", canonical.descriptor, "--out", tmp_path / "payload"
    )

    assert result.returncode != 0
    assert "hash" in result.stderr
    assert not (tmp_path / "payload").exists()


def test_the_slim_payload_uploads_whole_through_the_kaggle_cli(tmp_path):
    canonical = write_canonical_package(tmp_path / "canonical", ROWS)
    slim = tmp_path / "payload"

    run_script("slim_corpus.py", canonical.descriptor, "--out", slim)

    # `kaggle datasets create` never uploads files with these names.
    skipped_by_kaggle = {
        "datapackage.json",
        "dataset-metadata.json",
        "kernel-metadata.json",
    }
    uploaded = {path.name for path in slim_package(slim).iterdir()}
    assert uploaded == {"articles.parquet", "slim.datapackage.json"}
    assert not skipped_by_kaggle & uploaded


def test_reslimming_the_same_canonical_package_is_a_no_op(tmp_path):
    canonical = write_canonical_package(tmp_path / "canonical", ROWS)
    slim = tmp_path / "payload"
    run_script("slim_corpus.py", canonical.descriptor, "--out", slim)
    before = sha256_of(slim_package(slim) / "articles.parquet")

    result = run_script("slim_corpus.py", canonical.descriptor, "--out", slim)

    assert result.returncode == 0, result.stderr
    assert sha256_of(slim_package(slim) / "articles.parquet") == before


def test_the_slim_package_lands_in_version_v1_by_default(tmp_path):
    canonical = write_canonical_package(tmp_path / "canonical", ROWS)
    slim = tmp_path / "payload"

    run_script("slim_corpus.py", canonical.descriptor, "--out", slim)

    folder = slim_package(slim)
    assert folder.name == "v1"
    assert json.loads((folder / "slim.datapackage.json").read_text())["version"] == "v1"


def test_refuses_to_reslim_other_bytes_into_an_existing_version(tmp_path):
    slim = tmp_path / "payload"
    first = write_canonical_package(tmp_path / "first", ROWS)
    run_script("slim_corpus.py", first.descriptor, "--out", slim)
    before = sha256_of(slim_package(slim) / "articles.parquet")
    changed = write_canonical_package(
        tmp_path / "changed",
        [*ROWS, article("e5d4c3b2-a1f0-4e9d-8c7b-6a5f4e3d2c1b", "Late addition")],
    )

    result = run_script("slim_corpus.py", changed.descriptor, "--out", slim)

    assert result.returncode != 0
    assert "v1" in result.stderr
    assert before in result.stderr
    assert sha256_of(slim_package(slim) / "articles.parquet") == before


def test_the_slim_descriptor_names_the_producing_tool(tmp_path):
    canonical = write_canonical_package(tmp_path / "canonical", ROWS)
    slim = tmp_path / "payload"

    run_script("slim_corpus.py", canonical.descriptor, "--out", slim)

    descriptor = json.loads((slim_package(slim) / "slim.datapackage.json").read_text())
    assert descriptor["producedBy"] == {"tool": "slim_corpus.py"}
