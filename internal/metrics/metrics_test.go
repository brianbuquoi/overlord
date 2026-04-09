package metrics_test

import (
	"testing"

	"github.com/orcastrator/orcastrator/internal/metrics"
)

func TestNew_ReturnsIndependentRegistries(t *testing.T) {
	m1 := metrics.New()
	m2 := metrics.New()

	// Increment a counter on m1; m2 should be unaffected.
	m1.TasksTotal.WithLabelValues("p1", "s1", "DONE").Inc()

	fam1 := gatherCounter(t, m1, "orcastrator_tasks_total")
	fam2 := gatherCounter(t, m2, "orcastrator_tasks_total")

	if fam1 != 1 {
		t.Fatalf("m1 counter should be 1, got %v", fam1)
	}
	if fam2 != 0 {
		t.Fatalf("m2 counter should be 0, got %v", fam2)
	}
}

func TestNew_AllCollectorsRegistered(t *testing.T) {
	m := metrics.New()

	// Gather all metrics — should not panic.
	if _, err := m.Registry.Gather(); err != nil {
		t.Fatal(err)
	}

	// Touch each metric so it appears in the gather output.
	m.TasksTotal.WithLabelValues("p", "s", "DONE").Inc()
	m.TaskDuration.WithLabelValues("p", "s").Observe(1)
	m.AgentRequestDuration.WithLabelValues("anthropic", "model").Observe(1)
	m.AgentTokensTotal.WithLabelValues("anthropic", "model", "input").Inc()
	m.TaskRetriesTotal.WithLabelValues("p", "s").Inc()
	m.TasksDeadLettered.WithLabelValues("p", "s").Inc()
	m.SanitizerRedactions.WithLabelValues("p", "s", "injection").Inc()
	m.QueueDepth.WithLabelValues("p", "s").Set(5)

	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"orcastrator_tasks_total":                    false,
		"orcastrator_task_duration_seconds":          false,
		"orcastrator_agent_request_duration_seconds": false,
		"orcastrator_agent_tokens_total":             false,
		"orcastrator_task_retries_total":             false,
		"orcastrator_tasks_dead_lettered_total":      false,
		"orcastrator_sanitizer_redactions_total":     false,
		"orcastrator_queue_depth":                    false,
	}

	for _, f := range families {
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
	}

	for name, found := range want {
		if !found {
			t.Errorf("metric family %q not found in gathered output", name)
		}
	}
}

// gatherCounter returns the sum of all samples for a counter family.
func gatherCounter(t *testing.T, m *metrics.Metrics, name string) float64 {
	t.Helper()
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			var total float64
			for _, metric := range f.GetMetric() {
				if metric.GetCounter() != nil {
					total += metric.GetCounter().GetValue()
				}
				if metric.GetUntyped() != nil {
					total += metric.GetUntyped().GetValue()
				}
			}
			return total
		}
	}
	return 0
}
