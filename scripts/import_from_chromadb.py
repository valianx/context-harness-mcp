#!/usr/bin/env python3
"""Migrate a local ChromaDB knowledge graph into Supabase / Postgres.

This is a convenience wrapper around import_to_supabase.py. It either reads a
pre-exported JSON (via --source-export) or auto-runs claude-dev-team's
export.py to produce one, then pipes the result through import_to_supabase.

ChromaDB is imported lazily — it is only loaded when this script runs, so the
other scripts (import_to_supabase, export_from_supabase, seed_dev) work
without the chromadb package installed.

Discovery order for export.py when --source-export is not provided:
  1. ../claude-dev-team/knowledge-graph/export.py  (sibling repo on developer machine)
  2. ~/.claude/knowledge-graph/export.py            (installed via claude-dev-team installer)

Usage:
    uv run scripts/import_from_chromadb.py [--dsn URL] [--source-export PATH]

Environment:
    SUPABASE_DB_URL  Postgres DSN used when --dsn is omitted.

Exit codes:
    0  success
    1  any error (export.py not found, DSN failure, import error)
"""
from __future__ import annotations

import os
import subprocess
import sys
import tempfile
from pathlib import Path

import click

# Candidate paths for claude-dev-team's export.py, in priority order.
_EXPORT_PY_CANDIDATES = [
    Path(__file__).parent.parent.parent / "claude-dev-team" / "knowledge-graph" / "export.py",
    Path.home() / ".claude" / "knowledge-graph" / "export.py",
]


def _locate_export_py() -> Path | None:
    """Return the first candidate export.py that exists, or None."""
    for candidate in _EXPORT_PY_CANDIDATES:
        if candidate.exists():
            return candidate
    return None


def _run_export(export_py: Path, out_path: Path) -> None:
    """Run claude-dev-team's export.py via uv, writing JSON to out_path."""
    knowledge_graph_dir = export_py.parent
    result = subprocess.run(
        [
            "uv",
            "run",
            "--directory",
            str(knowledge_graph_dir),
            "python",
            str(export_py),
            "--out",
            str(out_path),
        ],
        capture_output=False,
        check=False,
    )
    if result.returncode != 0:
        click.echo(
            f"Error: export.py exited with code {result.returncode}.",
            err=True,
        )
        sys.exit(1)


def _run_import(json_path: Path, dsn: str) -> None:
    """Shell out to import_to_supabase.py with the given JSON and DSN."""
    import_script = Path(__file__).parent / "import_to_supabase.py"
    env = os.environ.copy()
    if dsn:
        env["SUPABASE_DB_URL"] = dsn

    result = subprocess.run(
        ["uv", "run", str(import_script), str(json_path), "--dsn", dsn],
        env=env,
        capture_output=False,
        check=False,
    )
    if result.returncode != 0:
        sys.exit(result.returncode)


@click.command()
@click.option(
    "--dsn",
    default=lambda: os.environ.get("SUPABASE_DB_URL", ""),
    show_default=True,
    help="Postgres DSN. Defaults to $SUPABASE_DB_URL.",
)
@click.option(
    "--source-export",
    type=click.Path(exists=True, path_type=Path),
    default=None,
    help=(
        "Path to a pre-exported JSON from claude-dev-team's export.py. "
        "When omitted, the script locates and runs export.py automatically."
    ),
)
def main(dsn: str, source_export: Path | None) -> None:
    """Migrate a local ChromaDB KG to Supabase (one-shot, idempotent)."""
    if not dsn:
        click.echo(
            "Error: --dsn is required or set SUPABASE_DB_URL.", err=True
        )
        sys.exit(1)

    if source_export is not None:
        # Operator provided a pre-exported JSON — skip the export step.
        _run_import(source_export, dsn)
        return

    export_py = _locate_export_py()
    if export_py is None:
        candidates = "\n  ".join(str(c) for c in _EXPORT_PY_CANDIDATES)
        click.echo(
            f"Error: could not locate claude-dev-team's export.py.\n"
            f"Searched:\n  {candidates}\n"
            f"Tip: pass --source-export PATH to a pre-exported JSON instead.",
            err=True,
        )
        sys.exit(1)

    click.echo(f"Using export.py at: {export_py}")

    with tempfile.NamedTemporaryFile(
        suffix=".json", delete=False, prefix="chromadb-export-"
    ) as tmp:
        tmp_path = Path(tmp.name)

    try:
        _run_export(export_py, tmp_path)
        _run_import(tmp_path, dsn)
    finally:
        if tmp_path.exists():
            tmp_path.unlink()


if __name__ == "__main__":
    main()
