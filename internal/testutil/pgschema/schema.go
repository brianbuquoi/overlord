// Package pgschema centralizes the Postgres DDL that integration tests
// use to stand up an overlord_tasks table. The canonical schema lives
// in docs/deployment.md and in the production code paths that read
// from it — integration tests previously hand-rolled their own
// CREATE TABLE and drifted out of sync with the production schema,
// which the audit flagged as a source of false test failures.
//
// Call CreateTable(ctx, pool, tableName) at the start of each
// integration test that needs a clean table. The function is
// idempotent (IF NOT EXISTS) and also runs an ALTER TABLE ADD
// COLUMN IF NOT EXISTS so tables created by earlier runs without
// the newer columns are brought up to the current shape.
package pgschema

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CreateTable applies the canonical overlord_tasks DDL to the
// database pointed at by pool. The tableName must be a safe
// identifier (the caller is responsible for sanitization — use a
// static string or validate the input).
//
// The DDL mirrors the production schema documented in
// docs/deployment.md: every column the store code reads or writes,
// the dequeue index, and the dead-letter partial index. Integration
// tests that need additional cleanup between runs should DELETE
// from the table themselves; CreateTable is safe to call on an
// already-populated table (no rows are touched).
func CreateTable(ctx context.Context, pool *pgxpool.Pool, tableName string) error {
	createSQL := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		id                      TEXT PRIMARY KEY,
		pipeline_id             TEXT NOT NULL,
		stage_id                TEXT NOT NULL,
		input_schema_name       TEXT NOT NULL DEFAULT '',
		input_schema_version    TEXT NOT NULL DEFAULT '',
		output_schema_name      TEXT NOT NULL DEFAULT '',
		output_schema_version   TEXT NOT NULL DEFAULT '',
		payload                 JSONB,
		metadata                JSONB,
		state                   TEXT NOT NULL DEFAULT 'PENDING',
		attempts                INTEGER NOT NULL DEFAULT 0,
		max_attempts            INTEGER NOT NULL DEFAULT 1,
		created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
		expires_at              TIMESTAMPTZ,
		routed_to_dead_letter   BOOLEAN NOT NULL DEFAULT FALSE,
		cross_stage_transitions INTEGER NOT NULL DEFAULT 0
	)`, tableName)
	if _, err := pool.Exec(ctx, createSQL); err != nil {
		return fmt.Errorf("create table %q: %w", tableName, err)
	}

	// Backfill columns for tables created by older test runs so the
	// same test binary works against a reused database.
	alterSQL := fmt.Sprintf(`ALTER TABLE %s
		ADD COLUMN IF NOT EXISTS routed_to_dead_letter   BOOLEAN NOT NULL DEFAULT FALSE,
		ADD COLUMN IF NOT EXISTS cross_stage_transitions INTEGER NOT NULL DEFAULT 0`, tableName)
	if _, err := pool.Exec(ctx, alterSQL); err != nil {
		return fmt.Errorf("alter table %q: %w", tableName, err)
	}

	dequeueIdx := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_dequeue
		ON %s (stage_id, state, created_at)`, tableName, tableName)
	if _, err := pool.Exec(ctx, dequeueIdx); err != nil {
		return fmt.Errorf("create dequeue index on %q: %w", tableName, err)
	}

	dlIdx := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_dead_letter
		ON %s (state, routed_to_dead_letter)
		WHERE routed_to_dead_letter = TRUE`, tableName, tableName)
	if _, err := pool.Exec(ctx, dlIdx); err != nil {
		return fmt.Errorf("create dead-letter index on %q: %w", tableName, err)
	}
	return nil
}
