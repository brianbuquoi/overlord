//go:build integration

package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/brianbuquoi/overlord/internal/store"
	pgstore "github.com/brianbuquoi/overlord/internal/store/postgres"
	"github.com/brianbuquoi/overlord/internal/testutil/pgschema"
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

	// Centralized schema creation; see internal/testutil/pgschema.
	if err := pgschema.CreateTable(ctx, pool, "overlord_tasks"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Clean up before running tests.
	_, _ = pool.Exec(ctx, "DELETE FROM overlord_tasks")

	RunConformanceTests(t, func() store.Store {
		s, err := pgstore.New(pool, "overlord_tasks")
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}
