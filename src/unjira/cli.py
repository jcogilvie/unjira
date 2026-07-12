"""unjira CLI: collect | digest | status."""

from __future__ import annotations

from datetime import date
from pathlib import Path

import click

from .config import load_config
from .pipeline.collect import run_collect
from .pipeline.digest import render_digest
from .store import Store


@click.group()
@click.option("--config", "config_path", type=click.Path(path_type=Path), default=None,
              help="Path to unjira.config.json (default: ./unjira.config.json)")
@click.pass_context
def main(ctx: click.Context, config_path: Path | None) -> None:
    """A reconciliation agent that keeps Jira in sync with what you actually did."""
    config = load_config(config_path)
    ctx.obj = {"config": config, "store": Store(config.db_path)}


@main.command()
@click.pass_obj
def collect(obj: dict) -> None:
    """Run every enabled collector and persist new events."""
    results = run_collect(obj["config"], obj["store"])
    for name, count in results.items():
        if count < 0:
            click.echo(f"{name}: enabled in config but no such collector is registered")
        else:
            click.echo(f"{name}: {count} new event(s)")


@main.command()
@click.option("--date", "day", type=click.DateTime(formats=["%Y-%m-%d"]), default=None,
              help="Day to digest (default: today)")
@click.pass_obj
def digest(obj: dict, day) -> None:
    """Print the drift digest for a day."""
    target = day.date() if day else date.today()
    click.echo(render_digest(obj["store"], target))


@main.command()
@click.pass_obj
def status(obj: dict) -> None:
    """Event counts and collector cursor freshness."""
    store: Store = obj["store"]
    counts = store.event_counts_by_source()
    if not counts:
        click.echo("No events yet. Run: unjira collect")
        return
    click.echo("Events by source:")
    for row in counts:
        click.echo(f"  {row['source']}: {row['n']} (latest {row['latest']})")
    click.echo("Cursors:")
    for row in store.cursor_counts():
        click.echo(f"  {row['collector']}: {row['n']} tracked resource(s), updated {row['latest']}")


if __name__ == "__main__":
    main()
