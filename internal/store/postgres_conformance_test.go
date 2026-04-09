//go:build integration

package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orcastrator/orcastrator/internal/store"
	pgstore "github.com/orcastrator/orcastrator/internal/store/postgres"
)

func TestPostgresStoreConformance(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// Create table if not exists.
	_, err = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS orcastrator_tasks (
		id                    TEXT PRIMARY KEY,
		pipeline_id           TEXT NOT NULL,
		stage_id              TEXT NOT NULL,
		input_schema_name     TEXT NOT NULL DEFAULT '',
		input_schema_version  TEXT NOT NULL DEFAULT '',
		output_schema_name    TEXT NOT NULL DEFAULT '',
		output_schema_version TEXT NOT NULL DEFAULT '',
		payload               JSONB,
		metadata              JSONB,
		state                 TEXT NOT NULL DEFAULT 'PENDING',
		attempts              INTEGER NOT NULL DEFAULT 0,
		max_attempts          INTEGER NOT NULL DEFAULT 1,
		created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at            TIMESTAMPTZ
	)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Clean up before running tests.
	_, _ = pool.Exec(ctx, "DELETE FROM orcastrator_tasks")

	RunConformanceTests(t, func() store.Store {
		s, err := pgstore.New(pool, "orcastrator_tasks")
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
