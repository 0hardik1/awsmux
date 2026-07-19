package mcpserver

import (
	"encoding/json"
	"testing"
)

func TestUnmarshalArgsRejectsUnknownFields(t *testing.T) {
	var a struct {
		Profiles []string `json:"profiles"`
	}
	// A typo like "profile" must be an error: unmarshalled silently it would
	// leave an empty selector, whose documented meaning is "all profiles".
	if err := unmarshalArgs(json.RawMessage(`{"profile": ["prod-*"]}`), &a); err == nil {
		t.Error("misspelled field should be rejected, not ignored")
	}
	if err := unmarshalArgs(json.RawMessage(`{"profiles": ["prod-*"]}`), &a); err != nil {
		t.Errorf("valid args rejected: %v", err)
	}
	if err := unmarshalArgs(nil, &a); err != nil {
		t.Errorf("absent args rejected: %v", err)
	}
}

func TestSchemasForbidAdditionalProperties(t *testing.T) {
	for _, td := range tools {
		if got, ok := td.InputSchema["additionalProperties"].(bool); !ok || got {
			t.Errorf("tool %s: inputSchema must set additionalProperties false", td.Name)
		}
	}
}
