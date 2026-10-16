package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeJSON(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", string(body), err)
	}
	return out
}

// A Codex client declares sub-agent tools in a namespace and hosted tools such
// as web_search alongside plain functions. Providers that accept function tools
// only must still receive the sub-agent tools.
func TestResponsesToolsForProviderFlattensNamespaces(t *testing.T) {
	input := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": "hi",
		"tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
			map[string]any{
				"type": "namespace",
				"name": "multi_agent_v1",
				"tools": []any{
					map[string]any{"type": "function", "name": "spawn_agent", "description": "Spawn an agent.", "parameters": map[string]any{"type": "object"}},
					map[string]any{"type": "custom", "name": "namespace_custom"},
				},
			},
			map[string]any{"type": "web_search"},
			map[string]any{"type": "custom", "name": "apply_patch"},
		},
	}

	output, aliases, dropped, err := ConvertRequestWithTools(Responses, Chat, input)
	if err != nil {
		t.Fatalf("ConvertRequestWithTools: %v", err)
	}
	tools, _ := output["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected 2 provider tools, got %d: %v", len(tools), tools)
	}
	names := make([]string, 0, len(tools))
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		function, _ := tool["function"].(map[string]any)
		names = append(names, jsonString(function["name"]))
		if typ := jsonString(tool["type"]); typ != "function" {
			t.Fatalf("provider tool is not a function tool: %v", tool)
		}
	}
	if names[0] != "exec_command" || names[1] != "multi_agent_v1__spawn_agent" {
		t.Fatalf("unexpected provider tool names: %v", names)
	}
	if got := aliases["multi_agent_v1__spawn_agent"]; got.Name != "spawn_agent" || got.Namespace != "multi_agent_v1" {
		t.Fatalf("namespace alias missing: %v", aliases)
	}
	if len(dropped) != 3 {
		t.Fatalf("expected web_search, the custom child and apply_patch to be dropped, got %v", dropped)
	}
}

// The sub-agent name comes back from the provider without its namespace, so the
// response path has to restore it or Codex reports an unknown tool.
func TestConvertResponseRestoresNamespace(t *testing.T) {
	aliases := ToolAliases{"multi_agent_v1__spawn_agent": {Name: "spawn_agent", Namespace: "multi_agent_v1"}}
	body := []byte(`{
		"id": "chatcmpl_1",
		"model": "deepseek-v4.1-flash",
		"created": 1,
		"choices": [{"index": 0, "finish_reason": "tool_calls", "message": {"role": "assistant", "content": null, "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "multi_agent_v1__spawn_agent", "arguments": "{\"prompt\":\"go\"}"}}]}}]
	}`)

	out, err := ConvertResponse(Chat, Responses, body, aliases)
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	decoded := decodeJSON(t, out)
	output, _ := decoded["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected one output item, got %v", output)
	}
	item, _ := output[0].(map[string]any)
	if item["type"] != "function_call" || item["name"] != "spawn_agent" || item["namespace"] != "multi_agent_v1" {
		t.Fatalf("namespaced call not restored: %v", item)
	}
}

// A finished sub-agent turn returns as a namespaced call in the client's input
// history; the provider has to see the flattened name it was given.
func TestNamespacedCallInHistoryIsFlattened(t *testing.T) {
	input := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "spawn_agent", "namespace": "multi_agent_v1", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"},
		},
		"tools": []any{
			map[string]any{"type": "namespace", "name": "multi_agent_v1", "tools": []any{map[string]any{"type": "function", "name": "spawn_agent", "parameters": map[string]any{"type": "object"}}}},
		},
	}

	output, _, _, err := ConvertRequestWithTools(Responses, Chat, input)
	if err != nil {
		t.Fatalf("ConvertRequestWithTools: %v", err)
	}
	messages, _ := output["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("expected converted messages, got %v", output)
	}
	first, _ := messages[0].(map[string]any)
	calls, _ := first["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("expected one tool call in history, got %v", first)
	}
	call, _ := calls[0].(map[string]any)
	function, _ := call["function"].(map[string]any)
	if got := jsonString(function["name"]); got != "multi_agent_v1__spawn_agent" {
		t.Fatalf("history call kept the wrong name: %q", got)
	}
}

// Function-only requests keep their declarations untouched, including when a
// caller does not care about aliases.
func TestFunctionToolsPassThroughUnchanged(t *testing.T) {
	input := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": "hi",
		"tools": []any{map[string]any{"type": "function", "name": "exec_command", "description": "run", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
	}

	output, aliases, dropped, err := ConvertRequestWithTools(Responses, Chat, input)
	if err != nil {
		t.Fatalf("ConvertRequestWithTools: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("nothing should be dropped: %v", dropped)
	}
	if got := aliases["exec_command"]; got.Name != "exec_command" {
		t.Fatalf("function alias missing: %v", aliases)
	}
	tools, _ := output["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	function, _ := tool["function"].(map[string]any)
	if jsonString(function["name"]) != "exec_command" {
		t.Fatalf("unexpected tool: %v", tool)
	}
}

func jsonString(value any) string {
	text, _ := value.(string)
	return text
}

// A Responses provider accepts a namespace declaration but answers with the
// dotted child name, which Codex cannot resolve against its tool registry.
func TestPassthroughRestoresDottedNamespaceCalls(t *testing.T) {
	input := map[string]any{
		"model": "muse-spark-1.3",
		"input": "spawn one agent",
		"tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}},
			map[string]any{
				"type":        "namespace",
				"name":        "multi_agent_v1",
				"description": "Sub-agent tools.",
				"tools": []any{
					map[string]any{"type": "function", "name": "spawn_agent", "parameters": map[string]any{"type": "object"}},
				},
			},
			map[string]any{"type": "local_shell"},
		},
	}

	output, namespaces, dropped, err := ConvertRequestWithTools(Responses, Responses, input)
	if err != nil {
		t.Fatalf("ConvertRequestWithTools: %v", err)
	}
	if _, ok := namespaces["multi_agent_v1.spawn_agent"]; !ok {
		t.Fatalf("namespace mapping missing: %v", namespaces)
	}
	if len(dropped) != 1 || dropped[0] != "local_shell" {
		t.Fatalf("local_shell should be dropped, got %v", dropped)
	}
	tools, _ := output["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected the namespace declaration to survive, got %v", tools)
	}

	// The provider answers with the dotted name in every event that names a call.
	for _, event := range []string{
		`{"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","name":"multi_agent_v1.spawn_agent","call_id":"call_1"}}`,
		`{"type":"response.function_call_arguments.done","name":"multi_agent_v1.spawn_agent","arguments":"{}"}`,
		`{"type":"response.completed","response":{"output":[{"id":"fc_1","type":"function_call","name":"multi_agent_v1.spawn_agent","call_id":"call_1"}]}}`,
	} {
		rewritten := RewriteResponsesDocument([]byte(`{"output":[{"type":"function_call","name":"multi_agent_v1.spawn_agent","call_id":"call_1"}]}`), namespaces)
		decoded := decodeJSON(t, rewritten)
		items, _ := decoded["output"].([]any)
		item, _ := items[0].(map[string]any)
		if item["name"] != "spawn_agent" || item["namespace"] != "multi_agent_v1" {
			t.Fatalf("document rewrite failed: %v", item)
		}

		rewriter := newResponsesPassthroughRewriter(namespaces)
		line := rewriter.rewriteLine([]byte("data: " + event + "\n"))
		if string(line) == "data: "+event+"\n" {
			t.Fatalf("event was not rewritten: %s", event)
		}
		if !strings.Contains(string(line), `"name":"spawn_agent"`) || !strings.Contains(string(line), `"namespace":"multi_agent_v1"`) {
			t.Fatalf("rewritten event lost the namespace: %s", line)
		}
	}
}

// The client's next turn carries the restored name and namespace; the provider
// has to see the dotted form it produced.
func TestPassthroughRewritesNamespacedHistory(t *testing.T) {
	input := map[string]any{
		"model": "muse-spark-1.3",
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "spawn_agent", "namespace": "multi_agent_v1", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "hi"},
		},
		"tools": []any{
			map[string]any{
				"type": "namespace", "name": "multi_agent_v1", "description": "Sub-agent tools.",
				"tools": []any{map[string]any{"type": "function", "name": "spawn_agent", "parameters": map[string]any{"type": "object"}}},
			},
		},
	}

	output, _, _, err := ConvertRequestWithTools(Responses, Responses, input)
	if err != nil {
		t.Fatalf("ConvertRequestWithTools: %v", err)
	}
	items, _ := output["input"].([]any)
	call, _ := items[0].(map[string]any)
	if call["name"] != "multi_agent_v1.spawn_agent" {
		t.Fatalf("history call kept the client name: %v", call)
	}
	if _, exists := call["namespace"]; exists {
		t.Fatalf("history call still carries the namespace field: %v", call)
	}
}

// Events and documents without namespaced calls must be forwarded untouched.
func TestPassthroughLeavesOtherPayloadsAlone(t *testing.T) {
	namespaces := ToolAliases{"multi_agent_v1.spawn_agent": {Name: "spawn_agent", Namespace: "multi_agent_v1"}}
	rewriter := newResponsesPassthroughRewriter(namespaces)
	line := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"multi_agent_v1.spawn_agent\"}\n")
	if got := string(rewriter.rewriteLine(line)); got != string(line) {
		t.Fatalf("delta event should pass through: %s", got)
	}
	document := []byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`)
	if got := string(RewriteResponsesDocument(document, namespaces)); got != string(document) {
		t.Fatalf("document should pass through: %s", got)
	}
}
