CREATE TABLE IF NOT EXISTS orcastrator_tasks (
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
);

CREATE INDEX IF NOT EXISTS idx_tasks_stage_state_created
    ON orcastrator_tasks (stage_id, state, created_at);
