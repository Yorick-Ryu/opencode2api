package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// Rebuilding the HTTP request body from a websocket create mirrors the
// conversion CPA performs for accounts without a native websocket upstream:
// prewarm follow-ups and incremental requests are merged into the full
// transcript, while self-contained replacements are passed through.

var (
	errResponsesWSModelRequired    = errors.New("model is required")
	errResponsesWSInputNotArray    = errors.New("websocket request requires array field: input")
	errResponsesWSPreviousNotFound = errors.New("previous_response_id not found")
)

// responsesWSCreate is one parsed response.create message. NewAPI relays the
// flat form (request fields plus "type"); direct clients may wrap the request
// in "response" and carry the envelope fields outside.
type responsesWSCreate struct {
	EventID  string
	StreamID string
	Generate *bool
	Previous string
	Body     map[string]json.RawMessage
}

func parseResponsesWSCreate(message []byte) (*responsesWSCreate, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(message, &envelope); err != nil {
		return nil, errors.New("websocket message must be a JSON object")
	}
	create := &responsesWSCreate{Body: envelope}
	if raw, ok := envelope["event_id"]; ok {
		_ = json.Unmarshal(raw, &create.EventID)
	}
	if raw, ok := envelope["stream_id"]; ok {
		_ = json.Unmarshal(raw, &create.StreamID)
	}
	if raw, ok := envelope["generate"]; ok {
		var generate bool
		if json.Unmarshal(raw, &generate) == nil {
			create.Generate = &generate
		}
	}
	if raw, ok := envelope["response"]; ok && wsIsJSONObject(raw) {
		var inner map[string]json.RawMessage
		if json.Unmarshal(raw, &inner) == nil {
			create.Body = inner
		}
	}
	if raw, ok := create.Body["previous_response_id"]; ok {
		_ = json.Unmarshal(raw, &create.Previous)
	}
	create.Previous = strings.TrimSpace(create.Previous)
	create.EventID = strings.TrimSpace(create.EventID)
	create.StreamID = strings.TrimSpace(create.StreamID)
	return create, nil
}

// buildCreateBody decides how one create maps onto the upstream transcript and
// returns the normalized HTTP body. The caller holds the session mutex.
func (s *responsesWSSession) buildCreateBody(create *responsesWSCreate) ([]byte, string, error) {
	previous := create.Previous
	if previous != "" && s.prewarmID != "" && previous != s.prewarmID {
		return nil, "", errResponsesWSPreviousNotFound
	}
	rawInput, hasInput := create.Body["input"]
	if hasInput && !wsIsJSONArray(rawInput) {
		return nil, "", errResponsesWSInputNotArray
	}
	delta, err := wsDecodeItems(rawInput)
	if err != nil {
		return nil, "", errResponsesWSInputNotArray
	}
	switch {
	case s.prewarmID != "" && previous == s.prewarmID:
		previousItems, err := wsInputFromBody(s.lastRequest)
		if err != nil {
			return nil, "", err
		}
		merged, err := wsMergeInput(previousItems, nil, delta)
		if err != nil {
			return nil, "", err
		}
		body, err := normalizeResponsesWSBody(create.Body, s.lastRequest, merged)
		if err != nil {
			return nil, "", err
		}
		return body, "prewarm-followup", nil
	case s.prewarmID != "" && previous == "":
		// No parent reference means a self-contained replacement, not a delta.
		body, err := normalizeResponsesWSBody(create.Body, s.lastRequest, delta)
		if err != nil {
			return nil, "", err
		}
		return body, "replace", nil
	case len(s.lastRequest) == 0 && previous != "":
		return nil, "", errResponsesWSPreviousNotFound
	case len(s.lastRequest) == 0:
		body, err := normalizeFirstResponsesWSBody(create.Body)
		if err != nil {
			return nil, "", err
		}
		return body, "full", nil
	case previous == "" && wsLooksLikeFullTranscript(delta):
		body, err := normalizeResponsesWSBody(create.Body, s.lastRequest, delta)
		if err != nil {
			return nil, "", err
		}
		return body, "replace", nil
	default:
		if wsHasCompactionItems(delta) {
			delta = wsStripCompactionItems(delta)
		}
		previousItems, err := wsInputFromBody(s.lastRequest)
		if err != nil {
			return nil, "", err
		}
		previousOutput, _ := wsDecodeItems(s.lastOutput)
		merged, err := wsMergeInput(previousItems, previousOutput, delta)
		if err != nil {
			return nil, "", err
		}
		body, err := normalizeResponsesWSBody(create.Body, s.lastRequest, merged)
		if err != nil {
			return nil, "", err
		}
		if previous == "" {
			return body, "merge", nil
		}
		return body, "incremental", nil
	}
}

// wsBaseBody strips envelope fields and forces streaming; the websocket stream
// is always an SSE-equivalent event stream internally.
func wsBaseBody(body map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(body)+1)
	for key, value := range body {
		switch key {
		case "type", "generate", "event_id", "stream_id", "previous_response_id":
			continue
		}
		out[key] = value
	}
	out["stream"] = json.RawMessage("true")
	return out
}

func normalizeFirstResponsesWSBody(body map[string]json.RawMessage) ([]byte, error) {
	out := wsBaseBody(body)
	rawInput, ok := out["input"]
	if !ok {
		out["input"] = json.RawMessage("[]")
	} else if !wsIsJSONArray(rawInput) {
		return nil, errResponsesWSInputNotArray
	}
	if wsStringField(out, "model") == "" {
		return nil, errResponsesWSModelRequired
	}
	return json.Marshal(out)
}

func normalizeResponsesWSBody(body map[string]json.RawMessage, previous []byte, input []json.RawMessage) ([]byte, error) {
	out := wsBaseBody(body)
	if wsStringField(out, "model") == "" {
		if value, ok := wsRawBodyField(previous, "model"); ok {
			out["model"] = value
		}
	}
	if _, ok := out["instructions"]; !ok {
		if value, ok := wsRawBodyField(previous, "instructions"); ok {
			out["instructions"] = value
		}
	}
	if wsStringField(out, "model") == "" {
		return nil, errResponsesWSModelRequired
	}
	marshaled, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	out["input"] = marshaled
	return json.Marshal(out)
}

func wsIsJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

func wsIsJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

func wsDecodeItems(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func wsBodyFields(body []byte) map[string]json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return nil
	}
	return fields
}

func wsRawBodyField(body []byte, key string) (json.RawMessage, bool) {
	fields := wsBodyFields(body)
	if fields == nil {
		return nil, false
	}
	value, ok := fields[key]
	return value, ok
}

func wsBodyString(body []byte, key string) string {
	return wsStringField(wsBodyFields(body), key)
}

func wsStringField(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func wsInputFromBody(body []byte) ([]json.RawMessage, error) {
	raw, ok := wsRawBodyField(body, "input")
	if !ok {
		return nil, nil
	}
	items, err := wsDecodeItems(raw)
	if err != nil {
		return nil, errResponsesWSInputNotArray
	}
	return items, nil
}

// wsMergeInput rebuilds the full transcript the HTTP upstream needs:
// previous input + previous response output + the client's delta, deduplicated
// the same way CPA deduplicates its websocket merge.
func wsMergeInput(previous []json.RawMessage, previousOutput []json.RawMessage, delta []json.RawMessage) ([]json.RawMessage, error) {
	items := make([]wsInputItem, 0, len(previous)+len(previousOutput)+len(delta))
	for _, raw := range previous {
		items = append(items, wsInputItemMeta(raw))
	}
	for _, raw := range previousOutput {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		items = append(items, wsInputItemMeta(raw))
	}
	for _, raw := range delta {
		items = append(items, wsInputItemMeta(raw))
	}
	items = wsDedupeToolCalls(items)
	items = wsDedupeItems(items)
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		out = append(out, item.raw)
	}
	return out, nil
}

type wsInputItem struct {
	raw      json.RawMessage
	itemType string
	id       string
	callID   string
}

func wsInputItemMeta(raw json.RawMessage) wsInputItem {
	item := wsInputItem{raw: raw}
	var probe struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		CallID string `json:"call_id"`
	}
	if json.Unmarshal(raw, &probe) == nil {
		item.itemType = strings.TrimSpace(probe.Type)
		item.id = strings.TrimSpace(probe.ID)
		item.callID = strings.TrimSpace(probe.CallID)
	}
	return item
}

func wsIsToolCallType(itemType string) bool {
	switch itemType {
	case "function_call", "custom_tool_call":
		return true
	default:
		return false
	}
}

func wsIsToolCallOutputType(itemType string) bool {
	switch itemType {
	case "function_call_output", "custom_tool_call_output":
		return true
	default:
		return false
	}
}

// wsDedupeToolCalls keeps the first item for each call_id; a re-sent tool call
// would otherwise repeat the same invocation in the transcript.
func wsDedupeToolCalls(items []wsInputItem) []wsInputItem {
	seen := make(map[string]struct{}, len(items))
	filtered := make([]wsInputItem, 0, len(items))
	for _, item := range items {
		if wsIsToolCallType(item.itemType) && item.callID != "" {
			if _, ok := seen[item.callID]; ok {
				continue
			}
			seen[item.callID] = struct{}{}
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// wsDedupeItems keeps one item per server-assigned id, preferring the copy a
// later function_call_output refers to.
func wsDedupeItems(items []wsInputItem) []wsInputItem {
	referencedCallIDs := make(map[string]struct{}, len(items))
	for _, item := range items {
		if wsIsToolCallOutputType(item.itemType) && item.callID != "" {
			referencedCallIDs[item.callID] = struct{}{}
		}
	}
	keepIndexByID := make(map[string]int, len(items))
	keepReferencedByID := make(map[string]bool, len(items))
	for index, item := range items {
		if item.id == "" {
			continue
		}
		_, referenced := referencedCallIDs[item.callID]
		referenced = referenced && item.callID != ""
		if _, seen := keepIndexByID[item.id]; !seen {
			keepIndexByID[item.id] = index
			keepReferencedByID[item.id] = referenced
			continue
		}
		if referenced || !keepReferencedByID[item.id] {
			keepIndexByID[item.id] = index
			keepReferencedByID[item.id] = referenced
		}
	}
	filtered := make([]wsInputItem, 0, len(items))
	for index, item := range items {
		if item.id != "" && keepIndexByID[item.id] != index {
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func wsHasCompactionItems(items []json.RawMessage) bool {
	for _, raw := range items {
		switch wsInputItemMeta(raw).itemType {
		case "compaction", "compaction_summary":
			return true
		}
	}
	return false
}

func wsStripCompactionItems(items []json.RawMessage) []json.RawMessage {
	filtered := make([]json.RawMessage, 0, len(items))
	for _, raw := range items {
		switch wsInputItemMeta(raw).itemType {
		case "compaction", "compaction_summary":
			continue
		}
		filtered = append(filtered, raw)
	}
	return filtered
}

// wsLooksLikeFullTranscript reports whether an input array already carries
// conversation history. Such an input is a self-contained replacement, not a
// delta to append to the previous transcript.
func wsLooksLikeFullTranscript(items []json.RawMessage) bool {
	for _, raw := range items {
		item := wsInputItemMeta(raw)
		switch item.itemType {
		case "function_call", "custom_tool_call", "compaction", "compaction_summary":
			return true
		case "message":
			var probe struct {
				Role string `json:"role"`
			}
			if json.Unmarshal(raw, &probe) == nil && strings.TrimSpace(probe.Role) == "assistant" {
				return true
			}
		}
	}
	return false
}
