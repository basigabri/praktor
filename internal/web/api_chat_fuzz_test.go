package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// keepAlive sends raw requests over one reused connection, so fuzzing at
// thousands of requests per second doesn't run out of ephemeral ports.
type keepAlive struct {
	addr string
	conn net.Conn
	br   *bufio.Reader
}

func (k *keepAlive) do(req string) (int, string, error) {
	for attempt := 0; ; attempt++ {
		code, body, err := k.try(req)
		if err == nil || attempt == 1 {
			return code, body, err
		}
		k.close() // a stale connection the server closed; redial once
	}
}

func (k *keepAlive) try(req string) (int, string, error) {
	if k.conn == nil {
		c, err := net.Dial("tcp", k.addr)
		if err != nil {
			return 0, "", err
		}
		k.conn, k.br = c, bufio.NewReader(c)
	}
	_ = k.conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(k.conn, req); err != nil {
		return 0, "", err
	}
	method, _, _ := strings.Cut(req, " ")
	resp, err := http.ReadResponse(k.br, &http.Request{Method: method})
	if err != nil {
		return 0, "", fmt.Errorf("read response: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.Close || err != nil {
		k.close()
	}
	return resp.StatusCode, string(body), err
}

func (k *keepAlive) close() {
	if k.conn != nil {
		_ = k.conn.Close()
		k.conn = nil
	}
}

// panicWatch reports handler panics: net/http recovers them and logs
// "http: panic serving ..." through the standard logger.
type panicWatch struct{ seen atomic.Bool }

func (p *panicWatch) Write(b []byte) (int, error) {
	if strings.Contains(string(b), "panic") {
		p.seen.Store(true)
		_, _ = os.Stderr.Write(b)
	}
	return len(b), nil
}

func watchPanics(f *testing.F) *panicWatch {
	p := &panicWatch{}
	prev := log.Writer()
	log.SetOutput(p)
	f.Cleanup(func() { log.SetOutput(prev) })
	return p
}

// echoChat answers every message at once and only counts calls, so fuzzing
// doesn't accumulate state.
type echoChat struct {
	srv   *Server
	calls atomic.Int64
}

func (e *echoChat) Route(_ context.Context, message string) (string, string, error) {
	if rest, ok := strings.CutPrefix(message, "@swarm "); ok {
		return "swarm", rest, nil
	}
	return "general", message, nil
}

func (e *echoChat) HasAgent(id string) bool { return id == "general" || id == "claude" }

func (e *echoChat) HandleMessage(_ context.Context, agentID, text string, meta map[string]string) error {
	e.calls.Add(1)
	go e.srv.chatWaiters.deliver(agentID, text, meta)
	return nil
}

func chatBodySeeds() []string {
	return []string{
		``, `{}`, `null`, `[]`, `"x"`, `{"message":"hi"}`, `{"message":"@claude hi"}`, `{"message":"@swarm x"}`,
		`{"message":"hi","agent":"claude"}`, `{"message":"hi","agent":"nope"}`, `{"message":"hi","timeout":1}`,
		`{"message":"hi","timeout":600}`, `{"message":"hi","timeout":601}`, `{"message":"hi","timeout":-1}`,
		`{"message":"hi","timeout":1.5}`, `{"message":"hi","timeout":36028797018963998}`,
		`{"message":"hi","timeout":99999999999999999999}`, `{"message":"hi","request_id":"evil","channel":"telegram"}`,
		`{"message":"hi"}garbage`, `{"message":"\u0000"}`, "{\"message\":\"\xff\"}", `{"message":` + strings.Repeat("[", 64),
		`{"MESSAGE":"hi","AGENT":"claude"}`, `{"message":"hi","agent":"general","agent":"claude"}`,
	}
}

// FuzzChatBody posts arbitrary bodies with valid credentials through the real
// server. The handler must never panic or fail with a 5xx other than the
// documented 503 and 504, and must always answer with JSON.
func FuzzChatBody(f *testing.F) {
	for _, s := range chatBodySeeds() {
		f.Add([]byte(s))
	}
	panics := watchPanics(f)
	ls := startServerWith(f, attackPassword, func(s *Server) chatBackend { return &echoChat{srv: s} })
	auth := []string{"Authorization: " + basic("a", attackPassword)}
	k := &keepAlive{addr: ls.addr}
	defer k.close()
	f.Fuzz(func(t *testing.T, body []byte) {
		req := strings.Replace(chatReq("/api/chat", auth, string(body)), "Connection: close\r\n", "", 1)
		code, resp, err := k.do(req)
		if panics.seen.Load() {
			t.Fatalf("handler panicked for body %q", truncate(string(body), 200))
		}
		if err != nil {
			t.Fatalf("connection failed: %v", err)
		}
		switch code {
		case http.StatusOK:
			var r chatResponse
			if err := json.Unmarshal([]byte(resp), &r); err != nil || (r.Agent != "general" && r.Agent != "claude") {
				t.Fatalf("200 with body %q", truncate(resp, 200))
			}
		case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			var e map[string]string
			if err := json.Unmarshal([]byte(resp), &e); err != nil || e["error"] == "" {
				t.Fatalf("status %d with body %q, want a JSON error", code, truncate(resp, 200))
			}
		default:
			t.Fatalf("status %d (%s) for body %q", code, truncate(resp, 200), truncate(string(body), 200))
		}
	})
}

// FuzzChatUnauthenticated sends arbitrary methods, request targets and one
// arbitrary header without the password. Nothing may reach the agent, and
// the server must not fail with a 5xx.
func FuzzChatUnauthenticated(f *testing.F) {
	seeds := []struct{ method, target, header string }{
		{"POST", "/api/chat", ""},
		{"POST", "//api/chat", "Authorization: Basic Og=="},
		{"POST", "/x/../api/chat", "Cookie: session=x"},
		{"POST", "/%61pi/chat", "Authorization: Bearer x"},
		{"GET", "/api/chat", "X-Original-URL: /api/login"},
		{"POST", "http://h/api/chat", "Authorization: Basic YTpi"},
		{"OPTIONS", "/api/chat", "Access-Control-Request-Method: POST"},
		{"POST", "/api/login/../chat", ""},
		{"CONNECT", "/api/chat", ""},
	}
	for _, s := range seeds {
		f.Add(s.method, s.target, s.header)
	}
	panics := watchPanics(f)
	var backend *echoChat
	ls := startServerWith(f, attackPassword, func(s *Server) chatBackend { backend = &echoChat{srv: s}; return backend })
	k := &keepAlive{addr: ls.addr}
	defer k.close()
	f.Fuzz(func(t *testing.T, method, target, header string) {
		if method == "" || target == "" || strings.ContainsAny(method+target, " \r\n\t") || strings.ContainsAny(header, "\r\n") {
			return
		}
		if len(method)+len(target)+len(header) > 8<<10 {
			return // oversized request lines get 431 and a reset connection
		}
		if strings.Contains(header, attackPassword) {
			return // the fuzzer found the password; that's not a bypass
		}
		req := method + " " + target + " HTTP/1.1\r\nHost: praktor\r\n"
		if header != "" {
			req += header + "\r\n"
		}
		body := `{"message":"open the door"}`
		req += "Content-Type: application/json\r\nContent-Length: 27\r\n\r\n" + body
		before := backend.calls.Load()
		code, resp, err := k.do(req)
		if panics.seen.Load() {
			t.Fatalf("handler panicked for %q %q with header %q", method, target, header)
		}
		if backend.calls.Load() != before {
			t.Fatalf("unauthenticated %q %q with header %q reached the agent (status %d)", method, target, header, code)
		}
		if err != nil {
			return // malformed requests get a 400 and a closed (often reset) connection
		}
		// The SPA file server answers 500 for some malformed static paths
		// (invalid UTF-8, e.g. GET /%ff.js): cosmetic, outside the API.
		if code >= 500 && strings.HasPrefix(strings.TrimPrefix(target, "http://h"), "/api/") {
			t.Fatalf("status %d (%s)", code, truncate(resp, 200))
		}
	})
}
