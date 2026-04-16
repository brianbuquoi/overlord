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
