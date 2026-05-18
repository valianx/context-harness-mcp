#!/usr/bin/env python3
"""Export active KG content from Supabase / Postgres to JSON.

Output shape uses graph-DB vocabulary (nodes, nodeType) matching the
migration-00003 schema rename. The import script (import_to_supabase.py)
accepts both this shape and the legacy {"entities": [...]} shape during
the transition window until claude-dev-team PR5 lands.

Usage:
    uv run scripts/export_from_supabase.py [--dsn URL] [--output PATH]

Environment:
    SUPABASE_DB_URL  Postgres DSN used when --dsn is omitted.

Exit codes:
    0  success
    1  any error (DSN failure, query error, write error)
"""
from __future__ import annotations

import json
import os
import socket
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import click
import psycopg
from pgvector.psycopg import register_vector

__version__ = "0.1.0"


def fetch_nodes(cur: psycopg.Cursor) -> list[dict[str, Any]]:
    """Return all active nodes with their observations and embeddings."""
    cur.execute(
        """
        SELECT n.id, n.name, n.node_type
        FROM nodes n
        WHERE n.deleted_at IS NULL
        ORDER BY n.name
        """
    )
    node_rows = cur.fetchall()

    nodes: list[dict[str, Any]] = []
    for node_id, name, node_type in node_rows:
        obs_rows = _fetch_observations(cur, node_id)
        observations = [row[0] for row in obs_rows]
        embeddings = [
            # pgvector returns numpy float32 values — convert to plain Python float
            # so the embedding array is JSON-serializable without a custom encoder.
            [float(v) for v in row[1]] if row[1] is not None else None
            for row in obs_rows
        ]

        entry: dict[str, Any] = {
            "name": name,
            "nodeType": node_type,
            "observations": observations,
        }
        # Only include embeddings key when at least one observation has one,
        # keeping the export compact for fixtures that omit embeddings.
        if any(e is not None for e in embeddings):
            entry["embeddings"] = embeddings

        nodes.append(entry)

    return nodes


def _fetch_observations(
    cur: psycopg.Cursor, node_id: object
) -> list[tuple[str, Any]]:
    """Return (text, embedding) pairs for a single node, active rows only."""
    cur.execute(
        """
        SELECT text, embedding
        FROM observations
        WHERE node_id = %s AND deleted_at IS NULL
        ORDER BY created_at
        """,
        (node_id,),
    )
    return cur.fetchall()


def fetch_relations(cur: psycopg.Cursor) -> list[dict[str, str]]:
    """Return all active relations using node names (not UUIDs)."""
    cur.execute(
        """
        SELECT fn.name AS from_name, tn.name AS to_name, r.relation_type
        FROM relations r
        JOIN nodes fn ON fn.id = r.from_node_id
        JOIN nodes tn ON tn.id = r.to_node_id
        WHERE r.deleted_at IS NULL
          AND fn.deleted_at IS NULL
          AND tn.deleted_at IS NULL
        ORDER BY fn.name, tn.name, r.relation_type
        """
    )
    return [
        {"from": row[0], "to": row[1], "relationType": row[2]}
        for row in cur.fetchall()
    ]


@click.command()
@click.option(
    "--dsn",
    default=lambda: os.environ.get("SUPABASE_DB_URL", ""),
    show_default=True,
    help="Postgres DSN. Defaults to $SUPABASE_DB_URL.",
)
@click.option(
    "--output",
    type=click.Path(path_type=Path),
    default=None,
    help="Output file path. Defaults to stdout.",
)
def main(dsn: str, output: Path | None) -> None:
    """Export active KG content from Supabase to JSON."""
    if not dsn:
        click.echo(
            "Error: --dsn is required or set SUPABASE_DB_URL.", err=True
        )
        sys.exit(1)

    try:
        with psycopg.connect(dsn) as conn:
            register_vector(conn)
            with conn.cursor() as cur:
                nodes = fetch_nodes(cur)
                relations = fetch_relations(cur)
    except Exception as exc:  # noqa: BLE001
        click.echo(f"Error during export: {exc}", err=True)
        sys.exit(1)

    payload = {
        "format_version": __version__,
        "exported_at": datetime.now(timezone.utc).isoformat(),
        "source_host": socket.gethostname(),
        "node_count": len(nodes),
        "relation_count": len(relations),
        "nodes": nodes,
        "relations": relations,
    }

    serialized = json.dumps(payload, indent=2, ensure_ascii=False) + "\n"

    if output is None:
        sys.stdout.write(serialized)
    else:
        try:
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_text(serialized, encoding="utf-8")
            click.echo(
                f"Exported {len(nodes)} nodes, "
                f"{len(relations)} relations → {output}",
                err=True,
            )
        except OSError as exc:
            click.echo(f"Error writing {output}: {exc}", err=True)
            sys.exit(1)


if __name__ == "__main__":
    main()
