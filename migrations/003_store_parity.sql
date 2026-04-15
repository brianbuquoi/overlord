-- Migration 003: Store contract parity with Redis/Memory backends.
-- Adds persisted columns for RoutedToDeadLetter and CrossStageTransitions.
-- The DISCARDED state continues to live in the `state` column (matching
-- Memory/Redis), so no separate `discarded` column is introduced.
-- Run before starting Overlord v0.3.0 (or later) against an existing database.
ALTER TABLE overlord_tasks
    ADD COLUMN IF NOT EXISTS routed_to_dead_letter   BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS cross_stage_transitions INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_overlord_tasks_dead_letter
    ON overlord_tasks (state, routed_to_dead_letter)
    WHERE routed_to_dead_letter = TRUE;
