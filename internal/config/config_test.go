package config

import (
	"testing"
)

func TestSubagentMarkerDefaultsAndRequiresModel(t *testing.T) {
	cfg := Config{Subagents: SubagentConfig{Model: "muse-spark-1.3-contributor-free"}}
	if got := cfg.Subagents.MarkerPath(); got != DefaultSubagentMarker {
		t.Fatalf("marker default = %q", got)
	}
	custom := Config{Subagents: SubagentConfig{Model: "x", Marker: " client_metadata.custom "}}
	if got := custom.Subagents.MarkerPath(); got != "client_metadata.custom" {
		t.Fatalf("marker path = %q", got)
	}
}
