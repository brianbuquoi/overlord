// Package redis implements a Redis-backed Store using sorted set indexes
// for efficient task listing and FIFO dequeuing.
package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brianbuquoi/overlord/internal/broker"
	"github.com/brianbuquoi/overlord/internal/store"
	"github.com/redis/go-redis/v9"
)

// RedisStore implements store.Store using Redis as the backend.
//
// Key schema:
//
//	{prefix}task:{taskID}                  → JSON-serialized Task (string, optional TTL)
//	{prefix}queue:{stageID}                → Redis list — LPUSH to enqueue, BLMOVE to dequeue
//	{prefix}processing:{stageID}           → Processing list (at-least-once delivery)
//	{prefix}index:{pipelineID}:{stageID}   → Primary sorted set (score=CreatedAt unix seconds,
//	                                          member=taskID). Terminal tasks REMAIN in this
//	                                          index; they age out naturally with the task key TTL.
//	{prefix}tasks:state:{STATE}            → Per-state sorted set (score=CreatedAt unix seconds,
//	                                          member=taskID). Maintained transactionally on
//	                                          enqueue and inside the atomic UpdateTask Lua
//	                                          script. Used by ListTasks when filter.State is set.
//	{prefix}tasks:state:{STATE}:pipeline:{PIPELINE_ID}
//	                                        → ZSET, score = task.CreatedAt unix seconds.
//	                                          Members: task IDs in STATE for PIPELINE_ID.
//	                                          Written on: EnqueueTask, state transitions in
//	                                          updateTaskScript, claimForReplayScript, and
//	                                          rollbackReplayClaimScript.
//	                                          Removed on: task key expiry (TTL-based, not
//	                                          explicit removal).
//	                                          Purpose: O(log N) ListTasks for state+pipeline
//	                                          filter combinations.
//
// Index entries whose task key has TTL-expired become dangling and are silently
// skipped by ListTasks (fetchTasksByIDs tolerates MGET misses).
//
// Note: two-dimensional state×pipeline index is populated on write going
// forward. Tasks created before this index was introduced will not appear in
// pipeline-scoped state queries until their state transitions. A backfill
// script is needed for existing deployments with live task data.
type RedisStore struct {
	client  redis.Cmdable
	prefix  string
	taskTTL time.Duration
}

// New creates a RedisStore. prefix is prepended to all keys (e.g. "overlord:").
// taskTTL sets the EXPIRE on task keys; zero means no expiry. Index entries
// for a given task age out when the task key itself expires (dangling entries
// are silently skipped by ListTasks).
func New(client redis.Cmdable, prefix string, taskTTL time.Duration) *RedisStore {
	return &RedisStore{
		client:  client,
		prefix:  prefix,
		taskTTL: taskTTL,
	}
}

func (r *RedisStore) taskKey(taskID string) string {
	return fmt.Sprintf("%stask:%s", r.prefix, taskID)
}

func (r *RedisStore) queueKey(stageID string) string {
	return fmt.Sprintf("%squeue:%s", r.prefix, stageID)
}

func (r *RedisStore) processingKey(stageID string) string {
	return fmt.Sprintf("%sprocessing:%s", r.prefix, stageID)
}

func (r *RedisStore) indexKey(pipelineID, stageID string) string {
	return fmt.Sprintf("%sindex:%s:%s", r.prefix, pipelineID, stageID)
}

// indexPattern returns a glob pattern matching all index keys for a pipeline.
func (r *RedisStore) indexPattern(pipelineID string) string {
	return fmt.Sprintf("%sindex:%s:*", r.prefix, pipelineID)
}

// stateIndexKey returns the per-state secondary index key for a given state.
func (r *RedisStore) stateIndexKey(state broker.TaskState) string {
	return fmt.Sprintf("%stasks:state:%s", r.prefix, string(state))
}

// stateIndexPrefix returns the string prefix used by the Lua update script to
// derive per-state index keys at runtime (it concatenates this prefix with the
// state string inside the script).
func (r *RedisStore) stateIndexPrefix() string {
	return fmt.Sprintf("%stasks:state:", r.prefix)
}

// statePipelineIndexKey returns the two-dimensional state×pipeline index key.
func (r *RedisStore) statePipelineIndexKey(state broker.TaskState, pipelineID string) string {
	return fmt.Sprintf("%stasks:state:%s:pipeline:%s", r.prefix, string(state), pipelineID)
}

func (r *RedisStore) EnqueueTask(ctx context.Context, stageID string, task *broker.Task) error {
	data, err := json.Marshal(task)
	if err != nil {
		return fmt.Errorf("redis: marshal task: %w", err)
	}

	pipe := r.client.Pipeline()

	if r.taskTTL > 0 {
		pipe.Set(ctx, r.taskKey(task.ID), data, r.taskTTL)
	} else {
		pipe.Set(ctx, r.taskKey(task.ID), data, 0)
	}

	pipe.LPush(ctx, r.queueKey(stageID), task.ID)

	score := float64(task.CreatedAt.UnixNano()) / 1e9

	// Maintain primary (pipeline,stage) index for ListTasks.
	pipe.ZAdd(ctx, r.indexKey(task.PipelineID, stageID), redis.Z{
		Score:  score,
		Member: task.ID,
	})

	// Maintain per-state secondary index for ListTasks with state filter.
	pipe.ZAdd(ctx, r.stateIndexKey(task.State), redis.Z{
		Score:  score,
		Member: task.ID,
	})

	// Maintain two-dimensional state×pipeline index for ListTasks with
	// combined state+pipeline filter.
	if task.PipelineID != "" {
		pipe.ZAdd(ctx, r.statePipelineIndexKey(task.State, task.PipelineID), redis.Z{
			Score:  score,
			Member: task.ID,
		})
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis: enqueue: %w", err)
	}
	return nil
}

func (r *RedisStore) DequeueTask(ctx context.Context, stageID string) (*broker.Task, error) {
	qKey := r.queueKey(stageID)
	pKey := r.processingKey(stageID)

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// BLMOVE atomically pops from queue tail and pushes to processing list.
		// Short timeout so we re-check ctx.Done() periodically.
		taskID, err := r.client.BLMove(ctx, qKey, pKey, "RIGHT", "LEFT", 2*time.Second).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("redis: blmove: %w", err)
		}

		data, err := r.client.Get(ctx, r.taskKey(taskID)).Bytes()
		if err != nil {
			r.client.LRem(ctx, pKey, 1, taskID)
			continue
		}

		var task broker.Task
		if err := json.Unmarshal(data, &task); err != nil {
			r.client.LRem(ctx, pKey, 1, taskID)
			continue
		}

		if !task.ExpiresAt.IsZero() && time.Now().After(task.ExpiresAt) {
			r.client.Del(ctx, r.taskKey(taskID))
			r.client.LRem(ctx, pKey, 1, taskID)
			continue
		}

		r.client.LRem(ctx, pKey, 1, taskID)

		return &task, nil
	}
}

// updateTaskScript performs an atomic GET→merge→SET of a task, plus an atomic
// state-index transition when the update changes task.state.
//
// KEYS[1] = task key
// ARGV[1] = JSON object containing only the fields to update (merged over the
//
//	stored task; metadata is merged per-key to preserve existing entries)
//
// ARGV[2] = updated_at timestamp (ISO-8601 string), applied unconditionally
// ARGV[3] = state index key prefix (e.g. "overlord:tasks:state:")
// ARGV[4] = score to use for state index ZADD (unix seconds as string)
//
// Returns: the new state string (may equal the old state). Replies with a Redis
// error carrying TASK_NOT_FOUND if the task key does not exist, which the Go
// caller maps to store.ErrTaskNotFound.
const updateTaskScript = `
local data = redis.call('GET', KEYS[1])
if not data then
  return redis.error_reply('TASK_NOT_FOUND')
end
local task = cjson.decode(data)
local update = cjson.decode(ARGV[1])
local oldState = task.state
for k, v in pairs(update) do
  if k == 'metadata' and type(v) == 'table' then
    if type(task.metadata) ~= 'table' then
      task.metadata = {}
    end
    for mk, mv in pairs(v) do
      task.metadata[mk] = mv
    end
  else
    task[k] = v
  end
end
task.updated_at = ARGV[2]
local newData = cjson.encode(task)
local pttl = redis.call('PTTL', KEYS[1])
if pttl and tonumber(pttl) and tonumber(pttl) > 0 then
  redis.call('SET', KEYS[1], newData, 'PX', tonumber(pttl))
else
  redis.call('SET', KEYS[1], newData)
end
local newState = task.state
if oldState ~= nil and newState ~= nil and oldState ~= newState then
  redis.call('ZREM', ARGV[3] .. oldState, task.id)
  redis.call('ZADD', ARGV[3] .. newState, tonumber(ARGV[4]), task.id)
  local pipeline_id = task.pipeline_id
  if pipeline_id and pipeline_id ~= '' then
    redis.call('ZREM', ARGV[3] .. oldState .. ':pipeline:' .. pipeline_id, task.id)
    redis.call('ZADD', ARGV[3] .. newState .. ':pipeline:' .. pipeline_id, tonumber(ARGV[4]), task.id)
  end
end
return tostring(newState)
`

var updateTask = redis.NewScript(updateTaskScript)

// requeueTaskScript is updateTaskScript + an LPUSH on the new
// stage queue, all inside one server-side execution so the broker
// sees either "task updated AND on queue" or "nothing changed".
// Keeping the script next to updateTaskScript (and keeping the
// body verbatim except for the trailing LPUSH) so changes to the
// update semantics flow to both paths.
//
// KEYS[1] = task key
// KEYS[2] = destination queue list key
// ARGV[1] = JSON object of update fields
// ARGV[2] = updated_at timestamp
// ARGV[3] = state index key prefix
// ARGV[4] = score for state index ZADD
// ARGV[5] = task ID (LPUSH member)
//
// Replies with the task's new state as a string, or TASK_NOT_FOUND.
const requeueTaskScript = `
local data = redis.call('GET', KEYS[1])
if not data then
  return redis.error_reply('TASK_NOT_FOUND')
end
local task = cjson.decode(data)
local update = cjson.decode(ARGV[1])
local oldState = task.state
for k, v in pairs(update) do
  if k == 'metadata' and type(v) == 'table' then
    if type(task.metadata) ~= 'table' then
      task.metadata = {}
    end
    for mk, mv in pairs(v) do
      task.metadata[mk] = mv
    end
  else
    task[k] = v
  end
end
task.updated_at = ARGV[2]
local newData = cjson.encode(task)
local pttl = redis.call('PTTL', KEYS[1])
if pttl and tonumber(pttl) and tonumber(pttl) > 0 then
  redis.call('SET', KEYS[1], newData, 'PX', tonumber(pttl))
else
  redis.call('SET', KEYS[1], newData)
end
local newState = task.state
if oldState ~= nil and newState ~= nil and oldState ~= newState then
  redis.call('ZREM', ARGV[3] .. oldState, task.id)
  redis.call('ZADD', ARGV[3] .. newState, tonumber(ARGV[4]), task.id)
  local pipeline_id = task.pipeline_id
  if pipeline_id and pipeline_id ~= '' then
    redis.call('ZREM', ARGV[3] .. oldState .. ':pipeline:' .. pipeline_id, task.id)
    redis.call('ZADD', ARGV[3] .. newState .. ':pipeline:' .. pipeline_id, tonumber(ARGV[4]), task.id)
  end
end
redis.call('LPUSH', KEYS[2], ARGV[5])
return tostring(newState)
`

var requeueTask = redis.NewScript(requeueTaskScript)

// buildUpdateJSON serializes only the fields that the TaskUpdate actually sets
// so the Lua script performs a partial merge rather than overwriting the whole
// task. This matches the per-field dispatch the previous GET→modify→SET code
// performed in Go.
func buildUpdateJSON(update broker.TaskUpdate) ([]byte, error) {
	m := make(map[string]any)
	if update.State != nil {
		m["state"] = string(*update.State)
	}
	if update.StageID != nil {
		m["stage_id"] = *update.StageID
	}
	if update.Payload != nil {
		var payloadVal any
		if err := json.Unmarshal(*update.Payload, &payloadVal); err != nil {
			return nil, fmt.Errorf("redis: decode payload for update: %w", err)
		}
		m["payload"] = payloadVal
	}
	if update.Metadata != nil {
		m["metadata"] = update.Metadata
	}
	if update.Attempts != nil {
		m["attempts"] = *update.Attempts
	}
	if update.InputSchemaName != nil {
		m["input_schema_name"] = *update.InputSchemaName
	}
	if update.InputSchemaVersion != nil {
		m["input_schema_version"] = *update.InputSchemaVersion
	}
	if update.OutputSchemaName != nil {
		m["output_schema_name"] = *update.OutputSchemaName
	}
	if update.OutputSchemaVersion != nil {
		m["output_schema_version"] = *update.OutputSchemaVersion
	}
	if update.MaxAttempts != nil {
		m["max_attempts"] = *update.MaxAttempts
	}
	if update.RoutedToDeadLetter != nil {
		m["routed_to_dead_letter"] = *update.RoutedToDeadLetter
	}
	if update.CrossStageTransitions != nil {
		m["cross_stage_transitions"] = *update.CrossStageTransitions
	}
	return json.Marshal(m)
}

// RequeueTask atomically applies update to the task and LPUSHes it
// onto stageID's queue list via a single Lua script. This is the
// Redis counterpart of the broker's old `UpdateTask + EnqueueTask`
// pair — now all-or-nothing.
//
// Returns ErrTaskNotFound if the task key does not exist.
func (r *RedisStore) RequeueTask(ctx context.Context, taskID, stageID string, update broker.TaskUpdate) error {
	// Make sure the requeued row lands on stageID with state
	// PENDING so it is immediately dequeueable from the new stage's
	// queue (the LPUSH below puts the ID on the queue; the task row
	// state must match for dequeue to accept it).
	if update.StageID == nil {
		update.StageID = &stageID
	}
	if update.State == nil {
		pending := broker.TaskStatePending
		update.State = &pending
	}
	updateJSON, err := buildUpdateJSON(update)
	if err != nil {
		return err
	}

	now := time.Now()
	nowStr := now.UTC().Format(time.RFC3339Nano)
	score := float64(now.UnixNano()) / 1e9
	scoreStr := fmt.Sprintf("%f", score)

	_, err = requeueTask.Run(ctx, r.client,
		[]string{r.taskKey(taskID), r.queueKey(stageID)},
		string(updateJSON),
		nowStr,
		r.stateIndexPrefix(),
		scoreStr,
		taskID,
	).Result()
	if err != nil {
		if strings.Contains(err.Error(), "TASK_NOT_FOUND") {
			return store.ErrTaskNotFound
		}
		return fmt.Errorf("redis: requeue task: %w", err)
	}
	return nil
}

func (r *RedisStore) UpdateTask(ctx context.Context, taskID string, update broker.TaskUpdate) error {
	updateJSON, err := buildUpdateJSON(update)
	if err != nil {
		return err
	}

	now := time.Now()
	// RFC3339Nano matches how time.Time marshals in the Task struct's JSON form,
	// so Lua-side re-encoding of updated_at stays consistent with Go's reader.
	nowStr := now.UTC().Format(time.RFC3339Nano)
	score := float64(now.UnixNano()) / 1e9
	scoreStr := fmt.Sprintf("%f", score)

	_, err = updateTask.Run(ctx, r.client,
		[]string{r.taskKey(taskID)},
		string(updateJSON),
		nowStr,
		r.stateIndexPrefix(),
		scoreStr,
	).Result()
	if err != nil {
		if strings.Contains(err.Error(), "TASK_NOT_FOUND") {
			return store.ErrTaskNotFound
		}
		return fmt.Errorf("redis: update task: %w", err)
	}
	return nil
}

// claimForReplayScript atomically validates a task is in FAILED+dead-lettered
// state, transitions it to REPLAY_PENDING, and flips routed_to_dead_letter to
// false. The transition is the claim token: once made, subsequent callers see
// NOT_REPLAYABLE because the task no longer satisfies the FAILED+dead-lettered
// predicate. Per-state secondary index is updated atomically.
//
//	KEYS[1] = task key
//	ARGV[1] = updated_at timestamp (RFC3339Nano string)
//	ARGV[2] = state index key prefix (e.g. "overlord:tasks:state:")
//	ARGV[3] = score for state index ZADD (unix seconds as string)
//
// Returns: the updated task JSON on success, or "TASK_NOT_FOUND" /
// "NOT_REPLAYABLE" on the failure paths.
const claimForReplayScript = `
local data = redis.call('GET', KEYS[1])
if not data then
  return 'TASK_NOT_FOUND'
end
local task = cjson.decode(data)
if task.state ~= 'FAILED' or not task.routed_to_dead_letter then
  return 'NOT_REPLAYABLE'
end
task.state = 'REPLAY_PENDING'
task.routed_to_dead_letter = false
task.updated_at = ARGV[1]
local newData = cjson.encode(task)
local pttl = redis.call('PTTL', KEYS[1])
if pttl and tonumber(pttl) and tonumber(pttl) > 0 then
  redis.call('SET', KEYS[1], newData, 'PX', tonumber(pttl))
else
  redis.call('SET', KEYS[1], newData)
end
redis.call('ZREM', ARGV[2] .. 'FAILED', task.id)
redis.call('ZADD', ARGV[2] .. 'REPLAY_PENDING', tonumber(ARGV[3]), task.id)
local pipeline_id = task.pipeline_id
if pipeline_id and pipeline_id ~= '' then
  redis.call('ZREM', ARGV[2] .. 'FAILED:pipeline:' .. pipeline_id, task.id)
  redis.call('ZADD', ARGV[2] .. 'REPLAY_PENDING:pipeline:' .. pipeline_id, tonumber(ARGV[3]), task.id)
end
return newData
`

var claimForReplay = redis.NewScript(claimForReplayScript)

// rollbackReplayClaimScript atomically transitions a REPLAY_PENDING task back
// to FAILED with routed_to_dead_letter=true, updating the per-state index.
//
//	KEYS[1] = task key
//	ARGV[1] = updated_at timestamp (RFC3339Nano string)
//	ARGV[2] = state index key prefix
//	ARGV[3] = score for state index ZADD
//
// Returns: "OK" on success, "TASK_NOT_FOUND" / "NOT_REPLAY_PENDING" on failure.
const rollbackReplayClaimScript = `
local data = redis.call('GET', KEYS[1])
if not data then
  return 'TASK_NOT_FOUND'
end
local task = cjson.decode(data)
if task.state ~= 'REPLAY_PENDING' then
  return 'NOT_REPLAY_PENDING'
end
task.state = 'FAILED'
task.routed_to_dead_letter = true
task.updated_at = ARGV[1]
local newData = cjson.encode(task)
local pttl = redis.call('PTTL', KEYS[1])
if pttl and tonumber(pttl) and tonumber(pttl) > 0 then
  redis.call('SET', KEYS[1], newData, 'PX', tonumber(pttl))
else
  redis.call('SET', KEYS[1], newData)
end
redis.call('ZREM', ARGV[2] .. 'REPLAY_PENDING', task.id)
redis.call('ZADD', ARGV[2] .. 'FAILED', tonumber(ARGV[3]), task.id)
local pipeline_id = task.pipeline_id
if pipeline_id and pipeline_id ~= '' then
  redis.call('ZREM', ARGV[2] .. 'REPLAY_PENDING:pipeline:' .. pipeline_id, task.id)
  redis.call('ZADD', ARGV[2] .. 'FAILED:pipeline:' .. pipeline_id, tonumber(ARGV[3]), task.id)
end
return 'OK'
`

var rollbackReplayClaim = redis.NewScript(rollbackReplayClaimScript)

// discardDeadLetterScript atomically transitions a FAILED+dead-lettered
// task to DISCARDED, updating the per-state index. The state transition
// is the claim token: concurrent callers whose task has already left the
// FAILED+DL=true predicate (e.g. because a replay claimed it into
// REPLAY_PENDING) receive NOT_DISCARDABLE rather than silently overwriting
// the winning state.
//
// ALREADY_DISCARDED is distinguished so the HTTP API can continue to
// surface the same 409 ALREADY_DISCARDED response code for idempotent
// retries.
//
//	KEYS[1] = task key
//	ARGV[1] = updated_at timestamp (RFC3339Nano string)
//	ARGV[2] = state index key prefix
//	ARGV[3] = score for state index ZADD
//
// Returns: "OK" / "TASK_NOT_FOUND" / "ALREADY_DISCARDED" / "NOT_DISCARDABLE".
const discardDeadLetterScript = `
local data = redis.call('GET', KEYS[1])
if not data then
  return 'TASK_NOT_FOUND'
end
local task = cjson.decode(data)
if task.state == 'DISCARDED' then
  return 'ALREADY_DISCARDED'
end
if task.state ~= 'FAILED' or not task.routed_to_dead_letter then
  return 'NOT_DISCARDABLE'
end
local prev_state = task.state
task.state = 'DISCARDED'
task.updated_at = ARGV[1]
local newData = cjson.encode(task)
local pttl = redis.call('PTTL', KEYS[1])
if pttl and tonumber(pttl) and tonumber(pttl) > 0 then
  redis.call('SET', KEYS[1], newData, 'PX', tonumber(pttl))
else
  redis.call('SET', KEYS[1], newData)
end
redis.call('ZREM', ARGV[2] .. prev_state, task.id)
redis.call('ZADD', ARGV[2] .. 'DISCARDED', tonumber(ARGV[3]), task.id)
local pipeline_id = task.pipeline_id
if pipeline_id and pipeline_id ~= '' then
  redis.call('ZREM', ARGV[2] .. prev_state .. ':pipeline:' .. pipeline_id, task.id)
  redis.call('ZADD', ARGV[2] .. 'DISCARDED:pipeline:' .. pipeline_id, tonumber(ARGV[3]), task.id)
end
return 'OK'
`

var discardDeadLetter = redis.NewScript(discardDeadLetterScript)

// ClaimForReplay atomically transitions a FAILED+dead-lettered task to
// REPLAY_PENDING and clears RoutedToDeadLetter. The state transition is the
// claim token so concurrent callers each get ErrTaskNotReplayable after the
// winner lands.
func (r *RedisStore) ClaimForReplay(ctx context.Context, taskID string) (*broker.Task, error) {
	now := time.Now()
	nowStr := now.UTC().Format(time.RFC3339Nano)
	score := float64(now.UnixNano()) / 1e9
	scoreStr := fmt.Sprintf("%f", score)

	res, err := claimForReplay.Run(ctx, r.client,
		[]string{r.taskKey(taskID)},
		nowStr,
		r.stateIndexPrefix(),
		scoreStr,
	).Result()
	if err != nil {
		return nil, fmt.Errorf("redis: claim for replay: %w", err)
	}

	s, ok := res.(string)
	if !ok {
		return nil, fmt.Errorf("redis: claim for replay: unexpected reply type %T", res)
	}
	switch s {
	case "TASK_NOT_FOUND":
		return nil, store.ErrTaskNotFound
	case "NOT_REPLAYABLE":
		return nil, store.ErrTaskNotReplayable
	}

	var task broker.Task
	if err := json.Unmarshal([]byte(s), &task); err != nil {
		return nil, fmt.Errorf("redis: claim for replay: unmarshal: %w", err)
	}
	return &task, nil
}

// DiscardDeadLetter atomically transitions a FAILED+dead-lettered task to
// DISCARDED. The state transition is the claim token so concurrent
// replay-vs-discard races cannot silently overwrite a winning replay's
// state. ErrTaskAlreadyDiscarded is returned on a repeat call so the API
// can preserve the ALREADY_DISCARDED response code for idempotent retries.
func (r *RedisStore) DiscardDeadLetter(ctx context.Context, taskID string) error {
	now := time.Now()
	nowStr := now.UTC().Format(time.RFC3339Nano)
	score := float64(now.UnixNano()) / 1e9
	scoreStr := fmt.Sprintf("%f", score)

	res, err := discardDeadLetter.Run(ctx, r.client,
		[]string{r.taskKey(taskID)},
		nowStr,
		r.stateIndexPrefix(),
		scoreStr,
	).Result()
	if err != nil {
		return fmt.Errorf("redis: discard dead-letter: %w", err)
	}
	s, ok := res.(string)
	if !ok {
		return fmt.Errorf("redis: discard dead-letter: unexpected reply type %T", res)
	}
	switch s {
	case "OK":
		return nil
	case "TASK_NOT_FOUND":
		return store.ErrTaskNotFound
	case "ALREADY_DISCARDED":
		return store.ErrTaskAlreadyDiscarded
	case "NOT_DISCARDABLE":
		return store.ErrTaskNotDiscardable
	}
	return fmt.Errorf("redis: discard dead-letter: unexpected reply %q", s)
}

// RollbackReplayClaim atomically reverts a REPLAY_PENDING task to
// FAILED+RoutedToDeadLetter=true so the replay remains recoverable after a
// Submit failure.
func (r *RedisStore) RollbackReplayClaim(ctx context.Context, taskID string) error {
	now := time.Now()
	nowStr := now.UTC().Format(time.RFC3339Nano)
	score := float64(now.UnixNano()) / 1e9
	scoreStr := fmt.Sprintf("%f", score)

	res, err := rollbackReplayClaim.Run(ctx, r.client,
		[]string{r.taskKey(taskID)},
		nowStr,
		r.stateIndexPrefix(),
		scoreStr,
	).Result()
	if err != nil {
		return fmt.Errorf("redis: rollback replay claim: %w", err)
	}
	s, ok := res.(string)
	if !ok {
		return fmt.Errorf("redis: rollback replay claim: unexpected reply type %T", res)
	}
	switch s {
	case "TASK_NOT_FOUND":
		return store.ErrTaskNotFound
	case "NOT_REPLAY_PENDING":
		return store.ErrTaskNotReplayPending
	case "OK":
		return nil
	}
	return fmt.Errorf("redis: rollback replay claim: unexpected reply %q", s)
}

func (r *RedisStore) GetTask(ctx context.Context, taskID string) (*broker.Task, error) {
	data, err := r.client.Get(ctx, r.taskKey(taskID)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, store.ErrTaskNotFound
		}
		return nil, fmt.Errorf("redis: get: %w", err)
	}

	var task broker.Task
	if err := json.Unmarshal(data, &task); err != nil {
		return nil, fmt.Errorf("redis: unmarshal: %w", err)
	}

	if !task.ExpiresAt.IsZero() && time.Now().After(task.ExpiresAt) {
		return nil, store.ErrTaskNotFound
	}

	return &task, nil
}

func (r *RedisStore) ListTasks(ctx context.Context, filter broker.TaskFilter) (*broker.ListTasksResult, error) {
	// When both state and pipeline filters are present, read from the
	// two-dimensional state×pipeline index — O(log N) against the subset
	// rather than a full scan of the flat state ZSET.
	if filter.State != nil && filter.PipelineID != nil {
		return r.listTasksFromStatePipelineIndex(ctx, filter)
	}

	// When a state filter is present, read from the per-state secondary index.
	// This avoids scanning every pipeline/stage index just to throw most IDs
	// away in Go post-filtering.
	if filter.State != nil {
		return r.listTasksFromStateIndex(ctx, filter)
	}

	indexKeys, err := r.resolveIndexKeys(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(indexKeys) == 0 {
		return &broker.ListTasksResult{Tasks: nil, Total: 0}, nil
	}
	return r.listTasksFromIndex(ctx, filter, indexKeys)
}

// resolveIndexKeys determines which sorted set keys to query based on the filter.
func (r *RedisStore) resolveIndexKeys(ctx context.Context, filter broker.TaskFilter) ([]string, error) {
	if filter.PipelineID != nil && filter.StageID != nil {
		return []string{r.indexKey(*filter.PipelineID, *filter.StageID)}, nil
	}
	if filter.PipelineID != nil {
		pattern := r.indexPattern(*filter.PipelineID)
		var keys []string
		iter := r.client.Scan(ctx, 0, pattern, 0).Iterator()
		for iter.Next(ctx) {
			keys = append(keys, iter.Val())
		}
		if err := iter.Err(); err != nil {
			return nil, fmt.Errorf("redis: scan index keys: %w", err)
		}
		return keys, nil
	}
	pattern := fmt.Sprintf("%sindex:*", r.prefix)
	var keys []string
	iter := r.client.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("redis: scan index keys: %w", err)
	}
	return keys, nil
}

// listTasksFromIndex handles the common case: no state filter. Pagination is
// done at Redis level via ZRANGEBYSCORE LIMIT when a single index is queried.
func (r *RedisStore) listTasksFromIndex(ctx context.Context, filter broker.TaskFilter, indexKeys []string) (*broker.ListTasksResult, error) {
	now := time.Now()

	if len(indexKeys) == 1 {
		key := indexKeys[0]
		total, err := r.client.ZCard(ctx, key).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: zcard: %w", err)
		}

		zrangeArgs := redis.ZRangeArgs{
			Key:     key,
			Start:   "-inf",
			Stop:    "+inf",
			ByScore: true,
			Offset:  int64(filter.Offset),
		}
		if filter.Limit > 0 {
			zrangeArgs.Count = int64(filter.Limit)
		} else {
			zrangeArgs.Count = -1
		}

		taskIDs, err := r.client.ZRangeArgs(ctx, zrangeArgs).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: zrange: %w", err)
		}

		tasks := r.fetchTasksByIDs(ctx, taskIDs, now, filter)

		return &broker.ListTasksResult{Tasks: tasks, Total: int(total)}, nil
	}

	return r.listTasksMultiIndex(ctx, filter, indexKeys, now)
}

// listTasksFromStateIndex reads the per-state secondary index and applies any
// remaining filters (pipeline, stage, dead-letter) in Go. This is the path
// taken whenever filter.State is set — it avoids expensive full-index scans.
func (r *RedisStore) listTasksFromStateIndex(ctx context.Context, filter broker.TaskFilter) (*broker.ListTasksResult, error) {
	key := r.stateIndexKey(*filter.State)

	taskIDs, err := r.client.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key:     key,
		Start:   "-inf",
		Stop:    "+inf",
		ByScore: true,
		Count:   -1,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("redis: zrange state index: %w", err)
	}

	now := time.Now()
	tasks := r.fetchTasksByIDs(ctx, taskIDs, now, filter)
	total := len(tasks)

	if filter.Offset > 0 && filter.Offset < len(tasks) {
		tasks = tasks[filter.Offset:]
	} else if filter.Offset >= len(tasks) {
		tasks = nil
	}
	if filter.Limit > 0 && len(tasks) > filter.Limit {
		tasks = tasks[:filter.Limit]
	}

	return &broker.ListTasksResult{Tasks: tasks, Total: total}, nil
}

// listTasksFromStatePipelineIndex reads the two-dimensional state×pipeline
// index directly. Used whenever both State and PipelineID filters are set.
func (r *RedisStore) listTasksFromStatePipelineIndex(ctx context.Context, filter broker.TaskFilter) (*broker.ListTasksResult, error) {
	key := r.statePipelineIndexKey(*filter.State, *filter.PipelineID)

	taskIDs, err := r.client.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key:     key,
		Start:   "-inf",
		Stop:    "+inf",
		ByScore: true,
		Count:   -1,
	}).Result()
	if err != nil {
		return nil, fmt.Errorf("redis: zrange state×pipeline index: %w", err)
	}

	now := time.Now()
	tasks := r.fetchTasksByIDs(ctx, taskIDs, now, filter)
	total := len(tasks)

	if filter.Offset > 0 && filter.Offset < len(tasks) {
		tasks = tasks[filter.Offset:]
	} else if filter.Offset >= len(tasks) {
		tasks = nil
	}
	if filter.Limit > 0 && len(tasks) > filter.Limit {
		tasks = tasks[:filter.Limit]
	}

	return &broker.ListTasksResult{Tasks: tasks, Total: total}, nil
}

// listTasksMultiIndex handles querying across multiple index keys (no state filter).
func (r *RedisStore) listTasksMultiIndex(ctx context.Context, filter broker.TaskFilter, indexKeys []string, now time.Time) (*broker.ListTasksResult, error) {
	var allIDs []string
	seen := make(map[string]bool)

	for _, key := range indexKeys {
		ids, err := r.client.ZRangeArgs(ctx, redis.ZRangeArgs{
			Key:     key,
			Start:   "-inf",
			Stop:    "+inf",
			ByScore: true,
			Count:   -1,
		}).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: zrange %s: %w", key, err)
		}
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				allIDs = append(allIDs, id)
			}
		}
	}

	tasks := r.fetchTasksByIDs(ctx, allIDs, now, filter)
	total := len(tasks)

	if filter.Offset > 0 && filter.Offset < len(tasks) {
		tasks = tasks[filter.Offset:]
	} else if filter.Offset >= len(tasks) {
		tasks = nil
	}
	if filter.Limit > 0 && len(tasks) > filter.Limit {
		tasks = tasks[:filter.Limit]
	}

	return &broker.ListTasksResult{Tasks: tasks, Total: total}, nil
}

// fetchTasksByIDs resolves task IDs to Task objects via MGET and applies in-Go
// filters (state, pipeline, stage, dead-letter, discarded). Dangling index
// entries whose task key expired are silently skipped.
func (r *RedisStore) fetchTasksByIDs(ctx context.Context, taskIDs []string, now time.Time, filter broker.TaskFilter) []*broker.Task {
	if len(taskIDs) == 0 {
		return nil
	}

	keys := make([]string, len(taskIDs))
	for i, id := range taskIDs {
		keys[i] = r.taskKey(id)
	}

	vals, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil
	}

	var tasks []*broker.Task
	for _, val := range vals {
		if val == nil {
			continue
		}
		str, ok := val.(string)
		if !ok {
			continue
		}
		var task broker.Task
		if err := json.Unmarshal([]byte(str), &task); err != nil {
			continue
		}
		if !task.ExpiresAt.IsZero() && now.After(task.ExpiresAt) {
			continue
		}
		if filter.State != nil && task.State != *filter.State {
			continue
		}
		if filter.PipelineID != nil && task.PipelineID != *filter.PipelineID {
			continue
		}
		if filter.StageID != nil && task.StageID != *filter.StageID {
			continue
		}
		if filter.RoutedToDeadLetter != nil && task.RoutedToDeadLetter != *filter.RoutedToDeadLetter {
			continue
		}
		if !filter.IncludeDiscarded && task.State == broker.TaskStateDiscarded {
			continue
		}
		tasks = append(tasks, &task)
	}
	return tasks
}
