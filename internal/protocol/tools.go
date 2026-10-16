package protocol

import (
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
// path, so a Codex sub-agent call survives the round trip. Declarations without
// a function equivalent — hosted tools such as web_search and custom
// (freeform) tools — are reported as dropped instead of failing the request.
func responsesToolsForProvider(raw []any) ([]bridgeTool, ToolAliases, []string) {
	tools := make([]bridgeTool, 0, len(raw))
	aliases := ToolAliases{}
	dropped := make([]string, 0)
	used := make(map[string]bool, len(raw))
	add := func(name, description string, schema any, strict bool, namespace string, aliasName string) {
		providerName := name
		for suffix := 2; used[providerName]; suffix++ {
			providerName = fmt.Sprintf("%s_%d", name, suffix)
		}
		used[providerName] = true
		if aliasName != "" {
			aliases[providerName] = ToolAlias{Name: aliasName, Namespace: namespace}
		}
		tools = append(tools, bridgeTool{Name: providerName, Description: description, Schema: schema, Strict: strict})
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
