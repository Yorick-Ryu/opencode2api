package protocol

import "opencode2api/internal/jsonutil"

// normalizeCodexAgentMessages makes Codex's inter-agent envelope portable
// before either Responses passthrough or conversion to another protocol.
// Only agent-message content is unwrapped; encrypted reasoning is unrelated.
func normalizeCodexAgentMessages(input map[string]any) {
	for _, raw := range jsonutil.SliceAt(input, "input") {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		delete(item, "internal_chat_message_metadata_passthrough")
		if jsonutil.StringAt(item, "type") != "agent_message" {
			continue
		}
		item["type"] = "message"
		item["role"] = "user"
		delete(item, "id")
		delete(item, "author")
		delete(item, "recipient")
		for _, rawPart := range jsonutil.SliceAt(item, "content") {
			part, ok := rawPart.(map[string]any)
			if !ok || jsonutil.StringAt(part, "type") != "encrypted_content" {
				continue
			}
			// Codex uses this wrapper for plaintext too, once the tool's
			// message schema no longer advertises encrypted arguments.
			if text, ok := part["encrypted_content"].(string); ok {
				part["type"] = "input_text"
				part["text"] = text
				delete(part, "encrypted_content")
			}
		}
	}
	for _, raw := range jsonutil.SliceAt(input, "tools") {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if jsonutil.StringAt(tool, "type") == "namespace" {
			for _, rawChild := range jsonutil.SliceAt(tool, "tools") {
				if child, ok := rawChild.(map[string]any); ok {
					removeCodexMessageEncryption(child)
				}
			}
		} else {
			removeCodexMessageEncryption(tool)
		}
	}
}

func removeCodexMessageEncryption(tool map[string]any) {
	switch jsonutil.StringAt(tool, "name") {
	case "spawn_agent", "send_message", "followup_task":
		if message := jsonutil.MapAt(tool, "parameters", "properties", "message"); message != nil {
			delete(message, "encrypted")
		}
	}
}
