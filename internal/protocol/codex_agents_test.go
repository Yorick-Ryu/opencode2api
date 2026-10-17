package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// This reproduces the failing Codex v2 child request and its follow-up, with
// the task carried in encrypted_content rather than in a normal user message.
func TestCodexAgentMessagesReachEveryProvider(t *testing.T) {
	for _, target := range []Protocol{Responses, Chat, Anthropic} {
		t.Run(string(target), func(t *testing.T) {
			input := decodeJSON(t, []byte(`{"model":"test","input":[
			 {"type":"message","role":"developer","content":[{"type":"input_text","text":"instructions"}]},
			 {"type":"agent_message","id":"amsg_task","author":"/root","recipient":"/root/child","internal_chat_message_metadata_passthrough":{"turn_id":"turn_1"},"content":[{"type":"input_text","text":"Task: "},{"type":"encrypted_content","encrypted_content":"Find Shenzhen weather"}]},
			 {"type":"agent_message","author":"/root","recipient":"/root/child","content":[{"type":"encrypted_content","encrypted_content":"Include the date"}]}
			],"client_metadata":{"x-openai-subagent":"child"},"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]}]}`))
			before, _ := json.Marshal(input)
			output, _, _, err := PrepareRequestWithTools(Responses, target, input, "https://example.test")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(output)
			for _, text := range []string{"Find Shenzhen weather", "Include the date"} {
				if !strings.Contains(string(body), text) {
					t.Fatalf("delegated task lost: %s", body)
				}
			}
			for _, field := range []string{`"agent_message"`, `"encrypted_content"`, `"encrypted":true`, `"author"`, `"recipient"`, `"amsg_task"`, `"internal_chat_message_metadata_passthrough"`} {
				if strings.Contains(string(body), field) {
					t.Fatalf("nonportable field %s: %s", field, body)
				}
			}
			if target == Responses {
				items := output["input"].([]any)
				for _, index := range []int{1, 2} {
					item := items[index].(map[string]any)
					if item["type"] != "message" || item["role"] != "user" {
						t.Fatalf("invalid child message: %v", item)
					}
				}
				if !reflect.DeepEqual(input["client_metadata"], output["client_metadata"]) {
					t.Fatal("subagent routing marker changed")
				}
			}
			after, _ := json.Marshal(input)
			if string(before) != string(after) {
				t.Fatal("request conversion mutated client transcript")
			}
		})
	}
}

func TestCodexAgentNormalizationPreservesReasoningAndToolResults(t *testing.T) {
	input := decodeJSON(t, []byte(`{"model":"test","input":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque-reasoning"},{"type":"function_call_output","call_id":"call_1","output":{"type":"agent_message","encrypted_content":"literal tool output"}}],"tools":[{"type":"function","name":"send_message","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}},{"type":"function","name":"unrelated","parameters":{"properties":{"message":{"encrypted":true}}}}]}`))
	output, _, _, err := ConvertRequestWithTools(Responses, Responses, input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input["input"], output["input"]) {
		t.Fatal("reasoning or tool result was rewritten")
	}
	tools := output["tools"].([]any)
	first, _ := json.Marshal(tools[0])
	second, _ := json.Marshal(tools[1])
	if strings.Contains(string(first), `"encrypted"`) || !strings.Contains(string(second), `"encrypted":true`) {
		t.Fatal("encryption schema change was not scoped to collaboration messages")
	}
}

func TestResponsesSearchToolKeepsProviderSupportedOptions(t *testing.T) {
	input := decodeJSON(t, []byte(`{"input":"Search Shenzhen weather","tools":[{"type":"web_search","search_content_types":["text","image"],"search_context_size":"low","user_location":{"type":"approximate","country":"CN"}},{"type":"web_search_preview","search_content_types":["text"]}]}`))
	output, _, _, err := ConvertRequestWithTools(Responses, Responses, input)
	if err != nil {
		t.Fatal(err)
	}
	tools := output["tools"].([]any)
	search := tools[0].(map[string]any)
	if _, exists := search["search_content_types"]; exists {
		t.Fatal("web_search still contains the provider-rejected option")
	}
	if search["search_context_size"] != "low" || search["user_location"] == nil {
		t.Fatal("supported search options were lost")
	}
	if tools[1].(map[string]any)["search_content_types"] == nil {
		t.Fatal("preview tool lost its supported option")
	}
	if input["tools"].([]any)[0].(map[string]any)["search_content_types"] == nil {
		t.Fatal("client tool declaration was mutated")
	}
}
