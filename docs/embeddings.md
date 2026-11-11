# Embeddings

Document embeddings power the semantic and hybrid search tracks. They are
precomputed offline, on a GPU, into an embeddings package built from one
canonical package, and `datapipe load` loads both packages together. Embedding
generation is a one-time, GPU-bound job, so nothing embeds documents during a
load.

## Offline workflow

```
datasets/<ds>/canonical/<version>/      canonical package (preprocess)
  → slim_corpus.py                      id, title, description only; the cloud upload
  → embed_corpus.py on a cloud GPU      Kaggle or Colab; Qwen3-Embedding-0.6B, lead-v1
  → datasets/<ds>/embeddings/<tag-dir>/<version>/
  → datapipe load --canonical … --embeddings …   → articles + article_embeddings
```

The embeddings package names the canonical resource it was built from
(`canonical.resourceHash`), and `datapipe load` refuses a pair that does not
match before it writes anything. Within the load every vector must belong to
the article at the same position, so a vector without its article fails the
load instead of being skipped.

GPU work runs on Kaggle or Colab, never on the laptop. `--device auto` resolves to CUDA
or CPU and never to Apple's MPS.

### Recipes

| Recipe              | Model input                                                                                     | Used for  |
|---------------------|-------------------------------------------------------------------------------------------------|-----------|
| `lead-v1`           | `" ".join` of the stripped, non-empty `title` and `description`. Never the body.                | documents |
| `query-instruct-v1` | `"Instruct: Given a web search, retrieve all relevant news documents\nQuery:" + strip(query)`   | queries   |

There is no space after `Query:`.

Both recipes use `Qwen/Qwen3-Embedding-0.6B` pinned to revision
`97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3`, last-token pooling, then a slice to
`--dim` and L2 normalisation. `--dim` defaults to the model's native 1024.
`--max-tokens` (default 512) caps the input tokens and has nothing to do with
the width. The tag carries the width: `qwen3-embedding:0.6b@1024`.

A row with neither a title nor a description is refused before the model loads.
Loaders pair vectors with articles by position, so no row can be skipped. In
cc-news 180,295 of 707,910 canonical rows (25.5%) have no description and embed
as title only. Ids have to be canonical lowercase uuid text, because loaders
compare them byte for byte; anything else is refused rather than rewritten.

A shard or a query whose vector comes out NaN or inf, which fp16 overflow does
silently, is refused before it is written, with the shard number and the first
ids.

### The lead

News is written as an inverted pyramid: the headline and the first lines say
who, what, where and when, and the body adds detail in falling order of
importance. Journalists call that opening the lead. `lead-v1` embeds it as the
article's `title` and `description`, the summary the publisher puts in the
page's metadata, usually the teaser or the first sentence. The body is never
embedded: the lead says what the article is about, and the 512-token input cap
would cut the longer half of cc-news bodies partway anyway.

In cc-news, 180,295 of 707,910 articles have no description and are embedded
from the headline alone. About 24,500 more carry a site-wide description, such
as a publisher's slogan or section list, repeated across at least 20 articles.

The recipe's version is part of a vector's identity, with the model and the
width. A different lead formula gets a new recipe name, a new embeddings package
and a new run.

### The embeddings package

```
datasets/<ds>/embeddings/qwen3-embedding-0.6b@1024/v1/
  datapackage.json
  embeddings.parquet        id (string uuid), embedding (list<float32>), canonical row order
```

The descriptor is a Frictionless Data Package v2:

```json
{
  "$schema": "https://datapackage.org/profiles/2.0/datapackage.json",
  "name": "cc-news-embeddings",
  "version": "v1",
  "producedBy": {"tool": "embed_corpus.py"},
  "language": "eng",
  "embedding": {
    "model": "qwen3-embedding:0.6b",
    "hfModel": "Qwen/Qwen3-Embedding-0.6B",
    "revision": "97b0c614be4d77ee51c0cef4e5f07c00f9eb65b3",
    "tag": "qwen3-embedding:0.6b@1024",
    "dim": 1024,
    "recipe": "lead-v1",
    "pooling": "last_token",
    "normalized": true,
    "maxTokens": 512,
    "batchSize": 64,
    "dtype": "float16"
  },
  "canonical": {"name": "cc-news", "version": "v1", "resourceHash": "sha256:<hex>"},
  "resources": [
    {"name": "embeddings", "path": "embeddings.parquet", "format": "parquet",
     "mediatype": "application/vnd.apache.parquet", "bytes": 0, "hash": "sha256:<hex>"}
  ]
}
```

`batchSize` is the batch size the run used, including one `--auto-batch-size`
picked, and `dtype` the inference dtype (`float16` on a GPU, `float32` on CPU).
Both move the last bits of a float, so they are recorded. A `--limit N` trial
also records `limit`.

The version is a name you choose with `--version`, `v1` by default, and it is the
folder the package lands in. `canonical.resourceHash` names the canonical file the
vectors were made from, and the input package's resource is checked against its
`bytes` and `hash` before anything is embedded.

### Shrink the upload first

`embed_corpus.py` reads only `id`, `title` and `description`. The article body is
most of the bytes, so project it away before moving anything to a cloud box:

```bash
uv run scripts/slim_corpus.py datasets/cc-news/canonical/<version>/datapackage.json --out /tmp/tusker-payload
# writes /tmp/tusker-payload/v1/; --version picks another name
```

The output is a package of its own in `<out>/<version>/`: `articles.parquet`
plus a `slim.datapackage.json` whose `canonical` property names the package it
came from. The descriptor isn't called `datapackage.json`
because the kaggle CLI never uploads a file by that name. `embed_corpus.py`
takes the canonical reference from there, so embedding the slim package gives
the same bytes as embedding the canonical one. The output
stays plain parquet, since Kaggle decompresses `.gz` uploads and that would break
the recorded hash.

### Kaggle script kernel

`kaggle kernels push` uploads exactly one `code_file`, which is `embed_corpus.py`
itself. The kernel config lives at `datasets/<corpus>/kernel-metadata.json`. The
kernel slug and its `dataset_sources` are per corpus, and `code_file` points back
at the shared script.

Kernels run with no argv. Without a package argument the script reads the one
`slim.datapackage.json` under `/kaggle/input`, writes to `/kaggle/working`, removes
checkpoints after the package is written, and sweeps the batch size.

**0. Verify your phone** at kaggle.com/settings. The CLI sends `enable_gpu` and
`enable_internet`, but the server drops both on unverified accounts, silently. You
get a CPU box with no network and the run dies fetching the model. This is the
first thing to check when a kernel logs `device : cpu`.

**1. Install and authenticate.**

```bash
pip install kaggle          # token from kaggle.com/settings/api -> ~/.kaggle/kaggle.json
```

**2. Upload the slim package as a dataset.**

```bash
uv run scripts/slim_corpus.py datasets/cc-news/canonical/<version>/datapackage.json --out /tmp/tusker-payload
PAYLOAD=/tmp/tusker-payload/v1
kaggle datasets init -p "$PAYLOAD"
# edit $PAYLOAD/dataset-metadata.json: set the title, and the id that
# datasets/cc-news/kernel-metadata.json lists under dataset_sources
kaggle datasets version -p "$PAYLOAD" -m "slim v1 of canonical <version>"
```

`version` adds a new version to `djordjevuckovic/tusker-cc-news`, which the
kernel already reads. `kaggle datasets create` fails on a dataset that exists;
use it only for a new dataset, then point `dataset_sources` at that id.

**3. Push the kernel.** Set your username in
`datasets/cc-news/kernel-metadata.json` first. Pushing also starts the run.

```bash
kaggle kernels push -p datasets/cc-news
```

**4. Watch it.**

```bash
kaggle kernels status djordjevuckovic/tusker-embed
kaggle kernels logs   djordjevuckovic/tusker-embed
```

**5. Collect the package.** Only `/kaggle/working` is downloadable. Fetch it to a
scratch dir, then move the version directory in; nothing in `datasets/` is
overwritten.

```bash
kaggle kernels output djordjevuckovic/tusker-embed -p /tmp/tusker-out
mkdir -p datasets/cc-news/embeddings/qwen3-embedding-0.6b@1024
mv /tmp/tusker-out/embeddings/qwen3-embedding-0.6b@1024/v1 \
   datasets/cc-news/embeddings/qwen3-embedding-0.6b@1024/
```

The kernel relies on the Kaggle image's preinstalled transformers, which has to
be at least 4.51 for Qwen3, since a script kernel has no `pip install` step. The
scripts stay on Python 3.11 syntax for the same image.

### Colab notebook

`scripts/embed_colab.ipynb` is the other GPU path. It drives the same scripts from
Drive, for when Kaggle's GPU quota is spent:

1. Copy `scripts/embed_corpus.py` and `scripts/embed_queries.py` to
   `MyDrive/tusker/`, and the slim package folder's two files
   (`slim.datapackage.json` and `articles.parquet`) to
   `MyDrive/tusker/<corpus>/payload/`. The full canonical package works too.
2. Open the notebook in Colab with a GPU runtime, set `DATASET` in its settings
   cell, and run the cells in order.
3. The run cell calls `embed_corpus.py` with `--device cuda`, `--dim`,
   `--max-tokens` and checkpoints in `MyDrive/tusker/<corpus>/checkpoints`. After
   a disconnect, re-running the cell resumes from the finished shards.
4. The package lands at `MyDrive/tusker/<corpus>/embeddings/<tag-dir>/<version>/`.
   Copy that version directory into `datasets/<corpus>/embeddings/<tag-dir>/`.

The notebook can't run locally. `scripts/tests/test_colab_notebook.py` checks that
every script call in it parses against the scripts' CLIs, which have abbreviations
turned off so a stale flag fails instead of matching a longer one.

### Query vectors

A track's queries are embedded with the same runtime into
`tracks/<ds>/<paradigm>/trec/query-vectors/`:

```
datapackage.json          embedding {…, recipe: query-instruct-v1}, suite {id}
query-vectors.parquet     query_id, text, embedding; suite order
```

`text` is the suite query's `text` exactly as the bench runner passes it to the
vector store, in `internal/bench/runner/runner.go`:

```go
vec, err := r.config.VectorStore.QueryVector(ctx, q.Text)
```

`bench validate` and the vector and hybrid judges look vectors up by the same
string, so engine and judge see the same query. A query's `description` is never
embedded. The model input is that text wrapped by `query-instruct-v1`. Queries are embedded
one per forward pass, so no query's vector depends on the others in the suite.
The bench refuses query vectors when a suite query is missing from them or its
stored `text` differs from the suite's current text. The package has no version:
the folder is fixed per track, so re-embedding an edited suite replaces it.

On Kaggle, push `scripts/kaggle/queries/kernel-metadata.json`. Its dataset holds
the track's `suite.yaml` and a copy of `scripts/embed_corpus.py`, which
`embed_queries.py` imports:

```bash
mkdir -p /tmp/tusker-queries
cp tracks/cc-news/<paradigm>/suite.yaml scripts/embed_corpus.py /tmp/tusker-queries/
kaggle datasets init -p /tmp/tusker-queries        # set title and id
kaggle datasets create -p /tmp/tusker-queries      # first time; later runs use `kaggle datasets version`
kaggle kernels push -p scripts/kaggle/queries
kaggle kernels output djordjevuckovic/tusker-embed-queries -p /tmp/tusker-queries-out
mv /tmp/tusker-queries-out/query-vectors tracks/cc-news/<paradigm>/trec/
```

### Checkpoints and re-runs

The corpus is streamed and checkpointed every `--shard-size` rows (default
20,000). Shards are cut in canonical row order, so shard N always covers the same
rows. Length sorting happens inside a shard and is undone before the vectors are
saved, which cuts padding waste (2.2x on an M4 Pro) without changing the order.

- The checkpoint dir records the run's settings: input hash, canonical hash,
  model, revision, recipe, dim, maxTokens, shard size, batch size, dtype and
  `--limit`. A run with any other setting is refused instead of mixing shards.
- Default checkpoints live in `embeddings/<tag-dir>/.checkpoints-<digest>/`, a
  digest of the input and the settings other than batch size and dtype. A resumed run
  finds them, `--auto-batch-size` reuses the batch size they recorded, and a run
  with another batch size is refused by the manifest.
- A package is built in a staging folder and renamed to `<version>/`. When that
  folder already exists, its resource hash decides: the same hash keeps it and
  drops the staging copy, a different hash is an error naming the version and
  both hashes, and nothing is replaced. `embed_corpus.py` warns before embedding
  when the version exists, and on a conflict keeps the new package at
  `.<version>.rejected-<hash>` beside it, so a finished GPU run isn't lost; move
  it to a new version folder and set its descriptor's `version` to match.
  `slim_corpus.py` follows the same rule.
- Another batch size or dtype writes slightly different bytes, so it needs its
  own `--version`.
- `--limit N` embeds the first N rows, marked by `embedding.limit`. It writes
  other bytes than the full run, so give it its own version, e.g.
  `--version v1-trial`; under the full run's version it is refused.
- `--clean-checkpoints` drops the shards once the package is written. The merge
  still runs with every shard on disk, so peak disk stays at shards plus Parquet,
  about 5.8 GB for cc-news; only what is left afterwards halves, to 2.9 GB.

`--batch-size` defaults to 64. Wider batches have measured slower on every device
tried so far: on an M4 Pro 64 beat 128 and 256 fell off a cliff, and on a Colab
CUDA GPU the fall-off was monotonic (64 at 199 docs/s, 256 at 158, 512 at 128).
The length sort is why: more, smaller batches pad less than a few wide ones.
`--auto-batch-size` times a 2,000-row slice at 64, 256 and 512 and uses the
fastest. A resumed run reuses the batch size its checkpoints recorded.

### Testing the scripts

The tests build a random one-layer Qwen3 at width 1024 on the fly, run every
script as a CLI on CPU, and never touch the network:

```bash
uv run --no-project --python 3.11 --with-requirements scripts/tests/requirements.txt pytest scripts/tests
```

### Running

```bash
ENV_PATHS=cmd/datapipe/pg.env go run ./cmd/datapipe load --engine native \
  --canonical datasets/cc-news/canonical/<version>/datapackage.json \
  --embeddings datasets/cc-news/embeddings/<tag-dir>/<version>/datapackage.json
```

The target must be empty: `load` refuses a database that already holds a corpus
or its indexes, so drop the schema first (`make migrate-down-native`). An
embeddings package from a `--limit` trial loads only with `--trial`. Descriptors
may also live at `https://…` or `s3://bucket/key` (credentials from
`DATASET_S3_*`); remote resources are downloaded to `$TMPDIR` and verified
before the load starts, so it needs room for the vectors, about 2.9 GB for
cc-news.

The descriptor's `embedding.tag` is stored as
`article_embeddings.model_name`, so document-side and query-side embeddings
agree on the model.
