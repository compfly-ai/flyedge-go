package enforce

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOperationDoesNotSerializeDestinationMetadata(t *testing.T) {
	body, err := json.Marshal(Operation{Type: "tool.call", ToolName: "fetch_url"})
	if err != nil {
		t.Fatalf("marshal operation: %v", err)
	}
	if strings.Contains(string(body), "dest_domain") || strings.Contains(string(body), "dest_type") {
		t.Fatalf("operation emitted retired destination metadata: %s", body)
	}
}
