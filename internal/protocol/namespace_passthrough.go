package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"

	"opencode2api/internal/jsonutil"
)

// responsesPassthroughRewriter rewrites only the SSE events that name a
// namespace child of a declared namespace. Every other byte of the upstream
// stream is forwarded unchanged, so provider-specific fields survive a
// passthrough that only needs the tool identity translated.
type responsesPassthroughRewriter struct {
	namespaces ToolAliases
	needles    [][]byte
}

func newResponsesPassthroughRewriter(namespaces ToolAliases) *responsesPassthroughRewriter {
	if len(namespaces) == 0 {
		return nil
	}
	rewriter := &responsesPassthroughRewriter{namespaces: namespaces}
	for dotted := range namespaces {
		rewriter.needles = append(rewriter.needles, []byte(`"`+dotted+`"`))
	}
	return rewriter
}

// copyStream forwards the upstream stream, rewriting the events that carry a
// namespaced tool call.
func (rewriter *responsesPassthroughRewriter) copyStream(dst io.Writer, src io.Reader) error {
	reader := bufio.NewReaderSize(src, 64<<10)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, writeErr := dst.Write(rewriter.rewriteLine(line)); writeErr != nil {
				return writeErr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func (rewriter *responsesPassthroughRewriter) rewriteLine(line []byte) []byte {
	body := bytes.TrimLeft(line, " \t")
	if !bytes.HasPrefix(body, []byte("data:")) {
		return line
	}
	payload := bytes.TrimSpace(body[len("data:"):])
	if len(payload) == 0 || payload[0] != '{' {
		return line
	}
	matched := false
	for _, needle := range rewriter.needles {
		if bytes.Contains(payload, needle) {
			matched = true
			break
		}
	}
	if !matched {
		return line
	}
	var event map[string]any
	if err := json.Unmarshal(payload, &event); err != nil {
		return line
	}
	changed := false
	if item, ok := event["item"].(map[string]any); ok && restoreResponsesNamespace(rewriter.namespaces, item) {
		changed = true
	}
	// response.function_call_arguments.done repeats the name at the top level.
	if restoreResponsesNamespace(rewriter.namespaces, event) {
		changed = true
	}
	if response, ok := event["response"].(map[string]any); ok {
		for _, raw := range jsonutil.SliceAt(response, "output") {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if restoreResponsesNamespace(rewriter.namespaces, item) {
				changed = true
			}
		}
	}
	if !changed {
		return line
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return line
	}
	out := make([]byte, 0, len(encoded)+8)
	out = append(out, "data: "...)
	out = append(out, encoded...)
	switch {
	case bytes.HasSuffix(line, []byte("\r\n")):
		out = append(out, '\r', '\n')
	case bytes.HasSuffix(line, []byte("\n")):
		out = append(out, '\n')
	}
	return out
}

// RewriteResponsesDocument applies the namespace translation to a single
// Responses document, the non-streaming passthrough case.
func RewriteResponsesDocument(body []byte, namespaces ToolAliases) []byte {
	if len(namespaces) == 0 {
		return body
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		return body
	}
	changed := false
	for _, raw := range jsonutil.SliceAt(document, "output") {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if restoreResponsesNamespace(namespaces, item) {
			changed = true
		}
	}
	if !changed {
		return body
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return body
	}
	return encoded
}
