"""Entry point for ``python -m yt.benchmarks.map_perf``."""

import click

from yt.benchmarks.map_perf.prepare import prepare, variants
from yt.benchmarks.map_perf.run import run

################################################################################


@click.group()
def cli():
    """Map operation performance benchmark for YTsaurus.

    \b
    Workflow:
      1. Generate a large source table with synthetic data:
           python -m yt.benchmarks.map_perf prepare \\
               --proxy <cluster> \\
               --dst //home/user/map-bench/source \\
               --row-count 10000000

      2. Create variant tables covering different (optimize_for, compression_codec) pairs:
           python -m yt.benchmarks.map_perf variants \\
               --proxy <cluster> \\
               --src //home/user/map-bench/source \\
               --dst-prefix //home/user/map-bench/variants

      3. Run the map benchmark and compare formats and access options:
           python -m yt.benchmarks.map_perf run \\
               --proxy <cluster> \\
               --variants-prefix //home/user/map-bench/variants \\
               --output-format table
    """


cli.add_command(prepare)
cli.add_command(variants)
cli.add_command(run)

################################################################################

if __name__ == "__main__":
    cli()
