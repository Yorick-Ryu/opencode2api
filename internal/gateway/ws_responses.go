package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"opencode2api/internal/identity"
	wire "opencode2api/internal/protocol"
)

// responsesWSWriteTimeout bounds one websocket write. The relay on the other
// side reads continuously, so a stalled write means the connection is gone.
const responsesWSWriteTimeout = 30 * time.Second

// responsesWSTerminalEvents end one response. The names match the relay's
// terminal set so a converted stream settles billing exactly like a native one.
var responsesWSTerminalEvents = map[string]bool{
	"response.completed":  true,
	"response.done":       true,
	"response.incomplete": true,
	"response.failed":     true,
	"response.cancelled":  true,
	"response.canceled":   true,
}

var responsesWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// handleResponsesWebSocket serves /v1/responses over WebSocket. Codex sends one
// response.create message per request and reads the same Responses events the
// HTTP transport would stream as SSE. The opencode.ai upstream only speaks
// HTTP, so each create is executed by the regular inference pipeline and its
// events are forwarded as text frames — the same conversion CPA performs for
// accounts without a native websocket upstream.
func (g *Gateway) handleResponsesWebSocket(w http.ResponseWriter, r *http.Request) {
	if !websocket.IsWebSocketUpgrade(r) {
		wire.WriteError(w, wire.Responses, http.StatusUpgradeRequired, "this endpoint requires a websocket upgrade", "invalid_request_error", "")
		return
	}
	conn, err := responsesWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		return
	}
	conn.SetReadLimit(maxRequestBody)
	ctx, cancel := context.WithCancel(r.Context())
	session := &responsesWSSession{
		gateway: g,
		conn:    conn,
		request: r,
		logger:  g.logger,
		ctx:     ctx,
		cancel:  cancel,
	}
	session.execute = session.executeInference
	g.logger.Info("responses websocket opened", "component", "gateway", "event", "ws_opened", "remote", r.RemoteAddr)
	session.run()
	g.logger.Info("responses websocket closed", "component", "gateway", "event", "ws_closed", "remote", r.RemoteAddr)
}

// responsesWSActive tracks one in-flight response.create.
type responsesWSActive struct {
	body          []byte
	id            string
	cancel        context.CancelFunc
	done          bool
	terminal      string
	committed     bool
	itemsByIndex  map[int64]json.RawMessage
	itemsFallback []json.RawMessage
	finalOutput   []byte
}

func (active *responsesWSActive) collectItem(index *int64, item json.RawMessage) {
	if len(item) == 0 || !wsIsJSONObject(item) {
		return
	}
	copied := append([]byte(nil), item...)
	if index != nil {
		active.itemsByIndex[*index] = copied
		return
	}
	active.itemsFallback = append(active.itemsFallback, copied)
}

// collectedOutput renders the output items seen so far. It is used when the
// terminal event carries no output of its own and for interrupted responses.
func (active *responsesWSActive) collectedOutput() []byte {
	indexes := make([]int64, 0, len(active.itemsByIndex))
	for index := range active.itemsByIndex {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	items := make([]json.RawMessage, 0, len(active.itemsByIndex)+len(active.itemsFallback))
	for _, index := range indexes {
		if item := active.itemsByIndex[index]; wsItemCompleteForOutput(item) {
			items = append(items, item)
		}
	}
	for _, item := range active.itemsFallback {
		if wsItemCompleteForOutput(item) {
			items = append(items, item)
		}
	}
	out, err := json.Marshal(items)
	if err != nil {
		return []byte("[]")
	}
	return out
}

// wsItemCompleteForOutput drops tool calls that never finished streaming their
// arguments; a partial tool call cannot be replayed as conversation history.
func wsItemCompleteForOutput(raw json.RawMessage) bool {
	var probe struct {
		Type      string `json:"type"`
		Arguments string `json:"arguments"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return false
	}
	if wsIsToolCallType(strings.TrimSpace(probe.Type)) {
		return strings.TrimSpace(probe.Arguments) != ""
	}
	return true
}

// responsesWSSession converts one downstream websocket connection. State kept
// per connection lets incremental requests (previous_response_id + delta input)
// be reconstructed into the full transcript the HTTP upstream expects.
type responsesWSSession struct {
	gateway *Gateway
	conn    *websocket.Conn
	request *http.Request
	logger  *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	writeMu sync.Mutex
	mu      sync.Mutex
	active  *responsesWSActive

	lastRequest []byte // last full request body executed on this connection
	lastOutput  []byte // response.output of the last completed response
	lastID      string // response id of the last completed response
	prewarmID   string // synthetic prewarm response awaiting its follow-up

	execute func(ctx context.Context, sink *responsesWSEventSink, body []byte) error
}

func (s *responsesWSSession) run() {
	defer func() {
		s.cancel()
		_ = s.conn.Close()
	}()
	for {
		_, message, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(message, &envelope) != nil {
			s.sendError("", "", http.StatusBadRequest, "websocket message must be a JSON object", "invalid_request_error", "")
			continue
		}
		switch envelope.Type {
		case "response.create":
			s.handleCreate(message)
		case "response.interrupt", "response.cancel":
			s.handleInterrupt(message)
		default:
			s.sendError("", "", http.StatusBadRequest, fmt.Sprintf("unsupported websocket event %q", envelope.Type), "invalid_request_error", "")
		}
	}
}

func (s *responsesWSSession) handleCreate(message []byte) {
	create, err := parseResponsesWSCreate(message)
	if err != nil {
		s.sendError("", "", http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return
	}
	if create.Generate != nil && !*create.Generate {
		s.handlePrewarm(create)
		return
	}
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		s.sendError(create.EventID, create.StreamID, http.StatusConflict, "another response.create is already in progress on this websocket connection", "invalid_request_error", "")
		return
	}
	body, mode, err := s.buildCreateBody(create)
	if err != nil {
		s.mu.Unlock()
		code := ""
		if errors.Is(err, errResponsesWSPreviousNotFound) {
			code = "previous_response_not_found"
		}
		s.sendError(create.EventID, create.StreamID, http.StatusBadRequest, err.Error(), "invalid_request_error", code)
		return
	}
	active := &responsesWSActive{body: body, itemsByIndex: map[int64]json.RawMessage{}}
	ctx, cancel := context.WithCancel(s.ctx)
	active.cancel = cancel
	s.active = active
	s.mu.Unlock()
	s.logger.Info("responses websocket request", "component", "gateway", "event", "ws_create", "mode", mode, "model", wsBodyString(body, "model"), "bytes", len(body))
	go s.runCreate(ctx, active, body)
}

// handlePrewarm answers a generate=false warm-up locally. The upstream has no
// session to prepare, so the ack only exists to give the client a response id
// it can extend with the real request.
func (s *responsesWSSession) handlePrewarm(create *responsesWSCreate) {
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		s.sendError(create.EventID, create.StreamID, http.StatusConflict, "another response.create is already in progress on this websocket connection", "invalid_request_error", "")
		return
	}
	body, err := normalizeFirstResponsesWSBody(create.Body)
	if err != nil {
		s.mu.Unlock()
		s.sendError(create.EventID, create.StreamID, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return
	}
	id := identity.RandomID("resp_prewarm", 12)
	s.lastRequest = body
	s.lastOutput = []byte("[]")
	s.lastID = ""
	s.prewarmID = id
	model := wsBodyString(body, "model")
	for _, event := range buildResponsesWSPrewarmEvents(id, model) {
		if err := s.writePayloadLocked(event); err != nil {
			s.mu.Unlock()
			return
		}
	}
	s.mu.Unlock()
	s.logger.Debug("responses websocket prewarm acknowledged", "component", "gateway", "event", "ws_prewarm", "model", model)
}

func (s *responsesWSSession) handleInterrupt(message []byte) {
	var event struct {
		Type       string `json:"type"`
		EventID    string `json:"event_id"`
		StreamID   string `json:"stream_id"`
		ResponseID string `json:"response_id"`
	}
	_ = json.Unmarshal(message, &event)
	s.mu.Lock()
	active := s.active
	if active == nil || active.done {
		s.mu.Unlock()
		s.logger.Debug("responses websocket control without an active response", "component", "gateway", "event", "ws_control_ignored", "type", event.Type)
		return
	}
	responseID := strings.TrimSpace(event.ResponseID)
	if responseID != "" && active.id != "" && responseID != active.id {
		s.mu.Unlock()
		s.sendError(event.EventID, event.StreamID, http.StatusBadRequest, "response_id does not match the active response", "invalid_request_error", "")
		return
	}
	if responseID == "" {
		responseID = active.id
	}
	active.finalOutput = active.collectedOutput()
	s.markActiveDoneLocked(active, "response.incomplete")
	err := s.writePayloadLocked(buildResponsesWSIncompletePayload(responseID, active.finalOutput))
	cancel := active.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err != nil {
		s.logger.Debug("responses websocket interrupt acknowledgement failed", "component", "gateway", "event", "ws_interrupt_write_failed", "error", err)
		return
	}
	s.logger.Debug("responses websocket response interrupted", "component", "gateway", "event", "ws_interrupt", "response_id", responseID)
}

func (s *responsesWSSession) runCreate(ctx context.Context, active *responsesWSActive, body []byte) {
	sink := &responsesWSEventSink{session: s, active: active}
	runErr := s.execute(ctx, sink, body)
	sink.finish(runErr)
	s.completeActive(active)
}

// completeActive is the request goroutine's cleanup. Terminal and error paths
// normally finish the response earlier; this only clears leftovers.
func (s *responsesWSSession) completeActive(active *responsesWSActive) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == active {
		s.active = nil
	}
	switch active.terminal {
	case "response.completed", "response.done", "response.incomplete":
		s.commitActiveLocked(active)
	}
}

// markActiveDoneLocked finishes one active response. A committed terminal also
// advances the connection state immediately: the relay may send the next
// create as soon as it sees that frame, before the request goroutine returns.
// Caller holds s.mu.
func (s *responsesWSSession) markActiveDoneLocked(active *responsesWSActive, terminal string) {
	active.done = true
	active.terminal = terminal
	if s.active == active {
		s.active = nil
	}
	switch terminal {
	case "response.completed", "response.done", "response.incomplete":
		s.commitActiveLocked(active)
	}
}

// commitActiveLocked advances the transcript baseline used to rebuild
// incremental requests. Caller holds s.mu.
func (s *responsesWSSession) commitActiveLocked(active *responsesWSActive) {
	if active.committed {
		return
	}
	active.committed = true
	s.lastRequest = active.body
	if len(active.finalOutput) > 0 {
		s.lastOutput = active.finalOutput
	} else {
		s.lastOutput = []byte("[]")
	}
	if active.id != "" {
		s.lastID = active.id
	}
	s.prewarmID = ""
}

// executeInference runs one response.create through the regular HTTP pipeline
// with a sink that converts the SSE stream into websocket text frames.
func (s *responsesWSSession) executeInference(ctx context.Context, sink *responsesWSEventSink, body []byte) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("responses websocket request panic: %v", recovered)
		}
	}()
	req := s.request.Clone(ctx)
	req.Method = http.MethodPost
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header = s.request.Header.Clone()
	for _, name := range []string{"Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol"} {
		req.Header.Del(name)
	}
	req.Header.Set("Content-Type", "application/json")
	s.gateway.handleInference(wire.Responses)(sink, req)
	return nil
}

// forwardEvent relays one Responses event to the downstream socket.
func (s *responsesWSSession) forwardEvent(active *responsesWSActive, payload []byte, event *responsesWSEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if active.done {
		return nil
	}
	switch event.Type {
	case "response.created":
		if event.Response.ID != "" {
			active.id = event.Response.ID
		}
	case "response.output_item.done":
		active.collectItem(event.OutputIndex, event.Item)
	case "error":
		active.finalOutput = active.collectedOutput()
		s.markActiveDoneLocked(active, "error")
	}
	if responsesWSTerminalEvents[event.Type] {
		if event.Response.ID != "" {
			active.id = event.Response.ID
		}
		if output := event.Response.Output; wsIsJSONArray(output) && len(bytes.TrimSpace(output)) > 2 {
			active.finalOutput = append([]byte(nil), output...)
		} else {
			active.finalOutput = active.collectedOutput()
		}
		s.markActiveDoneLocked(active, event.Type)
	}
	return s.writePayloadLocked(payload)
}

// sendError writes one error envelope. The relay correlates it with the stream
// it terminates; clients map status and code to their own retry behavior.
func (s *responsesWSSession) sendError(eventID, streamID string, status int, message, kind, code string) {
	payload := buildResponsesWSErrorPayload(status, eventID, streamID, message, kind, code)
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.writePayloadLocked(payload)
}

func (s *responsesWSSession) finishWithError(active *responsesWSActive, status int, body []byte, runErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if active.done {
		return
	}
	active.finalOutput = active.collectedOutput()
	s.markActiveDoneLocked(active, "error")
	if s.ctx.Err() != nil {
		return
	}
	if status < 400 {
		status = http.StatusBadGateway
	}
	message, kind := "", "upstream_error"
	var parsed struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		if strings.TrimSpace(parsed.Error.Message) != "" {
			message = parsed.Error.Message
		}
		if strings.TrimSpace(parsed.Error.Type) != "" {
			kind = parsed.Error.Type
		}
	}
	if message == "" {
		if runErr != nil {
			message = runErr.Error()
		} else {
			message = http.StatusText(status)
		}
	}
	_ = s.writePayloadLocked(buildResponsesWSErrorPayload(status, "", "", message, kind, ""))
}

func (s *responsesWSSession) finishWithoutTerminal(active *responsesWSActive, runErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if active.done {
		return
	}
	active.finalOutput = active.collectedOutput()
	s.markActiveDoneLocked(active, "error")
	if s.ctx.Err() != nil {
		return
	}
	message := "upstream stream ended without a terminal event"
	if runErr != nil {
		message = runErr.Error()
	}
	_ = s.writePayloadLocked(buildResponsesWSErrorPayload(http.StatusBadGateway, "", "", message, "upstream_error", ""))
}

// writePayloadLocked writes one text frame. Callers serialize through s.mu so
// frames never interleave with an interrupt acknowledgement.
func (s *responsesWSSession) writePayloadLocked(payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(time.Now().Add(responsesWSWriteTimeout)); err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.TextMessage, payload)
}

// responsesWSEvent is the subset of a Responses event the session inspects.
type responsesWSEvent struct {
	Type        string          `json:"type"`
	OutputIndex *int64          `json:"output_index"`
	Item        json.RawMessage `json:"item"`
	Response    struct {
		ID     string          `json:"id"`
		Status string          `json:"status"`
		Output json.RawMessage `json:"output"`
	} `json:"response"`
}

// responsesWSEventSink adapts the inference pipeline's SSE output to websocket
// text frames. Streamed data lines become frames; non-2xx JSON bodies become a
// single error envelope once the handler returns.
type responsesWSEventSink struct {
	session *responsesWSSession
	active  *responsesWSActive

	header    http.Header
	status    int
	mode      int
	buf       bytes.Buffer
	dataLines []string
	errBody   bytes.Buffer
	writeErr  error
}

const (
	responsesWSSinkStream = iota
	responsesWSSinkError
)

func (sink *responsesWSEventSink) Header() http.Header {
	if sink.header == nil {
		sink.header = http.Header{}
	}
	return sink.header
}

func (sink *responsesWSEventSink) WriteHeader(status int) {
	if sink.status != 0 {
		return
	}
	sink.status = status
	if status >= 400 {
		sink.mode = responsesWSSinkError
	}
}

func (sink *responsesWSEventSink) Write(data []byte) (int, error) {
	if sink.mode == responsesWSSinkError {
		sink.errBody.Write(data)
		return len(data), nil
	}
	sink.buf.Write(data)
	sink.drain()
	if sink.writeErr != nil {
		return 0, sink.writeErr
	}
	return len(data), nil
}

func (sink *responsesWSEventSink) Flush() {}

func (sink *responsesWSEventSink) drain() {
	for {
		line, ok := sink.nextLine()
		if !ok {
			return
		}
		if line == "" {
			if len(sink.dataLines) == 0 {
				continue
			}
			payload := strings.Join(sink.dataLines, "\n")
			sink.dataLines = sink.dataLines[:0]
			if err := sink.forward(payload); err != nil {
				sink.writeErr = err
				return
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			sink.dataLines = append(sink.dataLines, data)
		}
	}
}

func (sink *responsesWSEventSink) nextLine() (string, bool) {
	data := sink.buf.Bytes()
	index := bytes.IndexByte(data, '\n')
	if index < 0 {
		return "", false
	}
	line := string(data[:index])
	sink.buf.Next(index + 1)
	return strings.TrimSuffix(line, "\r"), true
}

func (sink *responsesWSEventSink) forward(payload string) error {
	payload = strings.TrimSpace(payload)
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	var event responsesWSEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil
	}
	if event.Type == "" {
		return nil
	}
	return sink.session.forwardEvent(sink.active, []byte(payload), &event)
}

func (sink *responsesWSEventSink) finish(runErr error) {
	sink.drain()
	if sink.mode == responsesWSSinkError {
		sink.session.finishWithError(sink.active, sink.status, sink.errBody.Bytes(), runErr)
		return
	}
	sink.session.finishWithoutTerminal(sink.active, runErr)
}

// buildResponsesWSErrorPayload renders the error envelope the relay and Codex
// both parse: {"type":"error","status":N,"error":{message,type,param,code}}.
func buildResponsesWSErrorPayload(status int, eventID, streamID, message, kind, code string) []byte {
	if status == 0 {
		status = http.StatusInternalServerError
	}
	errorObject := map[string]any{"message": message, "type": kind, "param": nil, "code": nil}
	if code != "" {
		errorObject["code"] = code
	}
	payload := map[string]any{"type": "error", "status": status, "error": errorObject}
	if eventID != "" {
		payload["event_id"] = eventID
	}
	if streamID != "" {
		payload["stream_id"] = streamID
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"type":"error","status":500,"error":{"message":"internal error","type":"upstream_error"}}`)
	}
	return out
}

// buildResponsesWSIncompletePayload acknowledges an interrupt the way Codex
// expects to observe one: a terminal response.incomplete with reason
// "interrupted" carrying the partial output seen so far.
func buildResponsesWSIncompletePayload(responseID string, output []byte) []byte {
	if len(output) == 0 {
		output = []byte("[]")
	}
	payload := map[string]any{
		"type": "response.incomplete",
		"response": map[string]any{
			"id":                 responseID,
			"status":             "incomplete",
			"incomplete_details": map[string]any{"reason": "interrupted"},
			"output":             json.RawMessage(output),
		},
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return []byte(`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"interrupted"},"output":[]}}`)
	}
	return out
}

// buildResponsesWSPrewarmEvents acknowledges a generate=false warm-up. The
// synthetic response never reaches the upstream; it exists so the client can
// reuse the connection with previous_response_id.
func buildResponsesWSPrewarmEvents(id, model string) [][]byte {
	createdAt := time.Now().Unix()
	response := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": createdAt,
		"status":     "in_progress",
		"background": false,
		"error":      nil,
		"output":     []any{},
	}
	if model != "" {
		response["model"] = model
	}
	created, err := json.Marshal(map[string]any{
		"type":            "response.created",
		"sequence_number": 0,
		"response":        response,
	})
	if err != nil {
		return nil
	}
	completedResponse := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": createdAt,
		"status":     "completed",
		"background": false,
		"error":      nil,
		"output":     []any{},
		"usage": map[string]any{
			"input_tokens":          0,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens":         0,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens":          0,
		},
	}
	if model != "" {
		completedResponse["model"] = model
	}
	completed, err := json.Marshal(map[string]any{
		"type":            "response.completed",
		"sequence_number": 1,
		"response":        completedResponse,
	})
	if err != nil {
		return nil
	}
	return [][]byte{created, completed}
}
