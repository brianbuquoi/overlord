// Package tracing provides OpenTelemetry instrumentation for Orcastrator.
// All state is held in a Tracer struct — no global state or init() functions.
package tracing

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Config mirrors the YAML observability.tracing block.
type Config struct {
	Enabled      bool
	Exporter     string // "stdout" or "otlp"
	OTLPEndpoint string // e.g. "localhost:4317"
	OTLPInsecure bool   // if true, disable TLS for OTLP (e.g. local collector)
	OTLPHeaders  string // comma-separated key=value pairs for OTLP auth headers (read from env var)
}

// Tracer wraps an OpenTelemetry TracerProvider. Pass through DI.
type Tracer struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	prop     propagation.TextMapPropagator
}

// New creates a Tracer from the given config. If tracing is disabled,
// returns a no-op Tracer that is safe to call.
func New(ctx context.Context, cfg Config) (*Tracer, error) {
	if !cfg.Enabled {
		return newNoop(), nil
	}

	var exporter sdktrace.SpanExporter
	var err error

	switch cfg.Exporter {
	case "otlp":
		if cfg.OTLPEndpoint == "" {
			return nil, fmt.Errorf("tracing: otlp exporter requires otlp_endpoint")
		}
		opts := []otlptracegrpc.Option{
			otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
		}
		if cfg.OTLPInsecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		if cfg.OTLPHeaders != "" {
			headers := parseOTLPHeaders(cfg.OTLPHeaders)
			if len(headers) > 0 {
				opts = append(opts, otlptracegrpc.WithHeaders(headers))
			}
		}
		exporter, err = otlptracegrpc.New(ctx, opts...)
	case "stdout", "":
		exporter, err = stdouttrace.New(stdouttrace.WithPrettyPrint())
	default:
		return nil, fmt.Errorf("tracing: unknown exporter %q (valid: stdout, otlp)", cfg.Exporter)
	}
	if err != nil {
		return nil, fmt.Errorf("tracing: create exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("orcastrator"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	prop := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)

	// Set global propagator so HTTP middleware can extract traceparent.
	otel.SetTextMapPropagator(prop)

	return &Tracer{
		provider: tp,
		tracer:   tp.Tracer("orcastrator"),
		prop:     prop,
	}, nil
}

// NewWithExporter creates a Tracer using the given SpanExporter. This is
// intended for testing — callers can pass a tracetest.InMemoryExporter to
// capture spans without network I/O.
func NewWithExporter(ctx context.Context, exporter sdktrace.SpanExporter) (*Tracer, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String("orcastrator"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: create resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithResource(res),
	)

	prop := propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)

	return &Tracer{
		provider: tp,
		tracer:   tp.Tracer("orcastrator"),
		prop:     prop,
	}, nil
}

// newNoop returns a Tracer backed by the no-op provider.
func newNoop() *Tracer {
	return &Tracer{
		tracer: noop.NewTracerProvider().Tracer("orcastrator"),
		prop:   propagation.NewCompositeTextMapPropagator(),
	}
}

// ForceFlush synchronously flushes all pending spans to the configured exporter
// without shutting down the TracerProvider. Call this before process exit in
// short-lived commands (e.g. submit, status) to ensure spans are exported, or
// at the end of a test to verify captured spans. Safe to call on a no-op Tracer.
func (t *Tracer) ForceFlush(ctx context.Context) error {
	if t.provider != nil {
		return t.provider.ForceFlush(ctx)
	}
	return nil
}

// Shutdown flushes pending spans. Safe to call on a no-op Tracer.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if t.provider != nil {
		return t.provider.Shutdown(ctx)
	}
	return nil
}

// StartTaskSpan creates the root span for a task.
func (t *Tracer) StartTaskSpan(ctx context.Context, taskID, pipelineID string) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, "orcastrator.task",
		trace.WithAttributes(
			attribute.String("task_id", taskID),
			attribute.String("pipeline_id", pipelineID),
		),
	)
}

// StartStageSpan creates a child span for a stage execution.
func (t *Tracer) StartStageSpan(ctx context.Context, stageID, agentID string, attempt int) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, "orcastrator.stage",
		trace.WithAttributes(
			attribute.String("stage_id", stageID),
			attribute.String("agent_id", agentID),
			attribute.Int("attempt", attempt),
		),
	)
}

// StartAgentSpan creates a child span for an LLM API call.
func (t *Tracer) StartAgentSpan(ctx context.Context, provider, model string) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, "orcastrator.agent.execute",
		trace.WithAttributes(
			attribute.String("provider", provider),
			attribute.String("model", model),
		),
	)
}

// Extract reads W3C traceparent from an HTTP request into a context.
func (t *Tracer) Extract(ctx context.Context, carrier propagation.TextMapCarrier) context.Context {
	return t.prop.Extract(ctx, carrier)
}

// SpanMeta returns the trace_id and span_id for storing in task metadata.
// Returns nil if the span is not recording.
func SpanMeta(ctx context.Context) map[string]any {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return map[string]any{
		"trace_id": sc.TraceID().String(),
		"span_id":  sc.SpanID().String(),
	}
}

// Propagator returns the text map propagator for middleware use.
func (t *Tracer) Propagator() propagation.TextMapPropagator {
	return t.prop
}

// parseOTLPHeaders parses a comma-separated list of key=value pairs into a map.
// Used to pass authentication headers (e.g. "Authorization=Bearer token") to
// the OTLP exporter from an environment variable.
func parseOTLPHeaders(raw string) map[string]string {
	headers := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if ok && k != "" {
			headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return headers
}
