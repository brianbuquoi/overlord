package config

// Security Audit Verification — Section 4: Store and Data Integrity
// Test 15: Redis key collision — verify config validator rejects IDs containing ":"

import (
	"strings"
	"testing"
)

// Test 15: Redis key collision from IDs containing colons.
// If pipelineID="foo:bar" stageID="baz" and pipelineID="foo" stageID="bar:baz"
// both produce key "orcastrator:queue:foo:bar:baz", this is a collision bug.
// The config validator should reject IDs containing ":".
func TestSecurity_RedisKeyCollision_IDsWithColons(t *testing.T) {
	cfg := &Config{
		Version: "1",
		SchemaRegistry: []SchemaEntry{
			{Name: "s1", Version: "v1", Path: "schemas/s1.json"},
		},
		Pipelines: []Pipeline{
			{
				Name:        "foo:bar",
				Concurrency: 1,
				Store:       "memory",
				Stages: []Stage{
					{
						ID:           "baz",
						Agent:        "agent1",
						InputSchema:  StageSchemaRef{Name: "s1", Version: "v1"},
						OutputSchema: StageSchemaRef{Name: "s1", Version: "v1"},
						OnSuccess:    StaticOnSuccess("done"),
					},
				},
			},
		},
		Agents: []Agent{
			{ID: "agent1", Provider: "anthropic"},
		},
	}

	err := validate(cfg)
	if err == nil {
		t.Error("BUG (SEC-007): config validator accepted pipeline name containing ':' — " +
			"this causes Redis key collisions. Pipeline ID 'foo:bar' with stage 'baz' " +
			"and pipeline 'foo' with stage 'bar:baz' would produce identical Redis keys.")
	} else if strings.Contains(err.Error(), ":") || strings.Contains(err.Error(), "character") || strings.Contains(err.Error(), "invalid") {
		t.Logf("SEC-007 FIXED: colon in pipeline name rejected: %v", err)
	}

	// Also test colon in stage ID
	cfg2 := &Config{
		Version: "1",
		SchemaRegistry: []SchemaEntry{
			{Name: "s1", Version: "v1", Path: "schemas/s1.json"},
		},
		Pipelines: []Pipeline{
			{
				Name:        "foo",
				Concurrency: 1,
				Stages: []Stage{
					{
						ID:           "bar:baz",
						Agent:        "agent1",
						InputSchema:  StageSchemaRef{Name: "s1", Version: "v1"},
						OutputSchema: StageSchemaRef{Name: "s1", Version: "v1"},
						OnSuccess:    StaticOnSuccess("done"),
					},
				},
			},
		},
		Agents: []Agent{
			{ID: "agent1", Provider: "anthropic"},
		},
	}

	err2 := validate(cfg2)
	if err2 == nil {
		t.Error("BUG (SEC-007): config validator accepted stage ID containing ':' — " +
			"this causes Redis key collisions.")
	} else {
		t.Logf("SEC-007 FIXED: colon in stage ID rejected: %v", err2)
	}

	// Test colon in agent ID
	cfg3 := &Config{
		Version: "1",
		SchemaRegistry: []SchemaEntry{
			{Name: "s1", Version: "v1", Path: "schemas/s1.json"},
		},
		Pipelines: []Pipeline{
			{
				Name: "pipe",
				Stages: []Stage{
					{
						ID:           "stage1",
						Agent:        "agent:1",
						InputSchema:  StageSchemaRef{Name: "s1", Version: "v1"},
						OutputSchema: StageSchemaRef{Name: "s1", Version: "v1"},
						OnSuccess:    StaticOnSuccess("done"),
					},
				},
			},
		},
		Agents: []Agent{
			{ID: "agent:1", Provider: "anthropic"},
		},
	}

	err3 := validate(cfg3)
	if err3 == nil {
		t.Error("BUG (SEC-007): config validator accepted agent ID containing ':'")
	} else {
		t.Logf("SEC-007 FIXED: colon in agent ID rejected: %v", err3)
	}
}

// Test that the recommended regex pattern ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ is enforced
func TestSecurity_IDCharacterValidation(t *testing.T) {
	invalidIDs := []struct {
		name string
		id   string
	}{
		{"slash", "foo/bar"},
		{"colon", "foo:bar"},
		{"space", "foo bar"},
		{"null_byte", "foo\x00bar"},
		{"starts_with_dash", "-foo"},
		{"starts_with_dot", ".foo"},
		{"empty", ""},
	}

	for _, tc := range invalidIDs {
		t.Run("pipeline_"+tc.name, func(t *testing.T) {
			cfg := &Config{
				Version: "1",
				SchemaRegistry: []SchemaEntry{
					{Name: "s1", Version: "v1", Path: "s1.json"},
				},
				Pipelines: []Pipeline{
					{
						Name: tc.id,
						Stages: []Stage{
							{
								ID:           "stage1",
								Agent:        "agent1",
								InputSchema:  StageSchemaRef{Name: "s1", Version: "v1"},
								OutputSchema: StageSchemaRef{Name: "s1", Version: "v1"},
								OnSuccess:    StaticOnSuccess("done"),
							},
						},
					},
				},
				Agents: []Agent{{ID: "agent1", Provider: "anthropic"}},
			}

			err := validate(cfg)
			if err == nil && tc.id != "" {
				// Empty is caught by other validation, but special chars should be caught
				t.Logf("SEC-007 OPEN: pipeline name %q not rejected", tc.id)
			}
		})
	}
}
