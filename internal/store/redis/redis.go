// Package redis implements a Redis-backed Store using sorted set indexes
// for efficient task listing and FIFO dequeuing.
package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/orcastrator/orcastrator/internal/broker"
	"github.com/orcastrator/orcastrator/internal/store"
	"github.com/redis/go-redis/v9"
)

// RedisStore implements store.Store using Redis as the backend.
// Keys:
//
//	{prefix}:task:{taskID}   → JSON-serialized Task (string)
//	{prefix}:queue:{stageID} → Redis List (LPUSH to enqueue, BLMOVE to dequeue)
//	{prefix}:processing:{stageID} → Processing list (reliability via BLMOVE)
//	{prefix}:index:{pipelineID}:{stageID} → Sorted set (score=CreatedAt unix ts, member=taskID)
type RedisStore struct {
	client  redis.Cmdable
	prefix  string
	taskTTL time.Duration
}

// New creates a RedisStore. prefix is prepended to all keys (e.g. "orcastrator:").
// taskTTL sets the EXPIRE on task keys; zero means no expiry.
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

	// Maintain sorted set index for ListTasks (score = CreatedAt unix timestamp).
	pipe.ZAdd(ctx, r.indexKey(task.PipelineID, stageID), redis.Z{
		Score:  float64(task.CreatedAt.UnixNano()) / 1e9,
		Member: task.ID,
	})

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
		// Use a short timeout so we re-check ctx.Done() periodically.
		// go-redis enforces a minimum 1s for blocking commands, but also
		// cancels immediately when ctx is done, so short-lived contexts
		// are still respected.
		taskID, err := r.client.BLMove(ctx, qKey, pKey, "RIGHT", "LEFT", 2*time.Second).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				// Timeout on BLMOVE — no items available, loop and retry.
				continue
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("redis: blmove: %w", err)
		}

		// Fetch the task data.
		data, err := r.client.Get(ctx, r.taskKey(taskID)).Bytes()
		if err != nil {
			// Task key expired or was deleted — remove from processing list and retry.
			r.client.LRem(ctx, pKey, 1, taskID)
			continue
		}

		var task broker.Task
		if err := json.Unmarshal(data, &task); err != nil {
			r.client.LRem(ctx, pKey, 1, taskID)
			continue
		}

		// Skip expired tasks.
		if !task.ExpiresAt.IsZero() && time.Now().After(task.ExpiresAt) {
			r.client.Del(ctx, r.taskKey(taskID))
			r.client.LRem(ctx, pKey, 1, taskID)
			continue
		}

		// Remove from processing list on successful dequeue.
		r.client.LRem(ctx, pKey, 1, taskID)

		return &task, nil
	}
}

func (r *RedisStore) UpdateTask(ctx context.Context, taskID string, update broker.TaskUpdate) error {
	data, err := r.client.Get(ctx, r.taskKey(taskID)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return store.ErrTaskNotFound
		}
		return fmt.Errorf("redis: get for update: %w", err)
	}

	var task broker.Task
	if err := json.Unmarshal(data, &task); err != nil {
		return fmt.Errorf("redis: unmarshal for update: %w", err)
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
	task.UpdatedAt = time.Now()

	updated, err := json.Marshal(&task)
	if err != nil {
		return fmt.Errorf("redis: marshal updated task: %w", err)
	}

	ttl := r.client.TTL(ctx, r.taskKey(taskID)).Val()
	if ttl <= 0 {
		ttl = 0
	}
	if err := r.client.Set(ctx, r.taskKey(taskID), updated, ttl).Err(); err != nil {
		return fmt.Errorf("redis: set updated task: %w", err)
	}

	// Remove terminal tasks from the sorted set index so ListTasks only
	// returns active work by default. Callers filtering for DONE/FAILED
	// tasks will need to query the task keys directly — document this as
	// a known limitation of the sorted set approach.
	if task.State == broker.TaskStateDone || task.State == broker.TaskStateFailed {
		r.client.ZRem(ctx, r.indexKey(task.PipelineID, task.StageID), taskID)
	}

	return nil
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
	// Collect the index keys to query.
	indexKeys, err := r.resolveIndexKeys(ctx, filter)
	if err != nil {
		return nil, err
	}
	if len(indexKeys) == 0 {
		return &broker.ListTasksResult{Tasks: nil, Total: 0}, nil
	}

	// When state filter is set, we can't rely on ZCARD or ZRANGE LIMIT for
	// accurate pagination because state is not indexed in the sorted set.
	// We fetch all IDs from the index, resolve tasks, then filter in Go.
	// This is still O(log N + M) per index key instead of O(total keyspace).
	hasStateFilter := filter.State != nil

	if hasStateFilter {
		return r.listTasksWithStateFilter(ctx, filter, indexKeys)
	}
	return r.listTasksFromIndex(ctx, filter, indexKeys)
}

// resolveIndexKeys determines which sorted set keys to query based on the filter.
func (r *RedisStore) resolveIndexKeys(ctx context.Context, filter broker.TaskFilter) ([]string, error) {
	if filter.PipelineID != nil && filter.StageID != nil {
		return []string{r.indexKey(*filter.PipelineID, *filter.StageID)}, nil
	}
	if filter.PipelineID != nil {
		// Union across all stage index sets for this pipeline.
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
	// No pipeline filter — scan all index keys.
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

// listTasksFromIndex handles the common case: no state filter.
// Pagination is done at the Redis level via ZRANGEBYSCORE LIMIT.
func (r *RedisStore) listTasksFromIndex(ctx context.Context, filter broker.TaskFilter, indexKeys []string) (*broker.ListTasksResult, error) {
	now := time.Now()

	if len(indexKeys) == 1 {
		// Single index key — use ZCARD for total and ZRANGEBYSCORE with LIMIT.
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
			zrangeArgs.Count = -1 // all
		}

		taskIDs, err := r.client.ZRangeArgs(ctx, zrangeArgs).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: zrange: %w", err)
		}

		tasks := r.fetchTasksByIDs(ctx, taskIDs, now, nil)

		return &broker.ListTasksResult{Tasks: tasks, Total: int(total)}, nil
	}

	// Multiple index keys — collect all IDs, deduplicate, sort by score.
	return r.listTasksMultiIndex(ctx, filter, indexKeys, now, nil)
}

// listTasksWithStateFilter fetches all IDs from index, resolves, then filters by state in Go.
func (r *RedisStore) listTasksWithStateFilter(ctx context.Context, filter broker.TaskFilter, indexKeys []string) (*broker.ListTasksResult, error) {
	now := time.Now()

	if len(indexKeys) == 1 {
		key := indexKeys[0]
		taskIDs, err := r.client.ZRangeArgs(ctx, redis.ZRangeArgs{
			Key:     key,
			Start:   "-inf",
			Stop:    "+inf",
			ByScore: true,
			Count:   -1,
		}).Result()
		if err != nil {
			return nil, fmt.Errorf("redis: zrange: %w", err)
		}

		tasks := r.fetchTasksByIDs(ctx, taskIDs, now, filter.State)
		total := len(tasks)

		// Apply offset/limit in Go.
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

	return r.listTasksMultiIndex(ctx, filter, indexKeys, now, filter.State)
}

// listTasksMultiIndex handles querying across multiple index keys.
func (r *RedisStore) listTasksMultiIndex(ctx context.Context, filter broker.TaskFilter, indexKeys []string, now time.Time, stateFilter *broker.TaskState) (*broker.ListTasksResult, error) {
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

	tasks := r.fetchTasksByIDs(ctx, allIDs, now, stateFilter)
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

// fetchTasksByIDs resolves task IDs to Task objects via MGET.
// Dangling index entries (task key expired) are silently skipped.
// If stateFilter is non-nil, only tasks matching that state are returned.
func (r *RedisStore) fetchTasksByIDs(ctx context.Context, taskIDs []string, now time.Time, stateFilter *broker.TaskState) []*broker.Task {
	if len(taskIDs) == 0 {
		return nil
	}

	// Build task key list for MGET.
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
			// Dangling index entry — task key expired. Silently skip.
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
		if stateFilter != nil && task.State != *stateFilter {
			continue
		}
		tasks = append(tasks, &task)
	}
	return tasks
}
