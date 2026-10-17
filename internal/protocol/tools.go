package protocol

import (
	"encoding/json"
	"fmt"
	"strings"

	"opencode2api/internal/jsonutil"
)

// ToolAlias records how a provider-visible function maps back to the tool the
// client declared. Providers other than Responses accept function tools only,
// so namespaced declarations are re-encoded for the upstream and restored for
// the client on the way back.
type ToolAlias struct {
	Name      string // client-visible tool name
	Namespace string // enclosing namespace, empty for top-level tools
	// Custom marks a freeform custom tool (apply_patch) that was re-encoded as
	// a function tool for a provider that only accepts function tools. The
	// response path restores the custom_tool_call item shape.
	Custom bool
}

// ToolAliases maps provider-visible function names to their client identity.
// A nil map means the request keeps the client's declarations unchanged.
type ToolAliases map[string]ToolAlias

// ProviderName returns the name the upstream sees for a client tool. A tool
// that was never aliased keeps its own name.
func (aliases ToolAliases) ProviderName(namespace, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if namespace == "" {
		return name
	}
	for providerName, alias := range aliases {
		if alias.Name == name && alias.Namespace == namespace {
			return providerName
		}
	}
	return name
}

// apply returns the client-visible name and namespace of a provider tool call.
func (aliases ToolAliases) apply(providerName string) (string, string) {
	if alias, ok := aliases[providerName]; ok {
		return alias.Name, alias.Namespace
	}
	return providerName, ""
}

// customTool reports whether the provider-visible function is a freeform custom
// tool the request path re-encoded for a functions-only provider.
func (aliases ToolAliases) customTool(providerName string) bool {
	alias, ok := aliases[providerName]
	return ok && alias.Custom
}

// qualifiedToolName is the provider-visible name of a namespace child, used
// only when the plain child name is already taken by another declaration.
func qualifiedToolName(namespace, name string) string {
	namespace = strings.TrimSpace(namespace)
	name = strings.TrimSpace(name)
	if namespace == "" {
		return name
	}
	return namespace + "__" + name
}

// responsesToolsForProvider converts a client Responses tools array into the
// function-tool list a chat or Anthropic provider accepts.
//
// Namespace children become individually callable functions: the provider sees
// the child's own name and the gateway restores the namespace on the response
// path, so a Codex sub-agent call survives the round trip. Freeform custom
// tools (apply_patch) are re-encoded as function tools with a JSON input
// field, mirroring CPA's conversion, and restored as custom_tool_call items on
// the response path. Other declarations without a function equivalent — hosted
// tools such as web_search — are reported as dropped instead of failing the
// request.
func responsesToolsForProvider(raw []any) ([]bridgeTool, ToolAliases, []string) {
	tools := make([]bridgeTool, 0, len(raw))
	aliases := ToolAliases{}
	dropped := make([]string, 0)
	used := make(map[string]bool, len(raw))
	add := func(name, description string, schema any, strict bool, namespace string, aliasName string) string {
		providerName := name
		for suffix := 2; used[providerName]; suffix++ {
			providerName = fmt.Sprintf("%s_%d", name, suffix)
		}
		used[providerName] = true
		if aliasName != "" {
			aliases[providerName] = ToolAlias{Name: aliasName, Namespace: namespace}
		}
		tools = append(tools, bridgeTool{Name: providerName, Description: description, Schema: schema, Strict: strict})
		return providerName
	}
	for _, entry := range raw {
		tool, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		switch jsonutil.StringAt(tool, "type") {
		case "", "function":
			name := jsonutil.StringAt(tool, "name")
			if name == "" {
				continue
			}
			add(name, jsonutil.StringAt(tool, "description"), tool["parameters"], jsonutil.BoolAt(tool, "strict"), "", name)
		case "namespace":
			namespace := jsonutil.StringAt(tool, "name")
			for _, childEntry := range jsonutil.SliceAt(tool, "tools") {
				child, ok := childEntry.(map[string]any)
				if !ok {
					continue
				}
				childName := jsonutil.StringAt(child, "name")
				if childName == "" {
					continue
				}
				if childType := jsonutil.StringAt(child, "type"); childType != "" && childType != "function" {
					if namespace != "" {
						dropped = append(dropped, qualifiedToolName(namespace, childName))
					} else {
						dropped = append(dropped, childName)
					}
					continue
				}
				description := jsonutil.StringAt(child, "description")
				if namespace != "" && !strings.Contains(description, namespace) {
					description = strings.TrimSpace(fmt.Sprintf("%s (sub-tool of %s)", description, namespace))
				}
				add(qualifiedToolName(namespace, childName), description, child["parameters"], jsonutil.BoolAt(child, "strict"), namespace, childName)
			}
		case "custom":
			// Freeform custom tools (apply_patch) have no function equivalent
			// upstream, so they are re-encoded as a function with a single
			// input string field and restored on the response path.
			name := jsonutil.StringAt(tool, "name")
			if name == "" {
				dropped = append(dropped, "custom")
				continue
			}
			description := jsonutil.StringAt(tool, "description")
			schema := any(json.RawMessage(customToolParametersJSON))
			if name == "apply_patch" {
				description = applyPatchDescription(tool)
				schema = json.RawMessage(applyPatchParametersJSON)
			}
			providerName := add(name, description, schema, false, "", "")
			aliases[providerName] = ToolAlias{Name: name, Custom: true}
		default:
			name := jsonutil.StringAt(tool, "name")
			if name == "" {
				name = jsonutil.StringAt(tool, "type")
			}
			if name != "" {
				dropped = append(dropped, name)
			}
		}
	}
	if len(tools) == 0 {
		return nil, aliases, dropped
	}
	if len(dropped) == 0 {
		dropped = nil
	}
	return tools, aliases, dropped
}

// responsesFunctionCallItem renders a provider tool call as the Responses
// function_call item the client declared, restoring a namespace when the call
// belongs to a namespaced tool. Codex identifies a sub-agent call by that
// namespace, so losing it turns a working call into an unknown tool.
func responsesFunctionCallItem(aliases ToolAliases, itemID, providerName, callID, status, arguments string) map[string]any {
	name, namespace := aliases.apply(providerName)
	item := map[string]any{
		"id":        itemID,
		"type":      "function_call",
		"status":    status,
		"call_id":   callID,
		"name":      name,
		"arguments": arguments,
	}
	if namespace != "" {
		item["namespace"] = namespace
	}
	return item
}

// responsesCustomToolCallItem renders a converted freeform tool call as the
// Responses custom_tool_call item the client declared.
func responsesCustomToolCallItem(aliases ToolAliases, itemID, providerName, callID, status, input string) map[string]any {
	name, namespace := aliases.apply(providerName)
	if name == "" {
		name = providerName
	}
	item := map[string]any{
		"id":      itemID,
		"type":    "custom_tool_call",
		"status":  status,
		"call_id": callID,
		"name":    name,
		"input":   input,
	}
	if namespace != "" {
		item["namespace"] = namespace
	}
	return item
}

// Freeform custom tools reach functions-only providers as function tools with
// a JSON input field. The schema and patch instructions mirror CPA's
// conversion (internal/client/codex/apply-patch) so the model keeps producing
// the Codex patch grammar.
const applyPatchParametersJSON = `{"type":"object","properties":{"input":{"type":"string","description":"The complete apply_patch patch text."}},"required":["input"],"additionalProperties":false}`

const applyPatchInstructions = `Call this function with a JSON object whose input field contains the complete patch text.
Use the Codex apply_patch format, not a conventional git unified diff.
Start with *** Begin Patch and end with *** End Patch.
Use *** Add File: path, *** Delete File: path, or *** Update File: path.
Every added-file content line starts with +.
For updates, use @@; context lines start with one space, removed lines with -, and added lines with +.
Use *** Move to: path for a rename and *** End of File when required by the patch grammar.
Example input:
*** Begin Patch
*** Update File: src/main.go
@@
-old
+new
*** End Patch`

const customToolParametersJSON = `{"type":"object","properties":{"input":{"type":"string"}},"required":["input"]}`

const freeformToolSentence = "This is a FREEFORM tool, so do not wrap the patch in JSON."

// applyPatchDescription rewrites the freeform apply_patch description for a
// provider that receives the tool as a JSON-schema function.
func applyPatchDescription(tool map[string]any) string {
	original := strings.ReplaceAll(jsonutil.StringAt(tool, "description"), freeformToolSentence, "")
	var description strings.Builder
	if trimmed := strings.TrimSpace(original); trimmed != "" {
		description.WriteString(trimmed)
		description.WriteString("\n\n")
	}
	description.WriteString(applyPatchInstructions)
	grammar := jsonutil.StringAt(tool, "format", "definition")
	if grammar != "" {
		if strings.Contains(grammar, "*** Environment ID:") {
			description.WriteString("\n\nUse *** Environment ID: as specified by the patch grammar.")
		}
		description.WriteString("\n\nOriginal patch grammar:\n")
		description.WriteString(grammar)
	}
	return description.String()
}

// wrapCustomToolArguments encodes freeform tool input as the function arguments
// object the provider saw; the history conversion sends it back upstream.
func wrapCustomToolArguments(input string) string {
	encoded, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		return `{"input":""}`
	}
	return string(encoded)
}

// unwrapCustomToolArguments decodes provider function arguments back into
// freeform input. A provider that ignored the wrapper (plain patch text or a
// bare JSON string) is accepted as-is instead of failing the turn.
func unwrapCustomToolArguments(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" || trimmed == "{}" {
		return ""
	}
	var payload struct {
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal([]byte(trimmed), &payload) == nil && len(payload.Input) > 0 {
		var text string
		if json.Unmarshal(payload.Input, &text) == nil {
			return text
		}
		return string(payload.Input)
	}
	var text string
	if json.Unmarshal([]byte(trimmed), &text) == nil {
		return text
	}
	return arguments
}

// collectResponsesNamespaces records namespaced tool declarations under the
// dotted name a Responses provider uses for namespace children
// ("multi_agent_v1.spawn_agent"). Such a provider accepts the namespace
// declaration but answers with the dotted name, which the client cannot resolve.
func collectResponsesNamespaces(tools []any) ToolAliases {
	namespaces := ToolAliases{}
	for _, entry := range tools {
		tool, ok := entry.(map[string]any)
		if !ok || jsonutil.StringAt(tool, "type") != "namespace" {
			continue
		}
		namespace := jsonutil.StringAt(tool, "name")
		if namespace == "" {
			continue
		}
		for _, childEntry := range jsonutil.SliceAt(tool, "tools") {
			child, ok := childEntry.(map[string]any)
			if !ok {
				continue
			}
			childName := jsonutil.StringAt(child, "name")
			if childName == "" {
				continue
			}
			if childType := jsonutil.StringAt(child, "type"); childType != "" && childType != "function" {
				continue
			}
			namespaces[namespace+"."+childName] = ToolAlias{Name: childName, Namespace: namespace}
		}
	}
	if len(namespaces) == 0 {
		return nil
	}
	return namespaces
}

// restoreResponsesNamespace rewrites a provider call named "namespace.child"
// into the name and namespace pair the client declared. It reports whether the
// call belonged to a declared namespace.
func restoreResponsesNamespace(namespaces ToolAliases, item map[string]any) bool {
	name := jsonutil.StringAt(item, "name")
	alias, ok := namespaces[name]
	if !ok {
		return false
	}
	item["name"] = alias.Name
	item["namespace"] = alias.Namespace
	return true
}

// rewriteResponsesPassthroughRequest adapts a Responses request that is relayed
// to a Responses provider verbatim: namespaced calls in the history are
// rewritten to the provider's dotted form so they match what the provider
// produced, and declarations the provider rejects are dropped. It returns the
// dropped declarations.
func rewriteResponsesPassthroughRequest(input map[string]any, namespaces ToolAliases) []string {
	dropped := make([]string, 0)
	if tools := jsonutil.SliceAt(input, "tools"); tools != nil {
		kept := make([]any, 0, len(tools))
		for _, entry := range tools {
			tool, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			switch jsonutil.StringAt(tool, "type") {
			case "web_search":
				// Zen accepts this option only on web_search_preview, while
				// Codex also sends it with the current web_search tool.
				delete(tool, "search_content_types")
				kept = append(kept, tool)
			case "local_shell", "custom":
				// Zen rejects both with "not supported on this endpoint";
				// forwarding them fails the whole request.
				name := jsonutil.StringAt(tool, "name")
				if name == "" {
					name = jsonutil.StringAt(tool, "type")
				}
				dropped = append(dropped, name)
			default:
				kept = append(kept, tool)
			}
		}
		input["tools"] = kept
	}
	for _, raw := range jsonutil.SliceAt(input, "input") {
		item, ok := raw.(map[string]any)
		if !ok || jsonutil.StringAt(item, "type") != "function_call" {
			continue
		}
		namespace := jsonutil.StringAt(item, "namespace")
		name := jsonutil.StringAt(item, "name")
		if namespace == "" || name == "" {
			continue
		}
		if providerName := namespaces.ProviderName(namespace, name); providerName != name {
			item["name"] = providerName
		} else {
			item["name"] = namespace + "." + name
		}
		delete(item, "namespace")
	}
	if choice, ok := input["tool_choice"].(map[string]any); ok {
		if namespace := jsonutil.StringAt(choice, "namespace"); namespace != "" {
			if name := jsonutil.StringAt(choice, "name"); name != "" {
				choice["name"] = namespace + "." + name
				delete(choice, "namespace")
			}
		}
	}
	return dropped
}
