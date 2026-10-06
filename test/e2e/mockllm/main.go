// Command mockllm is a deterministic stand-in for the Anthropic Messages API,
// for end-to-end tests that run real agent containers (Claude Code) without
// an API key. Point agents at it with ANTHROPIC_BASE_URL.
//
// Replies depend only on the last user message:
//   - a routing query ("You are a message router") answers "beta" when the
//     message contains ROUTE-TO-BETA, else "alpha";
//   - "SLOW <n>" waits n seconds before answering (to have a run in flight);
//   - anything else answers "MOCK-REPLY: <first 40 characters>".
//
// GET /stats reports request counters as JSON, for test assertions.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// st counts requests for /stats.
var st struct {
	Messages, Streaming, InFlight, SlowStarted, Other atomic.Int64
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type request struct {
	Model    string    `json:"model"`
	Stream   bool      `json:"stream"`
	Messages []message `json:"messages"`
}

var slowRE = regexp.MustCompile(`SLOW (\d+)`)

// text returns the text of a message's content, a string or a block list.
func text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		if bl.Type == "text" {
			b.WriteString(bl.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func lastUserText(req request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			if t := text(req.Messages[i].Content); strings.TrimSpace(t) != "" {
				return t
			}
		}
	}
	return ""
}

// reply is the deterministic answer to a user message, and how long to wait
// before giving it.
func reply(user string) (string, time.Duration) {
	if strings.Contains(user, "You are a message router") {
		if strings.Contains(user, "ROUTE-TO-BETA") {
			return "beta", 0
		}
		return "alpha", 0
	}
	var delay time.Duration
	if m := slowRE.FindStringSubmatch(user); m != nil {
		n, _ := strconv.Atoi(m[1])
		delay = time.Duration(n) * time.Second
	}
	snippet := strings.Join(strings.Fields(user), " ")
	if i := strings.Index(snippet, "PING"); i >= 0 {
		snippet = snippet[i:]
	}
	if len(snippet) > 40 {
		snippet = snippet[:40]
	}
	return "MOCK-REPLY: " + snippet, delay
}

func messages(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad json"}}`, http.StatusBadRequest)
		return
	}
	st.Messages.Add(1)
	st.InFlight.Add(1)
	defer st.InFlight.Add(-1)

	user := lastUserText(req)
	answer, delay := reply(user)
	log.Printf("messages model=%s stream=%v delay=%v user=%q -> %q", req.Model, req.Stream, delay, truncate(user, 120), answer)
	if delay > 0 {
		st.SlowStarted.Add(1)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			log.Printf("client went away during a slow reply")
			return
		}
	}

	id := fmt.Sprintf("msg_mock_%d", st.Messages.Load())
	usage := map[string]int{"input_tokens": 10, "output_tokens": 5}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": req.Model,
			"content":     []map[string]string{{"type": "text", "text": answer}},
			"stop_reason": "end_turn", "stop_sequence": nil, "usage": usage,
		})
		return
	}

	st.Streaming.Add(1)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	send := func(event string, data any) {
		b, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	send("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": req.Model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 10, "output_tokens": 1},
	}})
	send("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
		"content_block": map[string]string{"type": "text", "text": ""}})
	send("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
		"delta": map[string]string{"type": "text_delta", "text": answer}})
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	send("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 5}})
	send("message_stop", map[string]any{"type": "message_stop"})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func main() {
	addr := flag.String("addr", ":8000", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"messages": st.Messages.Load(), "streaming": st.Streaming.Load(), "in_flight": st.InFlight.Load(),
			"slow_started": st.SlowStarted.Load(), "other": st.Other.Load(),
		})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/messages"):
			messages(w, r)
		case r.URL.Path == "/api/hello": // Claude Code's connectivity check
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/messages/count_tokens"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":10}`))
		default:
			st.Other.Add(1)
			log.Printf("unhandled %s %s", r.Method, r.URL.String())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"mockllm: not implemented"}}`))
		}
	})
	log.Printf("mockllm listening on %s", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
