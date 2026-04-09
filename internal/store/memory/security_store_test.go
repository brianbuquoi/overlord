package memory

// Security Audit Verification — Section 4: Store and Data Integrity (Memory Store)
// Test 14: Payload round-trip integrity

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/orcastrator/orcastrator/internal/broker"
)

func TestPayloadRoundTrip_LargeJSON(t *testing.T) {
	st := New()
	ctx := context.Background()

	// Create exactly 1MB of valid JSON
	// Build a JSON object with a large string value
	padding := make([]byte, 1024*1024-20) // leave room for JSON wrapper
	for i := range padding {
		padding[i] = 'A' + byte(i%26)
	}
	payload := json.RawMessage(`{"data":"` + string(padding) + `"}`)

	task := &broker.Task{
		ID:         "roundtrip-1mb",
		PipelineID: "test",
		StageID:    "stage1",
		Payload:    payload,
		Metadata:   map[string]any{},
		State:      broker.TaskStatePending,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	if err := st.EnqueueTask(ctx, "stage1", task); err != nil {
		t.Fatal(err)
	}

	dequeued, err := st.DequeueTask(ctx, "stage1")
	if err != nil {
		t.Fatal(err)
	}

	if len(dequeued.Payload) != len(payload) {
		t.Errorf("payload length mismatch: got %d, want %d", len(dequeued.Payload), len(payload))
	}
	for i := range payload {
		if payload[i] != dequeued.Payload[i] {
			t.Errorf("payload differs at byte %d: got %d, want %d", i, dequeued.Payload[i], payload[i])
			break
		}
	}
}

func TestPayloadRoundTrip_SpecialCharacters(t *testing.T) {
	st := New()
	ctx := context.Background()

	cases := []struct {
		name    string
		payload string
	}{
		{
			"null_bytes",
			`{"data":"before\u0000after"}`,
		},
		{
			"emoji",
			`{"data":"𝕳𝖊𝖑𝖑𝖔 🌍"}`,
		},
		{
			"rtl_unicode",
			`{"data":"مرحبا"}`,
		},
		{
			"ascii_control_chars",
			`{"data":"tab\there\nnewline\rcarriage"}`,
		},
		{
			"mixed_unicode",
			`{"data":"𝕳𝖊𝖑𝖑𝖔 مرحبا \u0000 🎉 \t\n"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := json.RawMessage(tc.payload)

			task := &broker.Task{
				ID:         "roundtrip-" + tc.name,
				PipelineID: "test",
				StageID:    "stage1",
				Payload:    payload,
				Metadata:   map[string]any{},
				State:      broker.TaskStatePending,
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
			}

			if err := st.EnqueueTask(ctx, "stage1", task); err != nil {
				t.Fatal(err)
			}

			dequeued, err := st.DequeueTask(ctx, "stage1")
			if err != nil {
				t.Fatal(err)
			}

			if string(dequeued.Payload) != tc.payload {
				t.Errorf("payload mismatch:\n  got:  %s\n  want: %s", dequeued.Payload, tc.payload)
			}
		})
	}
}
