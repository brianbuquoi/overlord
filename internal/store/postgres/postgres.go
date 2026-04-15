// Package postgres implements a Postgres-backed Store using pgx. It provides
// transactional task operations with FOR UPDATE SKIP LOCKED for safe
// multi-instance dequeuing.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/store"
)

// validTableName matches safe SQL identifiers: letters/underscore start,
// followed by letters, digits, or underscores.
var validTableName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// PostgresStore implements store.Store using PostgreSQL as the backend.
// Uses SELECT ... FOR UPDATE SKIP LOCKED for multi-worker dequeue semantics.
type PostgresStore struct {
	pool  *pgxpool.Pool
	table string
}

// selectColumns is the ordered column list used by every SELECT in this
// package. scanTask/scanTaskFromRows depend on this exact order.
const selectColumns = `id, pipeline_id, stage_id,
	input_schema_name, input_schema_version,
	output_schema_name, output_schema_version,
	payload, metadata, state,
	attempts, max_attempts,
	created_at, updated_at, expires_at,
	routed_to_dead_letter, cross_stage_transitions`

// New creates a PostgresStore. table is the task table name (e.g. "overlord_tasks").
// Returns an error if the table name contains unsafe characters that could
// enable SQL injection when interpolated into queries.
func New(pool *pgxpool.Pool, table string) (*PostgresStore, error) {
	if !validTableName.MatchString(table) {
		return nil, fmt.Errorf("postgres: invalid table name %q (must match %s)", table, validTableName.String())
	}
	return &PostgresStore{
		pool:  pool,
		table: table,
	}, nil
}

func (p *PostgresStore) EnqueueTask(ctx context.Context, stageID string, task *broker.Task) error {
	payload, err := json.Marshal(task.Payload)
	if err != nil {
		return fmt.Errorf("postgres: marshal payload: %w", err)
	}

	metadata, err := json.Marshal(task.Metadata)
	if err != nil {
		return fmt.Errorf("postgres: marshal metadata: %w", err)
	}

	query := fmt.Sprintf(`INSERT INTO %s (
		id, pipeline_id, stage_id,
		input_schema_name, input_schema_version,
		output_schema_name, output_schema_version,
		payload, metadata, state,
		attempts, max_attempts,
		created_at, updated_at, expires_at,
		routed_to_dead_letter, cross_stage_transitions
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`, p.table)

	_, err = p.pool.Exec(ctx, query,
		task.ID, task.PipelineID, stageID,
		task.InputSchemaName, task.InputSchemaVersion,
		task.OutputSchemaName, task.OutputSchemaVersion,
		payload, metadata, string(task.State),
		task.Attempts, task.MaxAttempts,
		task.CreatedAt, task.UpdatedAt, task.ExpiresAt,
		task.RoutedToDeadLetter, task.CrossStageTransitions,
	)
	if err != nil {
		return fmt.Errorf("postgres: insert: %w", err)
	}
	return nil
}

func (p *PostgresStore) DequeueTask(ctx context.Context, stageID string) (*broker.Task, error) {
	query := fmt.Sprintf(`UPDATE %s SET state = $1, updated_at = $2
		WHERE id = (
			SELECT id FROM %s
			WHERE stage_id = $3
			AND state = $4
			AND (expires_at IS NULL OR expires_at = '0001-01-01T00:00:00Z' OR expires_at > $5)
			ORDER BY created_at ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING %s`, p.table, p.table, selectColumns)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		now := time.Now()
		row := p.pool.QueryRow(ctx, query,
			string(broker.TaskStateRouting), now,
			stageID, string(broker.TaskStatePending), now,
		)

		task, err := scanTask(row)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				select {
				case <-ctx.Done():
					return nil, store.ErrQueueEmpty
				case <-time.After(250 * time.Millisecond):
					continue
				}
			}
			return nil, fmt.Errorf("postgres: dequeue: %w", err)
		}

		return task, nil
	}
}

func (p *PostgresStore) UpdateTask(ctx context.Context, taskID string, update broker.TaskUpdate) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the row for the duration of the transaction.
	selectQuery := fmt.Sprintf(`SELECT %s FROM %s WHERE id = $1 FOR UPDATE`, selectColumns, p.table)

	row := tx.QueryRow(ctx, selectQuery, taskID)
	task, err := scanTask(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrTaskNotFound
		}
		return fmt.Errorf("postgres: select for update: %w", err)
	}

	if update.State != nil {
		task.State = *update.State
	}
	if update.StageID != nil {
		task.StageID = *update.StageID
	}
	if update.Payload != nil {
		task.Payload = *update.Payload
	}
	if update.Metadata != nil {
		if task.Metadata == nil {
			task.Metadata = make(map[string]any)
		}
		for k, v := range update.Metadata {
			task.Metadata[k] = v
		}
	}
	if update.Attempts != nil {
		task.Attempts = *update.Attempts
	}
	if update.InputSchemaName != nil {
		task.InputSchemaName = *update.InputSchemaName
	}
	if update.InputSchemaVersion != nil {
		task.InputSchemaVersion = *update.InputSchemaVersion
	}
	if update.OutputSchemaName != nil {
		task.OutputSchemaName = *update.OutputSchemaName
	}
	if update.OutputSchemaVersion != nil {
		task.OutputSchemaVersion = *update.OutputSchemaVersion
	}
	if update.MaxAttempts != nil {
		task.MaxAttempts = *update.MaxAttempts
	}
	if update.RoutedToDeadLetter != nil {
		task.RoutedToDeadLetter = *update.RoutedToDeadLetter
	}
	if update.CrossStageTransitions != nil {
		task.CrossStageTransitions = *update.CrossStageTransitions
	}
	task.UpdatedAt = time.Now()

	payload, err := json.Marshal(task.Payload)
	if err != nil {
		return fmt.Errorf("postgres: marshal payload: %w", err)
	}
	metadata, err := json.Marshal(task.Metadata)
	if err != nil {
		return fmt.Errorf("postgres: marshal metadata: %w", err)
	}

	updateQuery := fmt.Sprintf(`UPDATE %s SET
		state = $1, stage_id = $2, payload = $3, metadata = $4,
		attempts = $5, updated_at = $6,
		input_schema_name = $7, input_schema_version = $8,
		output_schema_name = $9, output_schema_version = $10,
		max_attempts = $11,
		routed_to_dead_letter = $12, cross_stage_transitions = $13
		WHERE id = $14`, p.table)

	_, err = tx.Exec(ctx, updateQuery,
		string(task.State), task.StageID, payload, metadata,
		task.Attempts, task.UpdatedAt,
		task.InputSchemaName, task.InputSchemaVersion,
		task.OutputSchemaName, task.OutputSchemaVersion,
		task.MaxAttempts,
		task.RoutedToDeadLetter, task.CrossStageTransitions,
		taskID,
	)
	if err != nil {
		return fmt.Errorf("postgres: update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

// ClaimForReplay validates that taskID refers to a FAILED+dead-lettered task
// and returns a copy of it. Read-only — the original task is not modified.
// Matches Redis/Memory semantics: the original stays in its terminal
// dead-lettered form after a replay; callers submit a new task with the
// returned payload.
func (p *PostgresStore) ClaimForReplay(ctx context.Context, taskID string) (*broker.Task, error) {
	query := fmt.Sprintf(`SELECT %s FROM %s WHERE id = $1`, selectColumns, p.table)

	row := p.pool.QueryRow(ctx, query, taskID)
	task, err := scanTask(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrTaskNotFound
		}
		return nil, fmt.Errorf("postgres: claim for replay: %w", err)
	}

	if task.State != broker.TaskStateFailed || !task.RoutedToDeadLetter {
		return nil, store.ErrTaskNotReplayable
	}
	return task, nil
}

func (p *PostgresStore) GetTask(ctx context.Context, taskID string) (*broker.Task, error) {
	query := fmt.Sprintf(`SELECT %s FROM %s WHERE id = $1`, selectColumns, p.table)

	row := p.pool.QueryRow(ctx, query, taskID)
	task, err := scanTask(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrTaskNotFound
		}
		return nil, fmt.Errorf("postgres: get: %w", err)
	}

	if !task.ExpiresAt.IsZero() && time.Now().After(task.ExpiresAt) {
		return nil, store.ErrTaskNotFound
	}

	return task, nil
}

func (p *PostgresStore) ListTasks(ctx context.Context, filter broker.TaskFilter) (*broker.ListTasksResult, error) {
	// Build the WHERE clause for both count and data queries.
	whereClause := ` WHERE 1=1`
	args := []any{}
	argIdx := 1

	whereClause += fmt.Sprintf(` AND (expires_at IS NULL OR expires_at = '0001-01-01T00:00:00Z' OR expires_at > $%d)`, argIdx)
	args = append(args, time.Now())
	argIdx++

	if filter.PipelineID != nil {
		whereClause += fmt.Sprintf(` AND pipeline_id = $%d`, argIdx)
		args = append(args, *filter.PipelineID)
		argIdx++
	}
	if filter.StageID != nil {
		whereClause += fmt.Sprintf(` AND stage_id = $%d`, argIdx)
		args = append(args, *filter.StageID)
		argIdx++
	}
	if filter.State != nil {
		whereClause += fmt.Sprintf(` AND state = $%d`, argIdx)
		args = append(args, string(*filter.State))
		argIdx++
	}
	if filter.RoutedToDeadLetter != nil {
		whereClause += fmt.Sprintf(` AND routed_to_dead_letter = $%d`, argIdx)
		args = append(args, *filter.RoutedToDeadLetter)
		argIdx++
	}
	// Exclude DISCARDED tasks by default (matches Memory semantics: a
	// DISCARDED state is opaque unless the caller opts in).
	if !filter.IncludeDiscarded {
		whereClause += fmt.Sprintf(` AND state <> $%d`, argIdx)
		args = append(args, string(broker.TaskStateDiscarded))
		argIdx++
	}

	// Count total matching rows (before offset/limit).
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, p.table) + whereClause
	var total int
	if err := p.pool.QueryRow(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("postgres: count: %w", err)
	}

	// Data query with ordering and pagination.
	query := fmt.Sprintf(`SELECT %s FROM %s`, selectColumns, p.table) + whereClause + ` ORDER BY created_at ASC`

	if filter.Limit > 0 {
		query += fmt.Sprintf(` LIMIT $%d`, argIdx)
		args = append(args, filter.Limit)
		argIdx++
	}
	if filter.Offset > 0 {
		query += fmt.Sprintf(` OFFSET $%d`, argIdx)
		args = append(args, filter.Offset)
		argIdx++
	}

	rows, err := p.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("postgres: list: %w", err)
	}
	defer rows.Close()

	var results []*broker.Task
	for rows.Next() {
		task, err := scanTaskFromRows(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: scan: %w", err)
		}
		results = append(results, task)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &broker.ListTasksResult{Tasks: results, Total: total}, nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanTask(row scannable) (*broker.Task, error) {
	var task broker.Task
	var payloadBytes, metadataBytes []byte
	var state string

	err := row.Scan(
		&task.ID, &task.PipelineID, &task.StageID,
		&task.InputSchemaName, &task.InputSchemaVersion,
		&task.OutputSchemaName, &task.OutputSchemaVersion,
		&payloadBytes, &metadataBytes, &state,
		&task.Attempts, &task.MaxAttempts,
		&task.CreatedAt, &task.UpdatedAt, &task.ExpiresAt,
		&task.RoutedToDeadLetter, &task.CrossStageTransitions,
	)
	if err != nil {
		return nil, err
	}

	task.State = broker.TaskState(state)
	task.Payload = json.RawMessage(payloadBytes)

	if len(metadataBytes) > 0 {
		if err := json.Unmarshal(metadataBytes, &task.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}

	return &task, nil
}

func scanTaskFromRows(rows pgx.Rows) (*broker.Task, error) {
	var task broker.Task
	var payloadBytes, metadataBytes []byte
	var state string

	err := rows.Scan(
		&task.ID, &task.PipelineID, &task.StageID,
		&task.InputSchemaName, &task.InputSchemaVersion,
		&task.OutputSchemaName, &task.OutputSchemaVersion,
		&payloadBytes, &metadataBytes, &state,
		&task.Attempts, &task.MaxAttempts,
		&task.CreatedAt, &task.UpdatedAt, &task.ExpiresAt,
		&task.RoutedToDeadLetter, &task.CrossStageTransitions,
	)
	if err != nil {
		return nil, err
	}

	task.State = broker.TaskState(state)
	task.Payload = json.RawMessage(payloadBytes)

	if len(metadataBytes) > 0 {
		if err := json.Unmarshal(metadataBytes, &task.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
	}

	return &task, nil
}
