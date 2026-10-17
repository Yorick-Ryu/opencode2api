package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

// The legacy read alias must be restored before the Codex namespace mapping.
func TestLegacyReadAliasWithCodexNamespace(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				encoded, _ := json.Marshal(body["tools"])
				if !bytes.Contains(encoded, []byte(`"name":"read"`)) || bytes.Contains(encoded, []byte(`"name":"read_tools__read_file"`)) {
					t.Errorf("legacy tool alias not applied: %s", encoded)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"chat_test\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_read\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			cfg := config.Config{
				Prefer: config.TierZen, ZenKeys: []string{"test-only"},
				Upstream: config.UpstreamConfig{Zen: upstream.URL + "/zen"},
				Retry:    config.RetryConfig{MaxAttempts: 1, TimeoutSeconds: 5},
				Models:   config.ModelsConfig{Protocols: map[string]string{"test-free": "chat"}},
			}
			g, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if err != nil {
				t.Fatal(err)
			}
			g.catalog.Replace([]string{"test-free"}, nil)
			payload := fmt.Sprintf(`{"model":"test-free","stream":%t,"input":"read a file","tools":[{"type":"namespace","name":"read_tools","tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}]}`, stream)
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload))
			r = r.WithContext(telemetry.WithRequestMeta(r.Context(), &telemetry.RequestMeta{}))
			w := httptest.NewRecorder()
			g.handleInference(wire.Responses)(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"name":"read_file"`) || !strings.Contains(w.Body.String(), `"namespace":"read_tools"`) {
				t.Fatalf("client tool identity lost: %s", w.Body.String())
			}
		})
	}
}
