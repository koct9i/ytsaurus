"""Table preparation commands for map operation performance benchmark.

Typical workflow
----------------

1. Generate a source table filled with synthetic data::

       python -m yt.benchmarks.map_perf prepare \\
           --proxy <cluster> \\
           --dst //home/user/map-bench/source \\
           --row-count 10000000 \\
           --payload-size 100 \\
           --job-count 100

   Equivalent YT CLI commands::

       # Create helper input table (one row per map job)
       yt create table //home/user/map-bench/fake-input --proxy <cluster>
       yt write-table //home/user/map-bench/fake-input --format yson --proxy <cluster> \\
           <<< '[{job_index=0};{job_index=1};...;{job_index=99}]'

       # Create destination table with benchmark schema
       yt create table //home/user/map-bench/source --proxy <cluster> \\
           --attributes '{schema=[
               {name=key;type=int64;sort_order=ascending};
               {name=str_value;type=string};
               {name=int_value;type=int64};
               {name=float_value;type=double}
           ];optimize_for=scan}'

       # Populate destination via map (each job generates rows_per_job rows)
       yt map 'python row_generator.py' \\
           --src //home/user/map-bench/fake-input \\
           --dst //home/user/map-bench/source \\
           --format yson \\
           --spec '{"job_count": 100}' \\
           --proxy <cluster>

2. Create format variant tables from the source::

       python -m yt.benchmarks.map_perf variants \\
           --proxy <cluster> \\
           --src //home/user/map-bench/source \\
           --dst-prefix //home/user/map-bench/variants \\
           --medium ssd_blobs

   Equivalent YT CLI commands (repeated for each optimize_for x codec combination)::

       yt merge --mode ordered \\
           --src //home/user/map-bench/source \\
           --dst '<optimize_for=scan;compression_codec=lz4>//home/user/map-bench/variants/scan-lz4' \\
           --spec '{"force_transform": true}' \\
           --proxy <cluster>

       yt merge --mode ordered \\
           --src //home/user/map-bench/source \\
           --dst '<optimize_for=lookup;compression_codec=zstd_3>//home/user/map-bench/variants/lookup-zstd_3' \\
           --spec '{"force_transform": true}' \\
           --proxy <cluster>

       # ... and so on for every (optimize_for, compression_codec) pair
"""

import time

import click
import yt.wrapper as yt

################################################################################

BENCHMARK_SCHEMA = [
    {"name": "key", "type": "int64", "sort_order": "ascending"},
    {"name": "str_value", "type": "string"},
    {"name": "int_value", "type": "int64"},
    {"name": "float_value", "type": "double"},
]

DEFAULT_OPTIMIZE_FOR = ["lookup", "scan"]
DEFAULT_COMPRESSION_CODECS = ["none", "lz4", "snappy", "zstd_3"]

TMP_SUBDIR = "/map-bench"

################################################################################


class RowGeneratorMapper:
    """Generates synthetic benchmark rows.

    Each input row is expected to carry a ``job_index`` field.  The mapper
    yields ``rows_per_job`` output rows whose ``key`` values start at
    ``job_index * rows_per_job``, guaranteeing globally unique keys when
    each map job processes exactly one input row.
    """

    def __init__(self, rows_per_job, payload_size):
        self.rows_per_job = rows_per_job
        self.payload_size = payload_size

    def __call__(self, row):
        job_index = row.get("job_index", 0)
        base_key = job_index * self.rows_per_job
        payload = "a" * self.payload_size
        for i in range(self.rows_per_job):
            yield {
                "key": base_key + i,
                "str_value": payload,
                "int_value": base_key + i,
                "float_value": float(i) / max(self.rows_per_job, 1),
            }


################################################################################


@click.command("prepare")
@click.option("--proxy", envvar="YT_PROXY", required=True,
              help="YT cluster proxy address.")
@click.option("--dst", required=True,
              help="Destination path for the generated source table.")
@click.option("--row-count", default=10_000_000, show_default=True,
              help="Total number of rows to generate.")
@click.option("--payload-size", default=100, show_default=True,
              help="Length of the string payload in each row (bytes).")
@click.option("--job-count", default=100, show_default=True,
              help="Number of parallel map jobs used for data generation.")
@click.option("--medium", default="default", show_default=True,
              help="Primary storage medium (e.g. default, ssd_blobs, nvme_blobs).")
def prepare(proxy, dst, row_count, payload_size, job_count, medium):
    """Generate a source table for the map operation performance benchmark.

    Creates a sorted table with a fixed schema and fills it with synthetic
    data using a map operation.  The resulting table serves as the input for
    the ``variants`` command.

    \b
    Schema:
      key        int64  (ascending sort key)
      str_value  string
      int_value  int64
      float_value double
    """
    client = yt.YtClient(proxy=proxy)
    rows_per_job = max(1, row_count // job_count)
    actual_row_count = rows_per_job * job_count

    tmp_dir = client.config["remote_temp_tables_directory"] + TMP_SUBDIR
    client.mkdir(tmp_dir, recursive=True)

    click.echo(
        "Generating {:,} rows ({} jobs × {:,} rows/job)…".format(
            actual_row_count, job_count, rows_per_job
        )
    )
    click.echo("Destination : {}".format(dst))
    click.echo("Medium      : {}".format(medium))

    table_attrs = {"schema": BENCHMARK_SCHEMA, "optimize_for": "scan"}
    if medium != "default":
        table_attrs["primary_medium"] = medium
    client.create("table", dst, recursive=True, force=True, attributes=table_attrs)

    with client.TempTable(tmp_dir) as fake_input:
        client.write_table(fake_input, [{"job_index": i} for i in range(job_count)])

        mapper = RowGeneratorMapper(rows_per_job=rows_per_job, payload_size=payload_size)
        t0 = time.monotonic()
        client.run_map(
            mapper,
            source_table=fake_input,
            destination_table=dst,
            format=yt.YsonFormat(),
            spec={
                "job_count": job_count,
                "title": "map-bench: generate source data",
            },
            sync=True,
        )
        elapsed = time.monotonic() - t0

    attrs = client.get(dst + "/@")
    click.echo(
        "Done in {:.1f}s.  Rows: {:,},  Size: {:.1f} MB".format(
            elapsed,
            int(attrs["row_count"]),
            int(attrs["uncompressed_data_size"]) / 1024 / 1024,
        )
    )


################################################################################


@click.command("variants")
@click.option("--proxy", envvar="YT_PROXY", required=True,
              help="YT cluster proxy address.")
@click.option("--src", required=True,
              help="Source table (created by the ``prepare`` command).")
@click.option("--dst-prefix", required=True,
              help="Destination directory; one sub-table is created per variant.")
@click.option("--medium", default="default", show_default=True,
              help="Primary storage medium for all variant tables.")
@click.option("--optimize-for", "optimize_for_list", multiple=True,
              default=DEFAULT_OPTIMIZE_FOR, show_default=True,
              help="optimize_for values to include.  Repeat to add more.")
@click.option("--compression-codec", "compression_codec_list", multiple=True,
              default=DEFAULT_COMPRESSION_CODECS, show_default=True,
              help="Compression codecs to include.  Repeat to add more.")
def variants(proxy, src, dst_prefix, medium, optimize_for_list, compression_codec_list):
    """Create format variant tables from a source benchmark table.

    For every (optimize_for, compression_codec) pair an ordered merge with
    ``force_transform=true`` rewrites the source table in the target storage
    format.  All variant paths are printed on completion.

    \b
    Equivalent YT CLI command for each variant:
      yt merge --mode ordered \\
          --src <src> \\
          --dst '<optimize_for=scan;compression_codec=lz4><dst_prefix>/scan-lz4' \\
          --spec '{"force_transform": true}'
    """
    client = yt.YtClient(proxy=proxy)
    client.mkdir(dst_prefix, recursive=True)

    total = len(optimize_for_list) * len(compression_codec_list)
    idx = 0

    for optimize_for in optimize_for_list:
        for compression_codec in compression_codec_list:
            idx += 1
            variant_name = "{}-{}".format(optimize_for, compression_codec)
            dst = "{}/{}".format(dst_prefix, variant_name)

            click.echo("[{}/{}] Creating variant: {}…".format(idx, total, variant_name))

            path_attrs = {
                "optimize_for": optimize_for,
                "compression_codec": compression_codec,
            }
            if medium != "default":
                path_attrs["primary_medium"] = medium

            t0 = time.monotonic()
            client.run_merge(
                client.TablePath(src),
                client.TablePath(dst, **path_attrs),
                mode="ordered",
                spec={
                    "force_transform": True,
                    "title": "map-bench: create variant {}".format(variant_name),
                },
                sync=True,
            )
            elapsed = time.monotonic() - t0

            attrs = client.get(dst + "/@")
            click.echo(
                "  done in {:.1f}s  rows={:,}  compressed={:.1f} MB  ratio={:.2f}".format(
                    elapsed,
                    int(attrs["row_count"]),
                    int(attrs["compressed_data_size"]) / 1024 / 1024,
                    float(attrs["compression_ratio"]),
                )
            )

    click.echo("\nCreated {} variant(s) under {}.".format(total, dst_prefix))
