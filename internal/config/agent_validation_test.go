package config

import (
	"strings"
	"testing"
)

func TestValidateAgents_FixturesOnMockAllowed(t *testing.T) {
	cfg := &Config{
		Agents: []Agent{
			{
				ID:       "reviewer-mock",
				Provider: "mock",
				Fixtures: map[string]string{"review": "fixtures/review.json"},
			},
		},
	}
	if err := validateAgents(cfg.Agents); err != nil {
		t.Fatalf("expected no error for mock+fixtures; got %v", err)
	}
}

func TestValidateAgents_FixturesOnNonMockRejected(t *testing.T) {
	cases := []string{"anthropic", "openai", "openai-responses", "google", "ollama", "copilot", "plugin", "some-plugin"}
	for _, provider := range cases {
		t.Run(provider, func(t *testing.T) {
			cfg := &Config{
				Agents: []Agent{
					{
						ID:       "reviewer",
						Provider: provider,
						Fixtures: map[string]string{"review": "fixtures/review.json"},
					},
				},
			}
			err := validateAgents(cfg.Agents)
			if err == nil {
				t.Fatalf("expected error for provider=%q with fixtures; got nil", provider)
			}
			if !strings.Contains(err.Error(), "reviewer") {
				t.Errorf("expected error to name agent id; got %q", err.Error())
			}
			if !strings.Contains(err.Error(), provider) {
				t.Errorf("expected error to name provider; got %q", err.Error())
			}
			if !strings.Contains(err.Error(), "fixtures") {
				t.Errorf("expected error to mention fixtures; got %q", err.Error())
			}
		})
	}
}

func TestValidateAgents_NoFixturesNoError(t *testing.T) {
	cfg := &Config{
		Agents: []Agent{
			{ID: "reviewer", Provider: "anthropic"},
		},
	}
	if err := validateAgents(cfg.Agents); err != nil {
		t.Fatalf("expected no error; got %v", err)
	}
}

func TestValidateAgents_EmptyFixturesMapAllowed(t *testing.T) {
	cfg := &Config{
		Agents: []Agent{
			{ID: "reviewer", Provider: "anthropic", Fixtures: map[string]string{}},
		},
	}
	if err := validateAgents(cfg.Agents); err != nil {
		t.Fatalf("expected no error for empty fixtures map; got %v", err)
	}
}

// SEC4-006 regression. A system_prompt that stays under the 512 KiB
// ceiling passes validation; one that crosses it is rejected with a
// message naming the offending agent and pointing at the gap ID.

func TestValidateAgents_SystemPromptAtCeilingAccepted(t *testing.T) {
	cfg := &Config{
		Agents: []Agent{
			{
				ID:           "reviewer",
				Provider:     "anthropic",
				SystemPrompt: strings.Repeat("x", MaxSystemPromptBytes),
			},
		},
	}
	if err := validateAgents(cfg.Agents); err != nil {
		t.Fatalf("system_prompt at exactly the ceiling must be accepted; got %v", err)
	}
}

func TestValidateAgents_SystemPromptOverCeilingRejected(t *testing.T) {
	cfg := &Config{
		Agents: []Agent{
			{
				ID:           "reviewer",
				Provider:     "anthropic",
				SystemPrompt: strings.Repeat("x", MaxSystemPromptBytes+1),
			},
		},
	}
	err := validateAgents(cfg.Agents)
	if err == nil {
		t.Fatal("system_prompt above the ceiling must be rejected; got nil")
	}
	if !strings.Contains(err.Error(), "reviewer") {
		t.Errorf("error should name the offending agent; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "system_prompt") {
		t.Errorf("error should mention system_prompt; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "SEC4-006") {
		t.Errorf("error should reference the gap ID so operators can find context; got %q", err.Error())
	}
}

func TestValidateAgents_SystemPromptOversizeNamesEveryHit(t *testing.T) {
	// Only one agent is reported per call (fail-fast); confirm it is the
	// first oversize agent in declaration order so diagnostic output is
	// deterministic.
	cfg := &Config{
		Agents: []Agent{
			{ID: "first-ok", Provider: "anthropic", SystemPrompt: "tiny"},
			{ID: "second-huge", Provider: "anthropic", SystemPrompt: strings.Repeat("y", MaxSystemPromptBytes+10)},
			{ID: "third-also-huge", Provider: "openai", SystemPrompt: strings.Repeat("z", MaxSystemPromptBytes+20)},
		},
	}
	err := validateAgents(cfg.Agents)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "second-huge") {
		t.Errorf("expected first oversize agent to be named; got %q", err.Error())
	}
}
