#!/usr/bin/env python3
"""Export active KG content from Supabase / Postgres to JSON.

Output shape matches claude-dev-team/knowledge-graph/export.py byte-for-byte
at the structural level (same top-level keys, same nesting, same field names).
Embeddings serialize as plain JSON arrays of 384 floats — no base64, no
quantization — identical to ChromaDB's export format.

This script is the inverse of import_to_supabase.py; round-trip parity is
asserted by tests/migration_test.go.

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


def fetch_entities(cur: psycopg.Cursor) -> list[dict[str, Any]]:
    """Return all active entities with their observations and embeddings."""
    cur.execute(
        """
        SELECT e.id, e.name, e.entity_type
        FROM entities e
        WHERE e.deleted_at IS NULL
        ORDER BY e.name
        """
    )
    entity_rows = cur.fetchall()

    entities: list[dict[str, Any]] = []
    for entity_id, name, entity_type in entity_rows:
        obs_rows = _fetch_observations(cur, entity_id)
        observations = [row[0] for row in obs_rows]
        embeddings = [
            # pgvector returns numpy float32 values — convert to plain Python float
            # so the embedding array is JSON-serializable without a custom encoder.
            [float(v) for v in row[1]] if row[1] is not None else None
            for row in obs_rows
        ]

        entry: dict[str, Any] = {
            "name": name,
            "entityType": entity_type,
            "observations": observations,
        }
        # Only include embeddings key when at least one observation has one,
        # keeping the export compact for fixtures that omit embeddings.
        if any(e is not None for e in embeddings):
            entry["embeddings"] = embeddings

        entities.append(entry)

    return entities


def _fetch_observations(
    cur: psycopg.Cursor, entity_id: object
) -> list[tuple[str, Any]]:
    """Return (text, embedding) pairs for a single entity, active rows only."""
    cur.execute(
        """
        SELECT text, embedding
        FROM observations
        WHERE entity_id = %s AND deleted_at IS NULL
        ORDER BY created_at
        """,
        (entity_id,),
    )
    return cur.fetchall()


def fetch_relations(cur: psycopg.Cursor) -> list[dict[str, str]]:
    """Return all active relations using entity names (not UUIDs)."""
    cur.execute(
        """
        SELECT fe.name AS from_name, te.name AS to_name, r.relation_type
        FROM relations r
        JOIN entities fe ON fe.id = r.from_entity_id
        JOIN entities te ON te.id = r.to_entity_id
        WHERE r.deleted_at IS NULL
          AND fe.deleted_at IS NULL
          AND te.deleted_at IS NULL
        ORDER BY fe.name, te.name, r.relation_type
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
    """Export active KG content from Supabase to JSON (same shape as export.py)."""
    if not dsn:
        click.echo(
            "Error: --dsn is required or set SUPABASE_DB_URL.", err=True
        )
        sys.exit(1)

    try:
        with psycopg.connect(dsn) as conn:
            register_vector(conn)
            with conn.cursor() as cur:
                entities = fetch_entities(cur)
                relations = fetch_relations(cur)
    except Exception as exc:  # noqa: BLE001
        click.echo(f"Error during export: {exc}", err=True)
        sys.exit(1)

    payload = {
        "format_version": __version__,
        "exported_at": datetime.now(timezone.utc).isoformat(),
        "source_host": socket.gethostname(),
        "entity_count": len(entities),
        "relation_count": len(relations),
        "entities": entities,
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
                f"Exported {len(entities)} entities, "
                f"{len(relations)} relations → {output}",
                err=True,
            )
        except OSError as exc:
            click.echo(f"Error writing {output}: {exc}", err=True)
            sys.exit(1)


if __name__ == "__main__":
    main()
