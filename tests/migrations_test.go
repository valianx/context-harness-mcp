// Package tests — integration tests for migrations 00004 and 00005.
// Covers AC-1 through AC-5 of PR-1 (feat/auth-migrations).
package tests

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ── AC-1: TestMigrationsApplyCleanly ─────────────────────────────────────────

// TestMigrationsApplyCleanly verifica que goose reporta versión >= 5 en el
// contenedor compartido, confirmando que 00004 y 00005 se aplicaron sin error.
// El shared TestMain ya ejecutó goose.Up; este test lo valida explícitamente.
//
// AC-1: Given a clean DB, When goose up runs, Then 00004 and 00005 apply without error.
func TestMigrationsApplyCleanly(t *testing.T) {
	if testPool == nil {
		t.Skip("testPool is nil — Docker daemon was not available when the suite started")
	}

	db, err := sql.Open("pgx", testDSN)
	require.NoError(t, err, "abrir sql.DB para verificar versión de goose")
	defer db.Close()

	require.NoError(t, goose.SetDialect("postgres"))

	current, err := goose.GetDBVersion(db)
	require.NoError(t, err, "goose.GetDBVersion")
	assert.GreaterOrEqual(t, current, int64(5),
		"goose version debe ser >= 5 — 00004 y 00005 deben estar aplicados; versión actual: %d", current)
}

// TestMigrationsApplyCleanlyFreshContainer verifica que las migraciones 00004
// y 00005 aterrizan limpiamente en un contenedor efímero que parte desde cero,
// independientemente del estado del pool compartido.
func TestMigrationsApplyCleanlyFreshContainer(t *testing.T) {
	ctx := context.Background()
	if err := checkDockerAvailable(ctx); err != nil {
		t.Skipf("Docker daemon no disponible: %v", err)
	}

	pgCtr, err := tcpostgres.Run(ctx,
		pgvectorImage,
		tcpostgres.WithDatabase(testDBName),
		tcpostgres.WithUsername(testDBUser),
		tcpostgres.WithPassword(testDBPassword),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "iniciar contenedor postgres efímero")
	t.Cleanup(func() {
		if termErr := testcontainers.TerminateContainer(pgCtr); termErr != nil {
			t.Logf("advertencia: fallo al terminar contenedor: %v", termErr)
		}
	})

	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err, "obtener connection string del contenedor")

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err, "abrir sql.DB")
	defer db.Close()

	require.NoError(t, goose.SetDialect("postgres"))

	// goose.Up aplica todas las migraciones desde 00001 hasta la última.
	// Un error aquí indica que alguna migración falló.
	require.NoError(t, goose.Up(db, migrationsDir),
		"goose.Up debe aplicar todas las migraciones sin error")

	// Confirmar que se llegó a la versión actual (00001..00008 aplicados).
	// Updated in v0.4.0 when migrations 00007 and 00008 were added.
	version, err := goose.GetDBVersion(db)
	require.NoError(t, err, "goose.GetDBVersion")
	assert.Equal(t, int64(8), version,
		"versión final de goose debe ser 8 (00001..00008 aplicados); versión actual: %d", version)
}

// ── AC-2: TestUsersTableSchema ────────────────────────────────────────────────

// colMeta contiene metadatos de columna obtenidos de information_schema.
type colMeta struct {
	dataType   string
	isNullable string // "YES" o "NO"
}

// TestUsersTableSchema introspects the users table created by migration 00004
// and asserts all 5 columns with correct types, nullability, PK, and indexes.
//
// AC-2: VERIFY users table schema matches DDL spec.
func TestUsersTableSchema(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	// ── columnas esperadas ────────────────────────────────────────────────────
	expectedCols := map[string]colMeta{
		"supabase_user_id": {dataType: "uuid", isNullable: "NO"},
		"email":            {dataType: "text", isNullable: "NO"},
		"revoked_at":       {dataType: "timestamp with time zone", isNullable: "YES"},
		"created_at":       {dataType: "timestamp with time zone", isNullable: "NO"},
		"updated_at":       {dataType: "timestamp with time zone", isNullable: "NO"},
	}

	rows, err := pool.Query(ctx,
		`select column_name, data_type, is_nullable
		   from information_schema.columns
		  where table_schema = 'public'
		    and table_name   = 'users'`,
	)
	require.NoError(t, err, "query information_schema.columns for users")
	defer rows.Close()

	actualCols := make(map[string]colMeta)
	for rows.Next() {
		var name, dt, nullable string
		require.NoError(t, rows.Scan(&name, &dt, &nullable))
		actualCols[name] = colMeta{dataType: dt, isNullable: nullable}
	}
	require.NoError(t, rows.Err())

	// Verificar que existen exactamente las columnas esperadas.
	assert.Equal(t, len(expectedCols), len(actualCols),
		"users debe tener %d columnas, tiene %d: %v", len(expectedCols), len(actualCols), actualCols)

	for col, want := range expectedCols {
		col, want := col, want
		t.Run("column_"+col, func(t *testing.T) {
			got, ok := actualCols[col]
			require.True(t, ok, "columna %q no encontrada en users", col)
			assert.Equal(t, want.dataType, got.dataType,
				"users.%s: data_type esperado %q, obtenido %q", col, want.dataType, got.dataType)
			assert.Equal(t, want.isNullable, got.isNullable,
				"users.%s: is_nullable esperado %q, obtenido %q", col, want.isNullable, got.isNullable)
		})
	}

	// ── clave primaria en supabase_user_id ───────────────────────────────────
	t.Run("primary_key_on_supabase_user_id", func(t *testing.T) {
		var constraintName string
		err := pool.QueryRow(ctx,
			`select kcu.constraint_name
			   from information_schema.table_constraints tc
			   join information_schema.key_column_usage  kcu
			     on tc.constraint_name = kcu.constraint_name
			    and tc.table_schema    = kcu.table_schema
			  where tc.table_schema    = 'public'
			    and tc.table_name      = 'users'
			    and tc.constraint_type = 'PRIMARY KEY'
			    and kcu.column_name    = 'supabase_user_id'`,
		).Scan(&constraintName)
		require.NoError(t, err, "PK constraint sobre supabase_user_id debe existir")
		assert.NotEmpty(t, constraintName)
	})

	// ── índice users_email_idx ────────────────────────────────────────────────
	t.Run("index_users_email_idx", func(t *testing.T) {
		assertMigrationIndexExists(t, ctx, pool, "users", "users_email_idx")
	})

	// ── índice parcial users_active_idx ──────────────────────────────────────
	t.Run("partial_index_users_active_idx", func(t *testing.T) {
		var indexdef string
		err := pool.QueryRow(ctx,
			`select indexdef from pg_indexes
			  where schemaname = 'public'
			    and tablename  = 'users'
			    and indexname  = 'users_active_idx'`,
		).Scan(&indexdef)
		require.NoError(t, err, "índice users_active_idx debe existir")
		assert.Contains(t, indexdef, "WHERE",
			"users_active_idx debe ser un índice parcial (debe contener cláusula WHERE)")
		assert.Contains(t, indexdef, "revoked_at IS NULL",
			"users_active_idx WHERE clause debe filtrar por revoked_at IS NULL")
	})
}

// ── AC-3: TestAttributionColumnsSchema ───────────────────────────────────────

// TestAttributionColumnsSchema verifica que nodes, observations y relations
// tienen las columnas created_by_user_id y created_by_email añadidas por 00005,
// y que las FK constraints usan ON DELETE SET NULL.
//
// AC-3: VERIFY attribution columns and FK constraints.
func TestAttributionColumnsSchema(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	tables := []string{"nodes", "observations", "relations"}

	for _, tbl := range tables {
		tbl := tbl
		t.Run(tbl, func(t *testing.T) {
			t.Run("column_created_by_user_id", func(t *testing.T) {
				meta := fetchMigrationColumnMeta(t, ctx, pool, tbl, "created_by_user_id")
				assert.Equal(t, "uuid", meta.dataType,
					"%s.created_by_user_id debe ser uuid", tbl)
				assert.Equal(t, "YES", meta.isNullable,
					"%s.created_by_user_id debe ser nullable", tbl)
			})

			t.Run("column_created_by_email", func(t *testing.T) {
				meta := fetchMigrationColumnMeta(t, ctx, pool, tbl, "created_by_email")
				assert.Equal(t, "text", meta.dataType,
					"%s.created_by_email debe ser text", tbl)
				assert.Equal(t, "YES", meta.isNullable,
					"%s.created_by_email debe ser nullable", tbl)
			})

			t.Run("fk_on_delete_set_null", func(t *testing.T) {
				var deleteRule string
				err := pool.QueryRow(ctx,
					`select rc.delete_rule
					   from information_schema.key_column_usage   kcu
					   join information_schema.referential_constraints rc
					     on rc.constraint_name  = kcu.constraint_name
					    and rc.constraint_schema = kcu.constraint_schema
					   join information_schema.table_constraints tc
					     on tc.constraint_name = kcu.constraint_name
					    and tc.table_schema    = kcu.table_schema
					  where tc.table_schema    = 'public'
					    and tc.table_name      = $1
					    and kcu.column_name    = 'created_by_user_id'
					    and tc.constraint_type = 'FOREIGN KEY'`,
					tbl,
				).Scan(&deleteRule)
				require.NoError(t, err,
					"FK constraint sobre %s.created_by_user_id debe existir", tbl)
				assert.Equal(t, "SET NULL", deleteRule,
					"%s.created_by_user_id FK debe tener ON DELETE SET NULL", tbl)
			})
		})
	}
}

// ── AC-4: TestAttributionPartialIndexes ──────────────────────────────────────

// TestAttributionPartialIndexes verifica que los tres índices parciales sobre
// created_by_user_id existen con la cláusula WHERE correcta.
//
// AC-4: VERIFY partial indexes *_created_by_user_idx exist with WHERE clause.
func TestAttributionPartialIndexes(t *testing.T) {
	pool := NewTestPool(t)
	ctx := context.Background()

	indexes := []struct {
		table     string
		indexName string
	}{
		{"nodes", "nodes_created_by_user_idx"},
		{"observations", "observations_created_by_user_idx"},
		{"relations", "relations_created_by_user_idx"},
	}

	for _, idx := range indexes {
		idx := idx
		t.Run(idx.indexName, func(t *testing.T) {
			var indexdef string
			err := pool.QueryRow(ctx,
				`select indexdef from pg_indexes
				  where schemaname = 'public'
				    and tablename  = $1
				    and indexname  = $2`,
				idx.table, idx.indexName,
			).Scan(&indexdef)
			require.NoError(t, err,
				"índice %s debe existir en tabla %s", idx.indexName, idx.table)
			assert.Contains(t, indexdef, "WHERE",
				"%s debe ser un índice parcial (WHERE clause)", idx.indexName)
			assert.Contains(t, indexdef, "created_by_user_id IS NOT NULL",
				"%s WHERE clause debe filtrar por created_by_user_id IS NOT NULL", idx.indexName)
		})
	}
}

// ── AC-5: TestPreExistingRowsPreserved ────────────────────────────────────────

// TestPreExistingRowsPreserved simula el escenario "fila pre-existente antes de
// la migración 00005". Usa un contenedor efímero separado para poder aplicar
// 00001..00004, insertar una fila, y luego aplicar 00005, verificando que la
// fila sobrevive con created_by_user_id = NULL y created_by_email = NULL.
//
// AC-5: Given existing rows pre-00005, When 00005 applies, Then rows preserved with NULL attribution.
func TestPreExistingRowsPreserved(t *testing.T) {
	ctx := context.Background()
	if err := checkDockerAvailable(ctx); err != nil {
		t.Skipf("Docker daemon no disponible: %v", err)
	}

	// Iniciar contenedor efímero fresco.
	pgCtr, err := tcpostgres.Run(ctx,
		pgvectorImage,
		tcpostgres.WithDatabase(testDBName),
		tcpostgres.WithUsername(testDBUser),
		tcpostgres.WithPassword(testDBPassword),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err, "iniciar contenedor postgres efímero")
	t.Cleanup(func() {
		if termErr := testcontainers.TerminateContainer(pgCtr); termErr != nil {
			t.Logf("advertencia: fallo al terminar contenedor: %v", termErr)
		}
	})

	dsn, err := pgCtr.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err, "obtener connection string")

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err, "abrir sql.DB")
	defer db.Close()

	require.NoError(t, goose.SetDialect("postgres"))

	// Paso 1: aplicar migraciones 00001..00004 (sin 00005 todavía).
	require.NoError(t, goose.UpTo(db, migrationsDir, 4),
		"goose.UpTo(4) debe aplicar 00001..00004 sin error")

	// Paso 2: insertar una fila en nodes ANTES de aplicar 00005.
	_, err = db.ExecContext(ctx,
		`insert into nodes (name, node_type) values ('pre-existing-node', 'pattern')`,
	)
	require.NoError(t, err, "insertar nodo pre-existente antes de migración 00005")

	// Verificar que el nodo existe tras la inserción.
	var countBefore int
	require.NoError(t,
		db.QueryRowContext(ctx,
			`select count(*) from nodes where name = 'pre-existing-node'`,
		).Scan(&countBefore),
	)
	require.Equal(t, 1, countBefore, "el nodo pre-existente debe estar presente antes de 00005")

	// Paso 3: aplicar 00005.
	require.NoError(t, goose.UpTo(db, migrationsDir, 5),
		"goose.UpTo(5) debe aplicar 00005 sin error")

	// Paso 4: verificar que la fila pre-existente sigue intacta con columnas NULL.
	var (
		name            string
		createdByUserID *string
		createdByEmail  *string
	)
	err = db.QueryRowContext(ctx,
		`select name, created_by_user_id::text, created_by_email
		   from nodes
		  where name = 'pre-existing-node'`,
	).Scan(&name, &createdByUserID, &createdByEmail)
	require.NoError(t, err, "la fila pre-existente debe seguir existiendo tras 00005")

	assert.Equal(t, "pre-existing-node", name,
		"nombre de la fila pre-existente debe estar intacto")
	assert.Nil(t, createdByUserID,
		"created_by_user_id debe ser NULL para fila pre-existente")
	assert.Nil(t, createdByEmail,
		"created_by_email debe ser NULL para fila pre-existente")
}

// ── helpers locales ───────────────────────────────────────────────────────────

// fetchMigrationColumnMeta obtiene data_type e is_nullable de una columna.
// Falla el test si la columna no existe.
func fetchMigrationColumnMeta(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	table, column string,
) colMeta {
	t.Helper()
	var dt, nullable string
	err := pool.QueryRow(ctx,
		`select data_type, is_nullable
		   from information_schema.columns
		  where table_schema = 'public'
		    and table_name   = $1
		    and column_name  = $2`,
		table, column,
	).Scan(&dt, &nullable)
	require.NoError(t, err, "columna %q debe existir en tabla %q", column, table)
	return colMeta{dataType: dt, isNullable: nullable}
}

// assertMigrationIndexExists verifica que un índice existe en pg_indexes.
func assertMigrationIndexExists(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	table, indexName string,
) {
	t.Helper()
	var name string
	err := pool.QueryRow(ctx,
		`select indexname from pg_indexes
		  where schemaname = 'public'
		    and tablename  = $1
		    and indexname  = $2`,
		table, indexName,
	).Scan(&name)
	require.NoError(t, err, fmt.Sprintf("índice %q debe existir en tabla %q", indexName, table))
	assert.Equal(t, indexName, name)
}
