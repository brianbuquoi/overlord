// Package metrics provides Prometheus instrumentation for Overlord.
// All collectors are held in an explicit Metrics struct — no global state.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds all Prometheus collectors for Overlord. Pass this struct
// through dependency injection; do not use the default global registry.
type Metrics struct {
	Registry *prometheus.Registry

	TasksTotal           *prometheus.CounterVec
	TaskDuration         *prometheus.HistogramVec
	AgentRequestDuration *prometheus.HistogramVec
	AgentTokensTotal     *prometheus.CounterVec
	TaskRetriesTotal     *prometheus.CounterVec
	TasksDeadLettered    *prometheus.CounterVec
	SanitizerRedactions  *prometheus.CounterVec
	QueueDepth           *prometheus.GaugeVec

	// Broker-side persistence failures. Incremented when the
	// broker's fail-closed state-transition helpers see a store
	// error that would otherwise have been silently swallowed.
	// operation carries the store method name ("update_task",
	// "requeue_task", "merge_metadata") so operators can
	// distinguish routing failures from metadata-only failures.
	BrokerStoreErrorsTotal *prometheus.CounterVec

	// Retry budget metrics.
	RetryBudgetExhaustionsTotal *prometheus.CounterVec

	// Fan-out metrics.
	FanOutAgentResults          *prometheus.CounterVec
	FanOutRequirePolicyFailures *prometheus.CounterVec
}

// New creates a Metrics instance with its own prometheus.Registry.
func New() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		Registry: reg,

		TasksTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "overlord_tasks_total",
			Help: "Total tasks reaching a terminal state.",
		}, []string{"pipeline_id", "stage_id", "final_state"}),

		TaskDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "overlord_task_duration_seconds",
			Help:    "Time from task creation to terminal state.",
			Buckets: []float64{1, 5, 15, 30, 60, 120, 300},
		}, []string{"pipeline_id", "stage_id"}),

		AgentRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "overlord_agent_request_duration_seconds",
			Help:    "Duration of each LLM API call.",
			Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60},
		}, []string{"provider", "model"}),

		AgentTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "overlord_agent_tokens_total",
			Help: "Tokens consumed per API call.",
		}, []string{"provider", "model", "direction"}),

		TaskRetriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "overlord_task_retries_total",
			Help: "Total retry attempts.",
		}, []string{"pipeline_id", "stage_id"}),

		TasksDeadLettered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "overlord_tasks_dead_lettered_total",
			Help: "Tasks routed to dead-letter.",
		}, []string{"pipeline_id", "stage_id"}),

		SanitizerRedactions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "overlord_sanitizer_redactions_total",
			Help: "Sanitizer redactions by pattern class.",
		}, []string{"pipeline_id", "stage_id", "pattern"}),

		QueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "overlord_queue_depth",
			Help: "Current tasks waiting in each stage queue.",
		}, []string{"pipeline_id", "stage_id"}),

		RetryBudgetExhaustionsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "overlord_retry_budget_exhaustions_total",
			Help: "Times a retry budget was exhausted.",
		}, []string{"pipeline_id", "agent_id", "budget_type"}),

		FanOutAgentResults: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "overlord_fanout_agent_results_total",
			Help: "Per-agent results in fan-out executions.",
		}, []string{"pipeline_id", "stage_id", "agent_id", "result"}),

		FanOutRequirePolicyFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "overlord_fanout_require_policy_failures_total",
			Help: "Fan-out executions where require policy was not met.",
		}, []string{"pipeline_id", "stage_id", "require_policy"}),

		BrokerStoreErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "overlord_broker_store_errors_total",
			Help: "Store operations that failed inside broker state-transition helpers.",
		}, []string{"pipeline_id", "stage_id", "operation"}),
	}

	reg.MustRegister(
		m.TasksTotal,
		m.TaskDuration,
		m.AgentRequestDuration,
		m.AgentTokensTotal,
		m.TaskRetriesTotal,
		m.TasksDeadLettered,
		m.SanitizerRedactions,
		m.QueueDepth,
		m.RetryBudgetExhaustionsTotal,
		m.FanOutAgentResults,
		m.FanOutRequirePolicyFailures,
		m.BrokerStoreErrorsTotal,
	)

	return m
}
