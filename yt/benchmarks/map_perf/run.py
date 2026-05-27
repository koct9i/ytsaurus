"""Map operation benchmark runner.

Runs ``yt map 'cat > /dev/null'`` over every variant table produced by the
``variants`` command and measures wall time and throughput for each combination
of storage format and job input format.

Typical usage
-------------

::

    python -m yt.benchmarks.map_perf run \\
        --proxy <cluster> \\
        --variants-prefix //home/user/map-bench/variants \\
        --output-format table

Equivalent YT CLI command for each (variant, job-format) pair::

    yt map 'cat > /dev/null' \\
        --src //home/user/map-bench/variants/scan-lz4 \\
        --dst //tmp/map-bench/devnull-<timestamp> \\
        --format yson \\
        --proxy <cluster>

Supported job formats
---------------------

yson
    Default YTsaurus row format.  Human-readable, moderate parsing overhead.

json
    Standard JSON.  Broadly portable but slower to parse than YSON.

arrow
    Apache Arrow IPC streaming format (columnar).  Can be significantly faster
    for wide tables when processing column subsets, but requires the cluster
    and job environment to support Arrow output.
"""

import csv
import json
import sys
import time

import click
import yt.wrapper as yt

################################################################################

_FORMAT_FACTORIES = {
    "yson": yt.YsonFormat,
    "json": yt.JsonFormat,
}

# Arrow support is optional – include it only when the wrapper exposes it.
try:
    _FORMAT_FACTORIES["arrow"] = yt.ArrowFormat
except AttributeError:
    pass

_ALL_JOB_FORMATS = list(_FORMAT_FACTORIES.keys())

_RESULT_FIELDS = [
    "variant",
    "optimize_for",
    "compression_codec",
    "job_format",
    "wall_time_sec",
    "rows",
    "rows_per_sec",
    "mb_per_sec",
    "compression_ratio",
]

TMP_SUBDIR = "/map-bench"

################################################################################


def _run_single(client, variant_path, job_format_name, tmp_dir):
    """Run one (variant, job_format) benchmark and return a metrics dict."""
    attrs = client.get(variant_path + "/@")
    row_count = int(attrs["row_count"])
    uncompressed_size = int(attrs["uncompressed_data_size"])
    compression_ratio = float(attrs["compression_ratio"])
    optimize_for = str(attrs["optimize_for"])
    compression_codec = str(attrs["compression_codec"])

    fmt_cls = _FORMAT_FACTORIES[job_format_name]

    with client.TempTable(tmp_dir) as devnull:
        t0 = time.monotonic()
        client.run_map(
            "cat > /dev/null",
            source_table=variant_path,
            destination_table=devnull,
            format=fmt_cls(),
            spec={"title": "map-bench: {} fmt={}".format(variant_path, job_format_name)},
            sync=True,
        )
        wall_time = time.monotonic() - t0

    rows_per_sec = row_count / wall_time if wall_time > 0 else 0
    mb_per_sec = uncompressed_size / (1024 * 1024) / wall_time if wall_time > 0 else 0

    return {
        "optimize_for": optimize_for,
        "compression_codec": compression_codec,
        "job_format": job_format_name,
        "wall_time_sec": round(wall_time, 2),
        "rows": row_count,
        "rows_per_sec": int(round(rows_per_sec)),
        "mb_per_sec": round(mb_per_sec, 1),
        "compression_ratio": round(compression_ratio, 2),
    }


def _print_table(results):
    """Render results as a plain-text ASCII table."""
    col_w = (30, 8, 10, 20, 10, 12, 10, 7)
    fmt = " ".join("{{:<{}}}".format(w) for w in col_w)
    header = fmt.format(
        "variant", "fmt", "optimize", "codec",
        "time(s)", "rows/s", "MB/s", "ratio",
    )
    sep = "-" * len(header)
    click.echo(sep)
    click.echo(header)
    click.echo(sep)
    for r in results:
        if "error" in r:
            click.echo(fmt.format(
                r.get("variant", "?")[:30],
                r.get("job_format", "?"),
                "ERROR", str(r["error"])[:20],
                "", "", "", "",
            ))
        else:
            click.echo(fmt.format(
                r.get("variant", "")[:30],
                r.get("job_format", ""),
                r.get("optimize_for", ""),
                r.get("compression_codec", ""),
                "{:.1f}".format(r.get("wall_time_sec", 0)),
                "{:,}".format(r.get("rows_per_sec", 0)),
                "{:.1f}".format(r.get("mb_per_sec", 0)),
                "{:.2f}".format(r.get("compression_ratio", 0)),
            ))
    click.echo(sep)


################################################################################


@click.command("run")
@click.option("--proxy", envvar="YT_PROXY", required=True,
              help="YT cluster proxy address.")
@click.option("--variants-prefix", required=True,
              help="Directory containing variant tables (output of the ``variants`` command).")
@click.option("--job-format", "job_formats", multiple=True,
              default=_ALL_JOB_FORMATS, show_default=True,
              type=click.Choice(_ALL_JOB_FORMATS),
              help="Job input format(s) to test.  Repeat to test multiple.")
@click.option("--output-format", default="table",
              type=click.Choice(["table", "json", "csv"]),
              show_default=True,
              help="Output format for benchmark results.")
@click.option("--repeat", default=1, show_default=True,
              help="Number of repeated runs per (variant, format) pair for averaging.")
def run(proxy, variants_prefix, job_formats, output_format, repeat):
    """Run the map operation benchmark over all format variant tables.

    For every combination of variant table and job input format the command
    executes::

    \b
      yt map 'cat > /dev/null' --src <variant> --dst /tmp/... --format <fmt>

    and reports wall time, row throughput, and uncompressed byte throughput.
    """
    client = yt.YtClient(proxy=proxy)
    tmp_dir = client.config["remote_temp_tables_directory"] + TMP_SUBDIR
    client.mkdir(tmp_dir, recursive=True)

    variant_names = sorted(str(v) for v in client.list(variants_prefix))
    if not variant_names:
        raise click.ClickException(
            "No variant tables found under {}.  Run the 'variants' command first.".format(
                variants_prefix
            )
        )

    results = []
    total = len(variant_names) * len(job_formats) * repeat

    for variant_name in variant_names:
        variant_path = "{}/{}".format(variants_prefix, variant_name)
        for fmt_name in job_formats:
            for run_idx in range(repeat):
                idx = len(results) + 1
                click.echo(
                    "[{}/{}] {} fmt={} run={}… ".format(
                        idx, total, variant_name, fmt_name, run_idx + 1
                    ),
                    nl=False,
                )
                try:
                    metrics = _run_single(client, variant_path, fmt_name, tmp_dir)
                    metrics["variant"] = variant_name
                    results.append(metrics)
                    click.echo(
                        "{:.1f}s  {:,} rows/s  {:.1f} MB/s".format(
                            metrics["wall_time_sec"],
                            metrics["rows_per_sec"],
                            metrics["mb_per_sec"],
                        )
                    )
                except Exception as exc:
                    click.echo("ERROR: {}".format(exc))
                    results.append({
                        "variant": variant_name,
                        "job_format": fmt_name,
                        "error": str(exc),
                    })

    if output_format == "json":
        click.echo(json.dumps(results, indent=2))
    elif output_format == "csv":
        writer = csv.DictWriter(
            sys.stdout, fieldnames=_RESULT_FIELDS, extrasaction="ignore"
        )
        writer.writeheader()
        for r in results:
            writer.writerow(r)
    else:
        click.echo("")
        _print_table(results)
