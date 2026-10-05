package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mtzanidakis/praktor/internal/agent"
	"github.com/mtzanidakis/praktor/internal/config"
)

// fakeChat routes like the real router (@name prefix, else "general") and
// answers each message asynchronously through the server's waiters, the way
// the orchestrator's output listener does.
type fakeChat struct {
	srv       *Server
	routeWait time.Duration // simulate slow smart routing
	silent    bool          // never reply (timeout tests)
	failErr   error
	routeErr  error
	routeCtx  context.Context

	mu   sync.Mutex
	ctxs []context.Context
	meta []map[string]string
}

func (f *fakeChat) Route(ctx context.Context, message string) (string, string, error) {
	f.mu.Lock()
	f.routeCtx = ctx
	f.mu.Unlock()
	time.Sleep(f.routeWait)
	if f.routeErr != nil {
		return "", "", f.routeErr
	}
	if rest, ok := strings.CutPrefix(message, "@swarm "); ok {
		return "swarm", rest, nil
	}
	name, rest, _ := strings.Cut(message, " ")
	if strings.HasPrefix(name, "@") && f.HasAgent(name[1:]) {
		return name[1:], rest, nil
	}
	return "general", message, nil
}

func (f *fakeChat) HasAgent(id string) bool { return id == "general" || id == "claude" }

func (f *fakeChat) HandleMessage(ctx context.Context, agentID, text string, meta map[string]string) error {
	if f.failErr != nil {
		return f.failErr
	}
	f.mu.Lock()
	f.ctxs = append(f.ctxs, ctx)
	f.meta = append(f.meta, meta)
	f.mu.Unlock()
	if !f.silent {
		go f.srv.chatWaiters.deliver(agentID, agentID+" says: "+text, meta)
	}
	return nil
}

func newChatServer(t *testing.T) (*Server, *fakeChat) {
	t.Helper()
	srv := &Server{cfg: config.WebConfig{Auth: "secret"}, chatWaiters: newChatWaiters(), chatTimeout: 2 * time.Second}
	f := &fakeChat{srv: srv}
	srv.chatBackend = f
	return srv, f
}

func postChat(t *testing.T, srv *Server, body string) (*httptest.ResponseRecorder, chatResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	srv.chat(rec, req)
	var resp chatResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, resp
}

func TestChat(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantCode  int
		wantAgent string
		wantReply string
	}{
		{"default routing", `{"message":"ping"}`, http.StatusOK, "general", "general says: ping"},
		{"@prefix routing", `{"message":"@claude ping"}`, http.StatusOK, "claude", "claude says: ping"},
		{"explicit agent", `{"message":"ping","agent":"claude"}`, http.StatusOK, "claude", "claude says: ping"},
		{"explicit agent keeps text as is", `{"message":"@general hi","agent":"claude"}`, http.StatusOK, "claude", "claude says: @general hi"},
		{"mention only sends the message as typed", `{"message":"@claude"}`, http.StatusOK, "claude", "claude says: @claude"},
		{"swarm is rejected", `{"message":"@swarm research X"}`, http.StatusBadRequest, "", ""},
		{"unknown agent", `{"message":"ping","agent":"nope"}`, http.StatusNotFound, "", ""},
		{"empty message", `{"message":"  "}`, http.StatusBadRequest, "", ""},
		{"invalid json", `{"message":`, http.StatusBadRequest, "", ""},
		{"custom timeout", `{"message":"ping","timeout":30}`, http.StatusOK, "general", "general says: ping"},
		{"timeout too long", `{"message":"ping","timeout":601}`, http.StatusBadRequest, "", ""},
		{"negative timeout", `{"message":"ping","timeout":-1}`, http.StatusBadRequest, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := newChatServer(t)
			rec, resp := postChat(t, srv, c.body)
			if rec.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.wantCode, rec.Body.String())
			}
			if resp.Agent != c.wantAgent || resp.Reply != c.wantReply {
				t.Errorf("response = %+v, want agent %q reply %q", resp, c.wantAgent, c.wantReply)
			}
		})
	}
}

func TestChatTagsMessagesAsAPI(t *testing.T) {
	srv, f := newChatServer(t)
	if rec, _ := postChat(t, srv, `{"message":"ping"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	meta := f.meta[0]
	if meta[agent.MetaChannel] != agent.ChannelAPI || meta["sender"] != "user:api" || meta[metaRequestID] == "" {
		t.Errorf("meta = %v, want channel=api, sender=user:api and a request_id", meta)
	}
}

func TestChatTimeout(t *testing.T) {
	srv, f := newChatServer(t)
	f.silent = true
	srv.chatTimeout = 50 * time.Millisecond

	rec, _ := postChat(t, srv, `{"message":"ping"}`)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
	if n := len(srv.chatWaiters.pending); n != 0 {
		t.Errorf("%d waiters left after timeout, want 0", n)
	}
}

func TestChatHandleMessageError(t *testing.T) {
	srv, f := newChatServer(t)
	f.failErr = errors.New("agent not registered: general")
	if rec, _ := postChat(t, srv, `{"message":"ping"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if n := len(srv.chatWaiters.pending); n != 0 {
		t.Errorf("%d waiters left after error, want 0", n)
	}
}

func TestChatClientDisconnectDoesNotCancelAgentRun(t *testing.T) {
	srv, f := newChatServer(t)
	f.silent = true

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":"ping"}`)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		srv.chat(httptest.NewRecorder(), req)
		close(done)
	}()

	// Wait until the message has been handed to the backend, then disconnect.
	deadline := time.Now().Add(time.Second)
	for {
		f.mu.Lock()
		n := len(f.ctxs)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("message never reached the backend")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after the client disconnected")
	}
	if err := f.ctxs[0].Err(); err != nil {
		t.Errorf("agent run context = %v after disconnect, want not canceled", err)
	}
}

func TestChatConcurrentRequestsGetTheirOwnReplies(t *testing.T) {
	srv, _ := newChatServer(t)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := range n {
		wg.Go(func() {
			msg := fmt.Sprintf("message %d", i)
			rec, resp := postChat(t, srv, fmt.Sprintf(`{"message":%q}`, msg))
			if rec.Code != http.StatusOK || resp.Reply != "general says: "+msg {
				errs <- fmt.Sprintf("%s: status %d, reply %q", msg, rec.Code, resp.Reply)
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

func TestChatWaitersIgnoreOtherChannels(t *testing.T) {
	cw := newChatWaiters()
	ch := cw.add("req-1")

	// A Telegram reply that happens to carry the same request_id is not ours.
	cw.deliver("general", "telegram reply", map[string]string{metaRequestID: "req-1", "chat_id": "1"})
	select {
	case got := <-ch:
		t.Fatalf("delivered %q from a non-API message", got)
	default:
	}

	cw.deliver("general", "api reply", map[string]string{metaRequestID: "req-1", agent.MetaChannel: agent.ChannelAPI})
	if got := <-ch; got != "api reply" {
		t.Errorf("reply = %q, want %q", got, "api reply")
	}
	// A second reply for the same request is dropped instead of blocking.
	cw.deliver("general", "late reply", map[string]string{metaRequestID: "req-1", agent.MetaChannel: agent.ChannelAPI})
}

func TestChatRequiresAuth(t *testing.T) {
	srv, f := newChatServer(t)
	srv.cfg.Auth = ""
	if rec, _ := postChat(t, srv, `{"message":"ping"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if len(f.meta) != 0 {
		t.Error("message reached the agent without auth configured")
	}
}

func TestChatRunsOnServerContext(t *testing.T) {
	srv, f := newChatServer(t)
	type key struct{}
	srv.baseCtx = context.WithValue(context.Background(), key{}, "server")
	if rec, _ := postChat(t, srv, `{"message":"ping"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if f.ctxs[0].Value(key{}) != "server" {
		t.Error("agent run did not use the server's context")
	}
}

func TestChatUnavailableWithoutBackend(t *testing.T) {
	srv := &Server{chatWaiters: newChatWaiters()}
	if rec, _ := postChat(t, srv, `{"message":"ping"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestChatBodyTooLarge(t *testing.T) {
	srv, _ := newChatServer(t)
	body := `{"message":"` + strings.Repeat("x", maxChatBodyBytes) + `"}`
	if rec, _ := postChat(t, srv, body); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestChatRoutingFailure(t *testing.T) {
	srv, f := newChatServer(t)
	f.routeErr = errors.New("no default agent configured")
	if rec, _ := postChat(t, srv, `{"message":"ping"}`); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestChatRoutesOnServerContextWithDeadline(t *testing.T) {
	srv, f := newChatServer(t)
	type key struct{}
	srv.baseCtx = context.WithValue(context.Background(), key{}, "server")
	if rec, _ := postChat(t, srv, `{"message":"ping"}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if f.routeCtx.Value(key{}) != "server" {
		t.Error("routing did not use the server's context")
	}
	if _, ok := f.routeCtx.Deadline(); !ok {
		t.Error("routing context has no deadline")
	}
}

func TestChatRoutingUsesUpTimeoutDoesNotRunAgent(t *testing.T) {
	srv, f := newChatServer(t)
	srv.chatTimeout = 20 * time.Millisecond
	f.routeWait = 40 * time.Millisecond
	if rec, _ := postChat(t, srv, `{"message":"turn on the lights"}`); rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
	if len(f.meta) != 0 {
		t.Error("message was sent to the agent after the deadline passed")
	}
}
