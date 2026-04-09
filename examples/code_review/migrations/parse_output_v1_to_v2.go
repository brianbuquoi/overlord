package migrations

import (
	"context"
	"encoding/json"

	"github.com/orcastrator/orcastrator/internal/migration"
)

// ParseOutputV1ToV2 migrates the parse_output schema from v1 to v2.
// It adds the required "language_breakdown" field, defaulting to an empty map
// for tasks that predate the v2 schema.
var ParseOutputV1ToV2 = &migration.FuncMigration{
	Schema: "parse_output",
	From:   "v1",
	To:     "v2",
	Fn: func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
		var data map[string]any
		if err := json.Unmarshal(payload, &data); err != nil {
			return nil, err
		}

		// Add the new required field with a safe default.
		if _, ok := data["language_breakdown"]; !ok {
			data["language_breakdown"] = map[string]any{}
		}

		return json.Marshal(data)
	},
}

// Register adds the parse_output v1→v2 migration to the given registry.
func Register(r *migration.Registry) {
	r.MustRegister(ParseOutputV1ToV2)
}
