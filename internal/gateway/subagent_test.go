package gateway

import (
	"testing"

	"opencode2api/internal/config"
	"opencode2api/internal/models"
)

func TestSubagentModelMatchesOnlyMarkedRequests(t *testing.T) {
	catalog := models.NewCatalog(config.TierZen, map[string]string{"muse-spark-1.3-contributor-free": "chat"})
	catalog.Replace([]string{"muse-spark-1.3-contributor-free"}, nil)
	gateway := &Gateway{cfg: config.Config{Subagents: config.SubagentConfig{Model: "muse-spark-1.3-contributor-free"}}, catalog: catalog}

	subagent := map[string]any{"model": "deepseek-v4.1-flash-free", "client_metadata": map[string]any{"x-openai-subagent": "true"}}
	if got := gateway.subagentModel(subagent); got != "muse-spark-1.3-contributor-free" {
		t.Fatalf("sub-agent request not remapped: %q", got)
	}
	main := map[string]any{"model": "deepseek-v4.1-flash-free", "client_metadata": map[string]any{"thread_id": "main"}}
	if got := gateway.subagentModel(main); got != "" {
		t.Fatalf("main thread request was remapped: %q", got)
	}
	empty := map[string]any{"client_metadata": map[string]any{"x-openai-subagent": ""}}
	if got := gateway.subagentModel(empty); got != "" {
		t.Fatalf("empty marker was treated as a sub-agent: %q", got)
	}
	gateway.cfg.Subagents = config.SubagentConfig{}
	if got := gateway.subagentModel(subagent); got != "" {
		t.Fatalf("remap ran while disabled: %q", got)
	}
	gateway.cfg.Subagents = config.SubagentConfig{Model: "not-exposed-model"}
	if got := gateway.subagentModel(subagent); got != "" {
		t.Fatalf("unroutable model must fall back to the client model: %q", got)
	}
}
