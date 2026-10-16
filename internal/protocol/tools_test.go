package protocol

import (
	"encoding/json"
	"net/http/httptest"
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
	if len(tools) != 3 {
		t.Fatalf("expected 3 provider tools, got %d: %v", len(tools), tools)
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
	if names[0] != "exec_command" || names[1] != "multi_agent_v1__spawn_agent" || names[2] != "apply_patch" {
		t.Fatalf("unexpected provider tool names: %v", names)
	}
	if got := aliases["multi_agent_v1__spawn_agent"]; got.Name != "spawn_agent" || got.Namespace != "multi_agent_v1" {
		t.Fatalf("namespace alias missing: %v", aliases)
	}
	if got := aliases["apply_patch"]; !got.Custom || got.Name != "apply_patch" {
		t.Fatalf("apply_patch custom alias missing: %v", aliases)
	}
	if len(dropped) != 2 {
		t.Fatalf("expected web_search and the custom namespace child to be dropped, got %v", dropped)
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

// Freeform custom tools must reach functions-only providers as function tools
// with an input string field, and come back as custom_tool_call items.
func TestResponsesToolsForProviderConvertsCustomTools(t *testing.T) {
	input := map[string]any{
		"model": "deepseek-v4.1-flash",
		"input": "hi",
		"tools": []any{
			map[string]any{
				"type":        "custom",
				"name":        "apply_patch",
				"description": "Use the patch tool. This is a FREEFORM tool, so do not wrap the patch in JSON. More text.",
				"format":      map[string]any{"type": "grammar", "syntax": "lark", "definition": "*** Begin Patch grammar"},
			},
			map[string]any{"type": "custom", "name": "notes_tool", "description": "Append a note."},
		},
	}
	output, aliases, dropped, err := ConvertRequestWithTools(Responses, Chat, input)
	if err != nil {
		t.Fatalf("ConvertRequestWithTools: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("custom tools should not be dropped: %v", dropped)
	}
	// Normalize through JSON like the gateway does before sending upstream, so
	// raw-message schemas become plain maps.
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		t.Fatalf("normalize output: %v", err)
	}
	tools, _ := normalized["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected 2 provider tools, got %d", len(tools))
	}
	patch, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if jsonString(patch["name"]) != "apply_patch" {
		t.Fatalf("unexpected patch function: %v", patch)
	}
	description := jsonString(patch["description"])
	if strings.Contains(description, "FREEFORM") {
		t.Fatalf("freeform sentence should be removed: %q", description)
	}
	if !strings.Contains(description, "*** Begin Patch") || !strings.Contains(description, "Original patch grammar") {
		t.Fatalf("patch instructions missing: %q", description)
	}
	schema, _ := patch["parameters"].(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	inputField, _ := properties["input"].(map[string]any)
	if jsonString(inputField["type"]) != "string" {
		t.Fatalf("apply_patch input schema missing: %v", schema)
	}
	notes, _ := tools[1].(map[string]any)["function"].(map[string]any)
	notesSchema, _ := notes["parameters"].(map[string]any)
	notesProperties, _ := notesSchema["properties"].(map[string]any)
	if _, ok := notesProperties["input"]; !ok {
		t.Fatalf("generic custom tool schema missing input: %v", notesSchema)
	}
	if got := aliases["apply_patch"]; !got.Custom || got.Name != "apply_patch" {
		t.Fatalf("apply_patch alias missing: %v", aliases)
	}
	if got := aliases["notes_tool"]; !got.Custom {
		t.Fatalf("notes_tool alias missing: %v", aliases)
	}
}

// The provider's wrapped function call must reach the client as a
// custom_tool_call item plus one raw input delta, not as function arguments.
func TestCustomToolCallStreamReachesClientAsCustomItem(t *testing.T) {
	aliases := ToolAliases{"apply_patch": {Name: "apply_patch", Custom: true}}
	recorder := httptest.NewRecorder()
	emitter := newBridgeStreamEmitter(recorder, recorder, Responses, "deepseek-v4.1-flash-free", aliases)
	patch := "*** Begin Patch\n*** Add File: a.txt\n+hi\n*** End Patch"
	wrapped, err := json.Marshal(map[string]string{"input": patch})
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	events := []bridgeStreamEvent{
		{Kind: "tool_start", ToolKey: "t1", ToolID: "call_1", ToolName: "apply_patch"},
		{Kind: "tool_delta", ToolKey: "t1", Text: string(wrapped[:11])},
		{Kind: "tool_delta", ToolKey: "t1", Text: string(wrapped[11:])},
	}
	for _, event := range events {
		if err := emitter.Emit(event); err != nil {
			t.Fatalf("emit %s: %v", event.Kind, err)
		}
	}
	if err := emitter.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}

	type frame struct {
		Type     string         `json:"type"`
		Delta    string         `json:"delta"`
		Item     map[string]any `json:"item"`
		Response struct {
			Output []map[string]any `json:"output"`
		} `json:"response"`
	}
	frames := make([]frame, 0)
	for _, block := range strings.Split(recorder.Body.String(), "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var parsed frame
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &parsed); err != nil {
				continue
			}
			frames = append(frames, parsed)
		}
	}
	if len(frames) == 0 {
		t.Fatalf("no SSE frames emitted: %s", recorder.Body.String())
	}
	var added, done *frame
	deltaSeen := false
	functionArgs := 0
	for i := range frames {
		current := &frames[i]
		switch current.Type {
		case "response.output_item.added":
			if jsonString(current.Item["type"]) == "custom_tool_call" {
				added = current
			}
		case "response.custom_tool_call_input.delta":
			if current.Delta == patch {
				deltaSeen = true
			}
		case "response.function_call_arguments.delta":
			functionArgs++
		case "response.output_item.done":
			if jsonString(current.Item["type"]) == "custom_tool_call" {
				done = current
			}
		case "response.completed":
			if len(current.Response.Output) != 1 || jsonString(current.Response.Output[0]["type"]) != "custom_tool_call" {
				t.Fatalf("terminal output should hold the custom item: %+v", current.Response.Output)
			}
			if got := jsonString(current.Response.Output[0]["input"]); got != patch {
				t.Fatalf("terminal custom input = %q, want the raw patch", got)
			}
		}
	}
	if added == nil || jsonString(added.Item["call_id"]) != "call_1" {
		t.Fatalf("custom_tool_call added frame missing: %+v", added)
	}
	if !deltaSeen {
		t.Fatalf("raw patch delta missing")
	}
	if functionArgs != 0 {
		t.Fatalf("function argument deltas must not be emitted for custom tools")
	}
	if done == nil {
		t.Fatalf("custom_tool_call done frame missing")
	}
	if got := jsonString(done.Item["input"]); got != patch {
		t.Fatalf("done input = %q, want the raw patch", got)
	}
	if got := jsonString(done.Item["status"]); got != "completed" {
		t.Fatalf("done status = %q", got)
	}
}

// Custom tool history replays upstream in the wrapped function form so the
// provider sees the same shape it produced.
func TestCustomToolHistoryRoundTrips(t *testing.T) {
	input := map[string]any{
		"model": "deepseek-v4.1-flash",
		"tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}},
		"input": []any{
			map[string]any{"type": "custom_tool_call", "call_id": "call_1", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch"},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": "Done!"},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
		},
	}
	output, _, _, err := ConvertRequestWithTools(Responses, Chat, input)
	if err != nil {
		t.Fatalf("ConvertRequestWithTools: %v", err)
	}
	messages, _ := output["messages"].([]any)
	var sawCall, sawResult bool
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) == 1 {
			call, _ := calls[0].(map[string]any)
			function, _ := call["function"].(map[string]any)
			if jsonString(function["name"]) != "apply_patch" {
				t.Fatalf("history call name = %v", function["name"])
			}
			wantArguments := "{\"input\":\"*** Begin Patch\\n*** End Patch\"}"
			if jsonString(function["arguments"]) != wantArguments {
				t.Fatalf("history arguments = %q, want %q", jsonString(function["arguments"]), wantArguments)
			}
			sawCall = true
		}
		if jsonString(message["role"]) == "tool" {
			if jsonString(message["tool_call_id"]) != "call_1" {
				t.Fatalf("tool result call id = %v", message["tool_call_id"])
			}
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("custom history did not round-trip: %v", messages)
	}
}
