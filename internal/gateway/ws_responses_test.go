package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"opencode2api/internal/config"
)

func rawItems(items ...string) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		out = append(out, json.RawMessage(item))
	}
	return out
}

func TestWSMergeInputRebuildsTranscript(t *testing.T) {
	previous := rawItems(`{"type":"message","role":"user","id":"msg_1","content":[{"type":"input_text","text":"hi"}]}`)
	output := rawItems(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":"{}"}`)
	delta := rawItems(`{"type":"function_call_output","call_id":"call_1","output":"ok"}`)

	merged, err := wsMergeInput(previous, output, delta)
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if len(merged) != 3 {
		t.Fatalf("merged items = %d, want 3", len(merged))
	}
	types := make([]string, 0, len(merged))
	for _, item := range merged {
		types = append(types, wsInputItemMeta(item).itemType)
	}
	want := []string{"message", "function_call", "function_call_output"}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("merged types = %v, want %v", types, want)
		}
	}
}

func TestWSMergeInputDedupesResentItems(t *testing.T) {
	previous := rawItems(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":""}`)
	// The delta re-sends the same call (same call_id, new id-less copy) plus
	// its output; the merge must keep exactly one invocation.
	delta := rawItems(
		`{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"cmd\":\"ls\"}"}`,
		`{"type":"function_call_output","call_id":"call_1","output":"ok"}`,
	)
	merged, err := wsMergeInput(previous, nil, delta)
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	calls := 0
	for _, item := range merged {
		if wsInputItemMeta(item).itemType == "function_call" {
			calls++
		}
	}
	if calls != 1 {
		t.Fatalf("function_call count = %d, want 1", calls)
	}
}

func TestWSMergeInputPrefersReferencedDuplicate(t *testing.T) {
	// The same item id appears twice; the later copy is referenced by a
	// function_call_output and must win.
	previous := rawItems(
		`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"early"}]}`,
		`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"late"}]}`,
		`{"type":"function_call_output","call_id":"call_9","output":"ok"}`,
	)
	merged, err := wsMergeInput(previous, nil, nil)
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if len(merged) != 2 {
		t.Fatalf("merged items = %d, want 2", len(merged))
	}
	if !strings.Contains(string(merged[0]), "late") {
		t.Fatalf("kept copy = %s, want the later duplicate", merged[0])
	}
}

func TestWSBuildCreateBodyModes(t *testing.T) {
	model := json.RawMessage(`"deepseek-test-free"`)
	userInput := json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`)

	t.Run("first request normalizes", func(t *testing.T) {
		session := &responsesWSSession{}
		create := &responsesWSCreate{Body: map[string]json.RawMessage{
			"type":  json.RawMessage(`"response.create"`),
			"model": model,
			"input": userInput,
		}}
		body, mode, err := session.buildCreateBody(create)
		if err != nil {
			t.Fatalf("build failed: %v", err)
		}
		if mode != "full" {
			t.Fatalf("mode = %q, want full", mode)
		}
		var parsed map[string]json.RawMessage
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("body is not JSON: %v", err)
		}
		if string(parsed["stream"]) != "true" {
			t.Fatalf("stream = %s, want true", parsed["stream"])
		}
		if _, ok := parsed["type"]; ok {
			t.Fatalf("type envelope field leaked into body")
		}
	})

	t.Run("model required", func(t *testing.T) {
		session := &responsesWSSession{}
		create := &responsesWSCreate{Body: map[string]json.RawMessage{"input": userInput}}
		if _, _, err := session.buildCreateBody(create); err == nil {
			t.Fatalf("expected model error")
		}
	})

	t.Run("input must be an array", func(t *testing.T) {
		session := &responsesWSSession{}
		create := &responsesWSCreate{Body: map[string]json.RawMessage{"model": model, "input": json.RawMessage(`"hi"`)}}
		if _, _, err := session.buildCreateBody(create); err == nil {
			t.Fatalf("expected input error")
		}
	})

	t.Run("unknown previous response", func(t *testing.T) {
		session := &responsesWSSession{}
		create := &responsesWSCreate{Previous: "resp_ghost", Body: map[string]json.RawMessage{"model": model, "input": userInput}}
		if _, _, err := session.buildCreateBody(create); err != errResponsesWSPreviousNotFound {
			t.Fatalf("err = %v, want previous_response_id not found", err)
		}
	})

	t.Run("incremental merges state", func(t *testing.T) {
		session := &responsesWSSession{}
		first := &responsesWSCreate{Body: map[string]json.RawMessage{"model": model, "input": userInput}}
		body, _, err := session.buildCreateBody(first)
		if err != nil {
			t.Fatalf("first build failed: %v", err)
		}
		session.lastRequest = body
		session.lastOutput = []byte(`[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"shell","arguments":"{}"}]`)
		session.lastID = "resp_1"

		next := &responsesWSCreate{Previous: "resp_1", Body: map[string]json.RawMessage{
			"input": json.RawMessage(`[{"type":"function_call_output","call_id":"call_1","output":"done"}]`),
		}}
		merged, mode, err := session.buildCreateBody(next)
		if err != nil {
			t.Fatalf("incremental build failed: %v", err)
		}
		if mode != "incremental" {
			t.Fatalf("mode = %q, want incremental", mode)
		}
		var parsed map[string]json.RawMessage
		if err := json.Unmarshal(merged, &parsed); err != nil {
			t.Fatalf("merged body is not JSON: %v", err)
		}
		var items []json.RawMessage
		if err := json.Unmarshal(parsed["input"], &items); err != nil {
			t.Fatalf("merged input is not JSON: %v", err)
		}
		if len(items) != 3 {
			t.Fatalf("merged input items = %d, want 3", len(items))
		}
		if string(parsed["model"]) != `"deepseek-test-free"` {
			t.Fatalf("model was not inherited: %s", parsed["model"])
		}
		if _, ok := parsed["previous_response_id"]; ok {
			t.Fatalf("previous_response_id leaked into the HTTP body")
		}
	})

	t.Run("prewarm follow-up merges warmup input", func(t *testing.T) {
		session := &responsesWSSession{}
		warmup := &responsesWSCreate{Body: map[string]json.RawMessage{"model": model, "input": userInput}}
		body, _, err := session.buildCreateBody(warmup)
		if err != nil {
			t.Fatalf("warmup build failed: %v", err)
		}
		session.lastRequest = body
		session.prewarmID = "resp_prewarm_x"

		followUp := &responsesWSCreate{Previous: "resp_prewarm_x", Body: map[string]json.RawMessage{
			"input": json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]`),
		}}
		merged, mode, err := session.buildCreateBody(followUp)
		if err != nil {
			t.Fatalf("follow-up build failed: %v", err)
		}
		if mode != "prewarm-followup" {
			t.Fatalf("mode = %q, want prewarm-followup", mode)
		}
		var parsed map[string]json.RawMessage
		_ = json.Unmarshal(merged, &parsed)
		var items []json.RawMessage
		_ = json.Unmarshal(parsed["input"], &items)
		if len(items) != 2 {
			t.Fatalf("merged input items = %d, want 2", len(items))
		}

		mismatch := &responsesWSCreate{Previous: "resp_other", Body: map[string]json.RawMessage{"input": userInput}}
		if _, _, err := session.buildCreateBody(mismatch); err != errResponsesWSPreviousNotFound {
			t.Fatalf("err = %v, want previous_response_id not found", err)
		}
	})

	t.Run("full transcript replaces state", func(t *testing.T) {
		session := &responsesWSSession{}
		first := &responsesWSCreate{Body: map[string]json.RawMessage{"model": model, "input": userInput}}
		body, _, err := session.buildCreateBody(first)
		if err != nil {
			t.Fatalf("first build failed: %v", err)
		}
		session.lastRequest = body
		session.lastID = "resp_1"

		replacement := &responsesWSCreate{Body: map[string]json.RawMessage{
			"model": model,
			"input": json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}]`),
		}}
		merged, mode, err := session.buildCreateBody(replacement)
		if err != nil {
			t.Fatalf("replacement build failed: %v", err)
		}
		if mode != "replace" {
			t.Fatalf("mode = %q, want replace", mode)
		}
		var parsed map[string]json.RawMessage
		_ = json.Unmarshal(merged, &parsed)
		var items []json.RawMessage
		_ = json.Unmarshal(parsed["input"], &items)
		if len(items) != 3 {
			t.Fatalf("replacement input items = %d, want 3 (untouched)", len(items))
		}
	})
}

// startWSTestServer serves one responses websocket session per connection with
// a caller-provided execution function.
func startWSTestServer(t *testing.T, execute func(ctx context.Context, sink *responsesWSEventSink, body []byte) error) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(maxRequestBody)
		ctx, cancel := context.WithCancel(r.Context())
		session := &responsesWSSession{conn: conn, request: r, logger: slog.Default(), ctx: ctx, cancel: cancel}
		session.execute = execute
		session.run()
	}))
	t.Cleanup(server.Close)
	return server
}

func dialWSTestClient(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func readWSFrames(t *testing.T, client *websocket.Conn, count int) []map[string]any {
	t.Helper()
	frames := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, payload, err := client.ReadMessage()
		if err != nil {
			t.Fatalf("read frame %d failed: %v", i, err)
		}
		var frame map[string]any
		if err := json.Unmarshal(payload, &frame); err != nil {
			t.Fatalf("frame %d is not JSON: %v", i, err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func writeWSFrame(t *testing.T, client *websocket.Conn, payload string) {
	t.Helper()
	if err := client.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatalf("write failed: %v", err)
	}
}

func TestResponsesWebSocketServesCreateAndPrewarm(t *testing.T) {
	server := startWSTestServer(t, func(ctx context.Context, sink *responsesWSEventSink, body []byte) error {
		events := []string{
			`{"type":"response.created","response":{"id":"resp_live_1","status":"in_progress"}}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_live_1","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
			`{"type":"response.completed","response":{"id":"resp_live_1","status":"completed","output":[]}}`,
		}
		for index, event := range events {
			frame := "data: " + event + "\n\n"
			if index == 1 {
				// Split one frame across writes to exercise buffering.
				cut := len(frame) / 2
				if _, err := sink.Write([]byte(frame[:cut])); err != nil {
					return err
				}
				if _, err := sink.Write([]byte(frame[cut:])); err != nil {
					return err
				}
				continue
			}
			if _, err := sink.Write([]byte(frame)); err != nil {
				return err
			}
		}
		return nil
	})
	client := dialWSTestClient(t, server)

	writeWSFrame(t, client, `{"type":"response.create","model":"ws-test-free","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	frames := readWSFrames(t, client, 3)
	if frames[0]["type"] != "response.created" || frames[2]["type"] != "response.completed" {
		t.Fatalf("unexpected frame order: %v", []any{frames[0]["type"], frames[1]["type"], frames[2]["type"]})
	}
	response, _ := frames[2]["response"].(map[string]any)
	if response["id"] != "resp_live_1" {
		t.Fatalf("terminal response id = %v", response["id"])
	}

	// A prewarm is answered locally with a synthetic created+completed pair.
	writeWSFrame(t, client, `{"type":"response.create","generate":false,"model":"ws-test-free","input":[]}`)
	prewarm := readWSFrames(t, client, 2)
	if prewarm[0]["type"] != "response.created" || prewarm[1]["type"] != "response.completed" {
		t.Fatalf("unexpected prewarm frames: %v", []any{prewarm[0]["type"], prewarm[1]["type"]})
	}
	prewarmResponse, _ := prewarm[1]["response"].(map[string]any)
	id, _ := prewarmResponse["id"].(string)
	if !strings.HasPrefix(id, "resp_prewarm_") {
		t.Fatalf("prewarm id = %q", id)
	}
}

func TestResponsesWebSocketInterruptsActiveResponse(t *testing.T) {
	started := make(chan string, 1)
	server := startWSTestServer(t, func(ctx context.Context, sink *responsesWSEventSink, body []byte) error {
		if _, err := sink.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_int_1","status":"in_progress"}}` + "\n\n")); err != nil {
			return err
		}
		if _, err := sink.Write([]byte(`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n")); err != nil {
			return err
		}
		started <- "resp_int_1"
		<-ctx.Done()
		return nil
	})
	client := dialWSTestClient(t, server)

	writeWSFrame(t, client, `{"type":"response.create","model":"ws-test-free","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	readWSFrames(t, client, 2)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatalf("execution did not start")
	}

	writeWSFrame(t, client, `{"type":"response.interrupt","response_id":"resp_int_1","mode":"discard_partial_items"}`)
	frames := readWSFrames(t, client, 1)
	if frames[0]["type"] != "response.incomplete" {
		t.Fatalf("interrupt frame = %v, want response.incomplete", frames[0]["type"])
	}
	response, _ := frames[0]["response"].(map[string]any)
	if response["id"] != "resp_int_1" {
		t.Fatalf("incomplete response id = %v", response["id"])
	}
	details, _ := response["incomplete_details"].(map[string]any)
	if details["reason"] != "interrupted" {
		t.Fatalf("incomplete reason = %v", details["reason"])
	}
}

// TestResponsesWebSocketRunsInferencePipeline exercises the real conversion:
// websocket create -> HTTP upstream -> SSE events -> websocket frames, plus an
// incremental follow-up that must arrive upstream as a merged transcript.
func TestResponsesWebSocketRunsInferencePipeline(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		mu.Lock()
		bodies = append(bodies, parsed)
		index := len(bodies)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		id := fmt.Sprintf("resp_up_%d", index)
		events := []string{
			fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress"}}`, id),
			fmt.Sprintf(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_up_%d","role":"assistant","content":[{"type":"output_text","text":"reply"}]}}`, index),
			fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":[]}}`, id),
		}
		for _, event := range events {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	cfg := config.Config{
		Prefer:    config.TierZen,
		Listen:    "127.0.0.1:0",
		Anonymous: true,
		Upstream:  config.UpstreamConfig{Zen: upstream.URL, Go: upstream.URL, Anonymous: upstream.URL},
		Retry:     config.RetryConfig{MaxAttempts: 1, TimeoutSeconds: 30},
		Models:    config.ModelsConfig{Protocols: map[string]string{"ws-test-free": "responses"}},
	}
	gateway, err := New(cfg, slog.Default(), nil)
	if err != nil {
		t.Fatalf("gateway construction failed: %v", err)
	}
	gateway.catalog.Replace([]string{"ws-test-free"}, nil)

	server := httptest.NewServer(http.HandlerFunc(gateway.handleResponsesWebSocket))
	defer server.Close()
	client := dialWSTestClient(t, server)

	writeWSFrame(t, client, `{"type":"response.create","model":"ws-test-free","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	frames := readWSFrames(t, client, 3)
	if frames[2]["type"] != "response.completed" {
		t.Fatalf("terminal frame = %v", frames[2]["type"])
	}

	writeWSFrame(t, client, `{"type":"response.create","model":"ws-test-free","previous_response_id":"resp_up_1","input":[{"type":"function_call_output","call_id":"call_1","output":"done"}]}`)
	frames = readWSFrames(t, client, 3)
	if frames[2]["type"] != "response.completed" {
		t.Fatalf("terminal frame = %v", frames[2]["type"])
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(bodies))
	}
	if bodies[0]["model"] != "ws-test-free" {
		t.Fatalf("upstream model = %v", bodies[0]["model"])
	}
	secondInput, _ := bodies[1]["input"].([]any)
	if len(secondInput) != 3 {
		t.Fatalf("second request input items = %d, want 3 (user + output + delta)", len(secondInput))
	}
}
