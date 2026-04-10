package routing

import (
	"encoding/json"
	"fmt"
)

// RouteConfig describes how a stage routes on success. It is either a static
// string (backward compat) or a list of conditional routes with a default.
type RouteConfig struct {
	// Static is the target stage ID when no conditions are used.
	// Empty string means "done" (default behavior).
	Static string

	// Routes is the ordered list of conditional routes. Evaluated in order;
	// first match wins. Only set when using conditional routing.
	Routes []ConditionalRoute

	// Default is the fallback stage when no condition matches.
	// Required when Routes is non-empty.
	Default string

	// IsConditional is true when this config uses conditional routing.
	IsConditional bool
}

// ConditionalRoute pairs a parsed condition with a target stage.
type ConditionalRoute struct {
	Condition *Condition
	Stage     string
	RawExpr   string // original condition string from YAML
}

// Resolve evaluates the route config against the stage's output payload and
// returns the target stage ID.
//
// For static configs, returns the static string directly (no evaluation).
// For conditional configs, evaluates conditions in order and returns the
// first matching route's stage, or the default if no condition matches.
func Resolve(cfg RouteConfig, output json.RawMessage) (string, error) {
	if !cfg.IsConditional {
		return cfg.Static, nil
	}

	for _, route := range cfg.Routes {
		matched, err := route.Condition.Evaluate(output)
		if err != nil {
			return "", fmt.Errorf("routing condition evaluation failed for %q: %w", route.RawExpr, err)
		}
		if matched {
			return route.Stage, nil
		}
	}

	return cfg.Default, nil
}

// StaticRoute creates a RouteConfig for a simple static string target.
func StaticRoute(target string) RouteConfig {
	return RouteConfig{Static: target}
}
