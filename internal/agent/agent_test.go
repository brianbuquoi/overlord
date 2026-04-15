package agent

import (
	"encoding/json"
	"testing"
)

func TestParseJSONObjectOutput(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantPass  bool   // true: expect the input returned unchanged
		wantText  string // when wrapped: expected value of the "text" key
		wantExact string // when wantPass: expected raw bytes
	}{
		{
			name:      "valid object passes through",
			input:     `{"k":"v"}`,
			wantPass:  true,
			wantExact: `{"k":"v"}`,
		},
		{
			name:     "json array is wrapped",
			input:    `[1,2,3]`,
			wantText: `[1,2,3]`,
		},
		{
			name:     "plain string is wrapped",
			input:    `plain string`,
			wantText: `plain string`,
		},
		{
			name:     "empty string is wrapped",
			input:    ``,
			wantText: ``,
		},
		{
			name:     "json number is wrapped",
			input:    `42`,
			wantText: `42`,
		},
		{
			name:     "json boolean is wrapped",
			input:    `true`,
			wantText: `true`,
		},
		{
			name:      "deeply nested valid object passes through",
			input:     `{"a":{"b":{"c":[1,{"d":"e"}]}}}`,
			wantPass:  true,
			wantExact: `{"a":{"b":{"c":[1,{"d":"e"}]}}}`,
		},
		{
			name:      "code-fenced json object is unwrapped",
			input:     "```json\n{\"k\":\"v\"}\n```",
			wantPass:  true,
			wantExact: `{"k":"v"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseJSONObjectOutput(tc.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantPass {
				if string(got) != tc.wantExact {
					t.Fatalf("passthrough: got %s want %s", string(got), tc.wantExact)
				}
				return
			}
			var out map[string]any
			if err := json.Unmarshal(got, &out); err != nil {
				t.Fatalf("wrapped payload is not a JSON object: %v (raw %s)", err, got)
			}
			if out["text"] != tc.wantText {
				t.Fatalf("wrapped text: got %v want %q", out["text"], tc.wantText)
			}
		})
	}
}
