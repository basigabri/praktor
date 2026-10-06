package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mtzanidakis/praktor/internal/config"
)

const testChatToken = "chat-token-0123456789abcdef"

func startTokenServer(t *testing.T, cfg config.WebConfig) *liveServer {
	t.Helper()
	for attempt := 0; ; attempt++ {
		ls, err := tryStartServerCfg(t, cfg, nil)
		if err == nil {
			return ls
		}
		if attempt == 5 {
			t.Fatalf("server did not start: %v", err)
		}
	}
}

func bearer(token string) []string { return []string{"Authorization: Bearer " + token} }

// The chat token grants POST /api/chat and nothing else: it must not open
// Mission Control's API, whatever the path tricks.
func TestChatTokenOnlyGrantsChat(t *testing.T) {
	ls := startTokenServer(t, config.WebConfig{Auth: attackPassword, ChatToken: testChatToken, ChatAgents: []string{"claude"}})

	if code, resp := ls.raw(t, chatReq("/api/chat", bearer(testChatToken), `{"message":"ping"}`)); code != http.StatusOK {
		t.Fatalf("chat with token: status %d %s", code, resp)
	}
	for _, path := range []string{"/api/status", "/api/secrets", "/api/agents/definitions", "/api/tasks", "/api/chat/", "/api/chat/../secrets"} {
		req := "GET " + path + " HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + testChatToken + "\r\nConnection: close\r\n\r\n"
		if code, _ := ls.raw(t, req); code == http.StatusOK {
			t.Errorf("GET %s with the chat token: status 200, want refused", path)
		}
	}
}

func TestChatTokenRejectsWrongCredentials(t *testing.T) {
	ls := startTokenServer(t, config.WebConfig{Auth: attackPassword, ChatToken: testChatToken, ChatAgents: []string{"claude"}})
	cases := map[string][]string{
		"wrong token":             bearer("nope"),
		"empty token":             bearer(""),
		"token prefix":            bearer(testChatToken[:10]),
		"token with extra":        bearer(testChatToken + "x"),
		"lowercase scheme":        {"Authorization: bearer " + testChatToken},
		"token as basic password": {"Authorization: " + basic("a", testChatToken)},
		"no credentials":          nil,
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			if code, resp := ls.raw(t, chatReq("/api/chat", h, `{"message":"ping"}`)); code != http.StatusUnauthorized {
				t.Errorf("status %d %s, want 401", code, resp)
			}
		})
	}
	if ls.f.messages() != 0 {
		t.Error("a message reached the agent with wrong credentials")
	}
}

// With only a chat token configured (no web.auth), chat needs the token.
func TestChatTokenWithoutPassword(t *testing.T) {
	ls := startTokenServer(t, config.WebConfig{ChatToken: testChatToken, ChatAgents: []string{"claude"}})
	if code, _ := ls.raw(t, chatReq("/api/chat", nil, `{"message":"ping"}`)); code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", code)
	}
	if code, resp := ls.raw(t, chatReq("/api/chat", bearer(testChatToken), `{"message":"ping"}`)); code != http.StatusOK {
		t.Errorf("token: status %d %s, want 200", code, resp)
	}
}

func TestChatTokenAgentSelection(t *testing.T) {
	cases := []struct {
		name      string
		agents    []string
		body      string
		wantCode  int
		wantAgent string
		wantReply string
	}{
		{"defaults to the first allowed agent", []string{"claude", "general"}, `{"message":"ping"}`, 200, "claude", "claude says: ping"},
		{"explicit allowed agent", []string{"claude", "general"}, `{"message":"ping","agent":"general"}`, 200, "general", "general says: ping"},
		{"@prefix of an allowed agent", []string{"claude", "general"}, `{"message":"@general ping"}`, 200, "general", "general says: ping"},
		{"bare @prefix sends the message as typed", []string{"claude", "general"}, `{"message":"@general"}`, 200, "general", "general says: @general"},
		{"explicit agent not allowed", []string{"claude"}, `{"message":"ping","agent":"general"}`, 403, "", ""},
		{"@prefix of an agent not allowed", []string{"claude"}, `{"message":"@general ping"}`, 403, "", ""},
		{"unknown agent", nil, `{"message":"ping","agent":"nope"}`, 404, "", ""},
		{"no agent and no allow list", nil, `{"message":"ping"}`, 400, "", ""},
		{"swarm is not an agent", nil, `{"message":"x","agent":"swarm"}`, 404, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, f := newChatServer(t)
			srv.cfg.ChatToken = testChatToken
			srv.cfg.ChatAgents = c.agents
			rec, resp := postChatWith(t, srv, c.body, true)
			if rec.Code != c.wantCode {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, c.wantCode, rec.Body.String())
			}
			if resp.Agent != c.wantAgent || resp.Reply != c.wantReply {
				t.Errorf("response = %+v, want agent %q reply %q", resp, c.wantAgent, c.wantReply)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.routeCtx != nil {
				t.Error("a chat-token request went through smart routing")
			}
		})
	}
}

func TestChatTokenTimeoutStillValidated(t *testing.T) {
	srv, _ := newChatServer(t)
	srv.cfg.ChatToken = testChatToken
	srv.cfg.ChatAgents = []string{"claude"}
	for _, v := range []string{"0.5", "601", "-1", "36028797018963998"} {
		rec, _ := postChatWith(t, srv, `{"message":"ping","timeout":`+v+`}`, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("timeout %s: status %d, want 400", v, rec.Code)
		}
	}
	if !strings.Contains(testChatToken, "chat") { // keep the constant obviously fake
		t.Fatal("unexpected token constant")
	}
}
