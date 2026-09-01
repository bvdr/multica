package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestIssuePropertiesBigramIndexBuildsOnlyWherePGBigmExists runs migration
// 446's real SQL through the runner. The gate has to hold in both directions:
// the index appears where pg_bigm is installed, and where it is not — core
// Postgres, and the pgvector image CI and self-hosted deployments run — the
// version records with its SQL skipped rather than failing the run. A failure
// there would take backend startup with it, for an index that is only an
// optimization: the contains prefilter it serves stays correct unaccelerated.
func TestIssuePropertiesBigramIndexBuildsOnlyWherePGBigmExists(t *testing.T) {
	adminPool := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pgBigmUsable := installExtensionIfAvailable(t, ctx, adminPool, "pg_bigm")

	schema := createScratchSchema(t, ctx, adminPool, "migrate_issue_properties_bigm_")
	pool := openTestPoolWithSearchPath(t, schema+", public")
	if _, err := pool.Exec(ctx, `CREATE TABLE issue (
		id BIGSERIAL PRIMARY KEY,
		properties JSONB NOT NULL DEFAULT '{}'::jsonb
	)`); err != nil {
		t.Fatalf("create issue fixture: %v", err)
	}

	migrationsTable := schema + ".schema_migrations"
	const version = "446_issue_properties_bigm_index"
	if err := runMigrations(ctx, pool, runOptions{
		Direction:             "up",
		Files:                 realMigrationFiles(t, []string{version}, "up"),
		SchemaMigrationsTable: migrationsTable,
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection("up"),
		Conditions:            conditionsForDirection("up"),
	}); err != nil {
		t.Fatalf("apply properties bigram index: %v", err)
	}

	// Recorded either way: a skipped migration still advances the ledger, or
	// every later version would be blocked on a database without pg_bigm.
	assertMigrationVersionRecorded(t, ctx, pool, schema, version, true)
	assertIndexExists(t, pool, schema, "idx_issue_properties_bigm", pgBigmUsable)
	if pgBigmUsable {
		assertIndexValidity(t, pool, schema, "idx_issue_properties_bigm", true)
	}

	// The rollback drops unconditionally: it must be a no-op, not an error,
	// where the up direction never built anything.
	if err := runMigrations(ctx, pool, runOptions{
		Direction:             "down",
		Files:                 realMigrationFiles(t, []string{version}, "down"),
		SchemaMigrationsTable: migrationsTable,
		AdvisoryLockKey:       int64(rand.Uint64()&0x7fffffffffffffff) | 1,
		Hooks:                 hooksForDirection("down"),
	}); err != nil {
		t.Fatalf("roll back properties bigram index: %v", err)
	}
	assertMigrationVersionRecorded(t, ctx, pool, schema, version, false)
	assertIndexExists(t, pool, schema, "idx_issue_properties_bigm", false)
}

// TestOperatorClassAvailabilityFailsClosed checks the gate itself against real
// catalog rows. Every environment has pg_trgm (migration 137), so it stands in
// for "installed"; a condition that answered false for it would silently skip
// every gated migration forever, and one that answered true for an absent
// opclass would abort the run it exists to protect.
func TestOperatorClassAvailabilityFailsClosed(t *testing.T) {
	adminPool := openTestPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := adminPool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_trgm"); err != nil {
		t.Fatalf("install pg_trgm test dependency: %v", err)
	}
	pgBigmUsable := installExtensionIfAvailable(t, ctx, adminPool, "pg_bigm")

	conn, err := adminPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Release()

	for _, tc := range []struct {
		name    string
		opclass extensionOperatorClass
		want    bool
	}{
		{"installed", extensionOperatorClass{"gin", "gin_trgm_ops", "pg_trgm"}, true},
		{"wrong owning extension", extensionOperatorClass{"gin", "gin_trgm_ops", "pg_bigm"}, false},
		{"wrong access method", extensionOperatorClass{"btree", "gin_trgm_ops", "pg_trgm"}, false},
		{"unknown opclass", extensionOperatorClass{"gin", "gin_nonexistent_ops", "pg_trgm"}, false},
		{"migration 446 gate", issuePropertiesBigramOperatorClass, pgBigmUsable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apply, reason, err := whenOperatorClassAvailable(tc.opclass)(ctx, conn)
			if err != nil {
				t.Fatalf("evaluate condition: %v", err)
			}
			if apply != tc.want {
				t.Fatalf("apply=%v (%s), want %v", apply, reason, tc.want)
			}
			if !apply && reason == "" {
				t.Fatal("a skipped migration must report why")
			}
		})
	}
}

// installExtensionIfAvailable installs name when the server ships it and
// reports whether it is usable afterwards, so a test can assert the real
// behaviour of both environments instead of skipping on one of them.
func installExtensionIfAvailable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var available bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = $1)
	`, name).Scan(&available); err != nil {
		t.Fatalf("inspect %s availability: %v", name, err)
	}
	if !available {
		t.Logf("%s is not provided by this Postgres; asserting the skipped path", name)
		return false
	}
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("install %s test dependency: %v", name, err)
	}
	return true
}

// createScratchSchema gives a migration test its own namespace so the real
// migrations can run against fixture tables without touching the shared
// database the rest of the suite uses.
func createScratchSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool, prefix string) string {
	t.Helper()
	schema := fmt.Sprintf("%s%d_%d", prefix, time.Now().UnixNano(), rand.Uint32())
	schemaIdent := pgx.Identifier{schema}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schemaIdent); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanupCtx, "DROP SCHEMA IF EXISTS "+schemaIdent+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	return schema
}
