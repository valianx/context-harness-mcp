#!/usr/bin/env python3
"""Import a KG JSON export into Supabase / Postgres.

Idempotent merge semantics:
  - nodes        : ON CONFLICT (name) DO NOTHING
  - observations : ON CONFLICT (node_id, text) DO NOTHING
  - relations    : ON CONFLICT (from_node_id, to_node_id, relation_type) DO NOTHING

Re-running against the same JSON adds zero new rows.

Shape compatibility:
  - New shape (post-migration-00003): {"nodes": [...], "nodeType": ...}
  - Legacy shape (pre-PR5): {"entities": [...], "entityType": ...}
    Accepted with a deprecation warning on stderr. The fallback path is kept
    until claude-dev-team PR5 ships and retires the old ChromaDB export shape.

Usage:
    uv run scripts/import_to_supabase.py <input.json> [--dsn URL]

Environment:
    SUPABASE_DB_URL  Postgres DSN used when --dsn is omitted.

Exit codes:
    0  success
    1  any error (file missing, schema mismatch, DSN failure, etc.)
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path
from typing import Any

import click
import psycopg
from pgvector.psycopg import register_vector

EXPECTED_EMBEDDING_DIMS = 384


def _normalize_payload(payload: dict[str, Any]) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """Extract nodes and relations from the payload, handling both shape variants.

    Returns (nodes, relations) where each node dict has a "nodeType" key
    (normalised from "entityType" if the legacy shape was detected).
    """
    if "nodes" in payload:
        nodes = payload["nodes"]
    elif "entities" in payload:
        click.echo(
            "Warning: input uses legacy {'entities': [...]} shape. "
            "Please regenerate exports with the updated export_from_supabase.py "
            "after claude-dev-team PR5 lands to retire this fallback path.",
            err=True,
        )
        nodes = payload["entities"]
        # Normalise entityType → nodeType for the rest of the import logic.
        for node in nodes:
            if "entityType" in node and "nodeType" not in node:
                node["nodeType"] = node.pop("entityType")
    else:
        nodes = []

    relations = payload.get("relations", [])
    return nodes, relations


def import_nodes(
    cur: psycopg.Cursor,
    nodes: list[dict[str, Any]],
) -> int:
    """Insert nodes; returns count of rows actually inserted."""
    inserted = 0
    for node in nodes:
        # Defensive: skip rows that carry a soft-delete marker (not expected
        # from export.py, but guard against future format additions).
        if node.get("deleted_at"):
            continue
        result = cur.execute(
            """
            INSERT INTO nodes (name, node_type)
            VALUES (%s, %s)
            ON CONFLICT (name) DO NOTHING
            RETURNING id
            """,
            (node["name"], node["nodeType"]),
        )
        if result.fetchone():
            inserted += 1
    return inserted


def import_observations(
    cur: psycopg.Cursor,
    nodes: list[dict[str, Any]],
) -> tuple[int, int]:
    """Insert observations grouped by node; returns (inserted, deduped)."""
    inserted = 0
    deduped = 0
    for node in nodes:
        if node.get("deleted_at"):
            continue

        # Resolve the node UUID we just inserted (or that already existed).
        row = cur.execute(
            "SELECT id FROM nodes WHERE name = %s AND deleted_at IS NULL",
            (node["name"],),
        ).fetchone()
        if row is None:
            # Should not happen after import_nodes, but skip defensively.
            continue
        node_id = row[0]

        observations = node.get("observations", [])
        embeddings = node.get("embeddings", [])

        for idx, text in enumerate(observations):
            embedding = embeddings[idx] if idx < len(embeddings) else None

            if embedding is not None:
                dims = len(embedding)
                if dims != EXPECTED_EMBEDDING_DIMS:
                    raise ValueError(
                        f"Node '{node['name']}' observation {idx}: "
                        f"expected {EXPECTED_EMBEDDING_DIMS}-dim embedding, got {dims}."
                    )
                emb_value = embedding
            else:
                emb_value = None

            result = cur.execute(
                """
                INSERT INTO observations (node_id, text, embedding)
                VALUES (%s, %s, %s)
                ON CONFLICT (node_id, text) DO NOTHING
                RETURNING id
                """,
                (node_id, text, emb_value),
            )
            if result.fetchone():
                inserted += 1
            else:
                deduped += 1

    return inserted, deduped


def import_relations(
    cur: psycopg.Cursor,
    relations: list[dict[str, Any]],
) -> tuple[int, int]:
    """Insert relations by resolving node names to IDs; returns (inserted, deduped)."""
    inserted = 0
    deduped = 0
    for rel in relations:
        if rel.get("deleted_at"):
            continue

        from_name = rel.get("from", "")
        to_name = rel.get("to", "")
        rel_type = rel.get("relationType", "")

        if not from_name or not to_name or not rel_type:
            continue

        from_row = cur.execute(
            "SELECT id FROM nodes WHERE name = %s AND deleted_at IS NULL",
            (from_name,),
        ).fetchone()
        to_row = cur.execute(
            "SELECT id FROM nodes WHERE name = %s AND deleted_at IS NULL",
            (to_name,),
        ).fetchone()

        if from_row is None or to_row is None:
            # Node not present — skip rather than error; handles partial exports.
            continue

        result = cur.execute(
            """
            INSERT INTO relations (from_node_id, to_node_id, relation_type)
            VALUES (%s, %s, %s)
            ON CONFLICT (from_node_id, to_node_id, relation_type) DO NOTHING
            RETURNING id
            """,
            (from_row[0], to_row[0], rel_type),
        )
        if result.fetchone():
            inserted += 1
        else:
            deduped += 1

    return inserted, deduped


@click.command()
@click.argument("input_json", type=click.Path(exists=True, path_type=Path))
@click.option(
    "--dsn",
    default=lambda: os.environ.get("SUPABASE_DB_URL", ""),
    show_default=True,
    help="Postgres DSN. Defaults to $SUPABASE_DB_URL.",
)
def main(input_json: Path, dsn: str) -> None:
    """Import a KG JSON export into Supabase / Postgres (idempotent merge)."""
    if not dsn:
        click.echo(
            "Error: --dsn is required or set SUPABASE_DB_URL.", err=True
        )
        sys.exit(1)

    try:
        payload = json.loads(input_json.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError) as exc:
        click.echo(f"Error reading {input_json}: {exc}", err=True)
        sys.exit(1)

    nodes, relations = _normalize_payload(payload)

    try:
        with psycopg.connect(dsn) as conn:
            register_vector(conn)
            with conn.transaction():
                with conn.cursor() as cur:
                    node_inserted = import_nodes(cur, nodes)
                    obs_inserted, obs_deduped = import_observations(cur, nodes)
                    rel_inserted, rel_deduped = import_relations(cur, relations)

    except Exception as exc:  # noqa: BLE001
        click.echo(f"Error during import: {exc}", err=True)
        sys.exit(1)

    node_deduped = len(nodes) - node_inserted
    click.echo(
        f"imported nodes={node_inserted} observations={obs_inserted} "
        f"relations={rel_inserted} "
        f"(deduped: nodes={node_deduped} observations={obs_deduped} "
        f"relations={rel_deduped})"
    )


if __name__ == "__main__":
    main()
