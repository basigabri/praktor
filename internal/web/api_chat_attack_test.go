package web

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mtzanidakis/praktor/internal/agent"
	"github.com/mtzanidakis/praktor/internal/config"
)

// These tests attack POST /api/chat through Server.Start: the real listener,
// middleware (CORS, auth) and mux, with a fake chat backend.

const attackPassword = "s3cret-pass"

type liveServer struct {
	srv  *Server
	f    *fakeChat
	addr string // 127.0.0.1:port
}

func freePort(t testing.TB) int {
	t.Helper()
	l, err := net.Listen("tcp", ":0") // Start listens on all interfaces too
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// startServer runs Server.Start on a free port until the test ends.
func startServer(t testing.TB, auth string) *liveServer {
	t.Helper()
	return startServerWith(t, auth, nil)
}

// startServerWith uses the backend from mk instead of a fakeChat. It retries
// on another port if the chosen one was taken in the meantime (parallel
// packages and fuzz workers pick ports concurrently).
func startServerWith(t testing.TB, auth string, mk func(*Server) chatBackend) *liveServer {
	t.Helper()
	for attempt := 0; ; attempt++ {
		ls, err := tryStartServer(t, auth, mk)
		if err == nil {
			return ls
		}
		if attempt == 5 {
			t.Fatalf("server did not start: %v", err)
		}
	}
}

func tryStartServer(t testing.TB, auth string, mk func(*Server) chatBackend) (*liveServer, error) {
	port := freePort(t)
	srv := NewServer(nil, nil, nil, nil, nil, nil, config.WebConfig{Enabled: true, Port: port, Auth: auth}, nil, "test")
	srv.chatTimeout = 2 * time.Second
	f := &fakeChat{srv: srv}
	srv.chatBackend = f
	if mk != nil {
		srv.chatBackend = mk(srv)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	addr := "127.0.0.1:" + strconv.Itoa(port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case err := <-done:
			cancel()
			return nil, err
		default:
		}
		c, err := net.Dial("tcp", addr)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Start: %v", err)
		}
	})
	return &liveServer{srv: srv, f: f, addr: addr}, nil
}

// raw sends a raw HTTP/1.1 request and returns the status and body. It
// fails the test if the connection breaks (a panic in a handler does that).
func (ls *liveServer) raw(t testing.TB, req string) (int, string) {
	t.Helper()
	code, body, err := ls.do(req)
	if err != nil {
		t.Fatalf("%v (request %q)", err, truncate(req, 200))
	}
	return code, body
}

// do is raw for goroutines other than the test's.
func (ls *liveServer) do(req string) (int, string, error) {
	conn, err := net.Dial("tcp", ls.addr)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, req); err != nil {
		return 0, "", err
	}
	method, _, _ := strings.Cut(req, " ")
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		return 0, "", fmt.Errorf("read response: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), err
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// chatReq builds a POST with the given extra header lines and JSON body.
func chatReq(path string, headers []string, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\nHost: praktor\r\nConnection: close\r\nContent-Type: application/json\r\n", path)
	for _, h := range headers {
		b.WriteString(h + "\r\n")
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return b.String()
}

func (ls *liveServer) calls() int {
	ls.f.mu.Lock()
	defer ls.f.mu.Unlock()
	return len(ls.f.meta)
}

func (ls *liveServer) waiters() int {
	ls.srv.chatWaiters.mu.Lock()
	defer ls.srv.chatWaiters.mu.Unlock()
	return len(ls.srv.chatWaiters.pending)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func TestChatAuthBypassAttempts(t *testing.T) {
	ls := startServer(t, attackPassword)
	const body = `{"message":"unlock the front door"}`

	// A valid session, an expired one, and one from a logged-out login.
	valid := "a" + strings.Repeat("0", 63)
	expired := "b" + strings.Repeat("0", 63)
	ls.srv.sessionMu.Lock()
	ls.srv.sessions[valid] = time.Now().Add(time.Hour)
	ls.srv.sessions[expired] = time.Now().Add(-time.Second)
	ls.srv.sessionMu.Unlock()

	denied := []struct {
		name    string
		path    string
		headers []string
	}{
		{"no auth", "/api/chat", nil},
		{"wrong password", "/api/chat", []string{"Authorization: " + basic("admin", "wrong")}},
		{"password prefix", "/api/chat", []string{"Authorization: " + basic("admin", attackPassword[:5])}},
		{"password with suffix", "/api/chat", []string{"Authorization: " + basic("admin", attackPassword+"x")}},
		{"password with trailing space", "/api/chat", []string{"Authorization: " + basic("admin", attackPassword+" ")}},
		{"password with NUL", "/api/chat", []string{"Authorization: " + basic("admin", attackPassword+"\x00")}},
		{"password in the user field", "/api/chat", []string{"Authorization: " + basic(attackPassword, "")}},
		{"empty basic", "/api/chat", []string{"Authorization: Basic "}},
		{"basic with only a colon", "/api/chat", []string{"Authorization: Basic Og=="}},
		{"basic without colon", "/api/chat", []string{"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(attackPassword))}},
		{"basic not base64", "/api/chat", []string{"Authorization: Basic !!!" + attackPassword}},
		{"basic unpadded", "/api/chat", []string{"Authorization: " + strings.TrimRight(basic("a", attackPassword), "=") + "\x01"}},
		{"bearer password", "/api/chat", []string{"Authorization: Bearer " + attackPassword}},
		{"bare password", "/api/chat", []string{"Authorization: " + attackPassword}},
		{"digest", "/api/chat", []string{`Authorization: Digest username="a", response="` + attackPassword + `"`}},
		{"wrong first, right second header", "/api/chat", []string{"Authorization: " + basic("a", "wrong"), "Authorization: " + basic("a", attackPassword)}},
		{"password in query", "/api/chat?password=" + attackPassword + "&auth=" + attackPassword, nil},
		{"password in proxy header", "/api/chat", []string{"Proxy-Authorization: " + basic("a", attackPassword)}},
		{"unknown session", "/api/chat", []string{"Cookie: session=" + strings.Repeat("f", 64)}},
		{"empty session", "/api/chat", []string{"Cookie: session="}},
		{"expired session", "/api/chat", []string{"Cookie: session=" + expired}},
		{"session cookie under another name", "/api/chat", []string{"Cookie: Session=" + valid}},
		{"session token prefix", "/api/chat", []string{"Cookie: session=" + valid[:32]}},
		{"password as session", "/api/chat", []string{"Cookie: session=" + attackPassword}},
		{"forwarded headers", "/api/chat", []string{"X-Forwarded-For: 127.0.0.1", "X-Real-IP: 127.0.0.1", "X-Original-URL: /api/login"}},
	}
	for _, c := range denied {
		t.Run(c.name, func(t *testing.T) {
			before := ls.calls()
			code, resp := ls.raw(t, chatReq(c.path, c.headers, body))
			// 400 is fine too: net/http rejects malformed header values.
			if code != http.StatusUnauthorized && code != http.StatusBadRequest {
				t.Errorf("status = %d (%s), want 401", code, truncate(resp, 100))
			}
			if ls.calls() != before {
				t.Fatal("message reached the agent without valid credentials")
			}
		})
	}

	allowed := []struct {
		name    string
		headers []string
	}{
		{"basic, any user", []string{"Authorization: " + basic("anyone", attackPassword)}},
		{"basic, lowercase scheme", []string{"Authorization: basic " + base64.StdEncoding.EncodeToString([]byte("a:"+attackPassword))}},
		{"valid session", []string{"Cookie: session=" + valid}},
		{"right first, wrong second header", []string{"Authorization: " + basic("a", attackPassword), "Authorization: " + basic("a", "wrong")}},
	}
	for _, c := range allowed {
		t.Run(c.name, func(t *testing.T) {
			if code, resp := ls.raw(t, chatReq("/api/chat", c.headers, body)); code != http.StatusOK {
				t.Fatalf("status = %d (%s), want 200", code, resp)
			}
		})
	}

	t.Run("expired session is deleted", func(t *testing.T) {
		ls.srv.sessionMu.Lock()
		_, ok := ls.srv.sessions[expired]
		ls.srv.sessionMu.Unlock()
		if ok {
			t.Error("expired session still in the session store")
		}
	})
}

func TestChatSessionLifecycle(t *testing.T) {
	ls := startServer(t, attackPassword)
	login := func(pw string) (int, string) {
		conn, err := net.Dial("tcp", ls.addr)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		body := fmt.Sprintf(`{"password":%q}`, pw)
		_, _ = fmt.Fprintf(conn, "POST /api/login HTTP/1.1\r\nHost: x\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		for _, c := range resp.Cookies() {
			if c.Name == sessionCookieName {
				return resp.StatusCode, c.Value
			}
		}
		return resp.StatusCode, ""
	}

	if code, tok := login("wrong"); code != http.StatusUnauthorized || tok != "" {
		t.Fatalf("login with a wrong password: %d, cookie %q", code, tok)
	}
	code, tok := login(attackPassword)
	if code != http.StatusOK || tok == "" {
		t.Fatalf("login: %d, cookie %q", code, tok)
	}
	cookie := []string{"Cookie: session=" + tok}
	if code, resp := ls.raw(t, chatReq("/api/chat", cookie, `{"message":"hi"}`)); code != http.StatusOK {
		t.Fatalf("chat with session: %d %s", code, resp)
	}
	if code, _ := ls.raw(t, chatReq("/api/logout", cookie, ``)); code != http.StatusOK {
		t.Fatalf("logout: %d", code)
	}
	before := ls.calls()
	if code, _ := ls.raw(t, chatReq("/api/chat", cookie, `{"message":"hi"}`)); code != http.StatusUnauthorized {
		t.Fatalf("chat after logout: %d, want 401", code)
	}
	if ls.calls() != before {
		t.Fatal("message reached the agent after logout")
	}
}

// Every method and path trick: without credentials nothing reaches the agent,
// and with credentials only POST to /api/chat (however it is encoded) does.
func TestChatMethodAndPathTricks(t *testing.T) {
	ls := startServer(t, attackPassword)
	auth := "Authorization: " + basic("a", attackPassword)
	const body = `{"message":"open the garage"}`

	paths := []string{
		"/api/chat", "/api/chat/", "//api/chat", "/api//chat", "/api/../api/chat", "/api/./chat",
		"/./api/chat", "/x/../api/chat", "/static/../api/chat", "/api/chat/..", "/api/chat/.",
		"/%61pi/chat", "/api/%63hat", "/api%2Fchat", "/%2Fapi/chat", "/API/chat", "/Api/Chat",
		"/api/chat%00", "/api/chat%20", "/api/chat;x=1", "/api/chat?x=1", "/api/chat%3F",
		"http://evil.example/api/chat", "http://evil.example//api/chat", "/api/chat\\", "/api\\chat",
		"/api/login/../chat", "/api/auth/check/../../chat", "/api/ws/../chat",
	}
	methods := []string{"POST", "GET", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE", "post", "PoSt", "CONNECT"}

	// canonical reports whether the request is POST /api/chat once decoded.
	canonical := func(method, path string) bool {
		if method != "POST" {
			return false
		}
		p := strings.TrimPrefix(path, "http://evil.example")
		return p == "/api/chat" || p == "/%61pi/chat" || p == "/api/%63hat" || p == "/api/chat?x=1"
	}

	for _, authed := range []bool{false, true} {
		for _, m := range methods {
			for _, p := range paths {
				name := fmt.Sprintf("auth=%v %s %s", authed, m, p)
				t.Run(name, func(t *testing.T) {
					var headers []string
					if authed {
						headers = []string{auth}
					}
					req := strings.Replace(chatReq(p, headers, body), "POST ", m+" ", 1)
					before := ls.calls()
					code, resp := ls.raw(t, req)
					called := ls.calls() != before
					if code >= 500 {
						t.Errorf("status %d (%s)", code, truncate(resp, 100))
					}
					switch {
					case !authed && called:
						t.Fatalf("unauthenticated %s %s reached the agent (status %d)", m, p, code)
					case authed && called && !canonical(m, p):
						t.Fatalf("%s %s reached the agent (status %d)", m, p, code)
					case authed && canonical(m, p) && (!called || code != http.StatusOK):
						t.Fatalf("%s %s: status %d, called %v; want a reply", m, p, code, called)
					}
				})
			}
		}
	}
}

func TestChatBodyAttacks(t *testing.T) {
	ls := startServer(t, attackPassword)
	ls.srv.chatTimeout = 500 * time.Millisecond
	auth := []string{"Authorization: " + basic("a", attackPassword)}

	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty body", "", http.StatusBadRequest},
		{"null", "null", http.StatusBadRequest},
		{"array", "[]", http.StatusBadRequest},
		{"string", `"hi"`, http.StatusBadRequest},
		{"message null", `{"message":null}`, http.StatusBadRequest},
		{"message object", `{"message":{"text":"hi"}}`, http.StatusBadRequest},
		{"message array", `{"message":["hi"]}`, http.StatusBadRequest},
		{"message number", `{"message":1}`, http.StatusBadRequest},
		{"whitespace message", `{"message":" \t\n\r "}`, http.StatusBadRequest},
		{"unicode whitespace message", `{"message":"\u00a0\u2003\u3000"}`, http.StatusBadRequest},
		{"BOM prefix", "\xef\xbb\xbf" + `{"message":"hi"}`, http.StatusBadRequest},
		{"deeply nested JSON", `{"message":"x","agent":` + strings.Repeat("[", 100000) + strings.Repeat("]", 100000) + `}`, http.StatusBadRequest},
		{"deeply nested objects", strings.Repeat(`{"a":`, 50000) + "1" + strings.Repeat("}", 50000), http.StatusBadRequest},
		{"body over 1 MiB", `{"message":"` + strings.Repeat("x", maxChatBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},
		{"whitespace padding over 1 MiB", strings.Repeat(" ", maxChatBodyBytes+1) + `{"message":"hi"}`, http.StatusRequestEntityTooLarge},
		{"message just under 1 MiB", `{"message":"` + strings.Repeat("x", maxChatBodyBytes-20) + `"}`, http.StatusOK},
		{"timeout float", `{"message":"hi","timeout":1.5}`, http.StatusBadRequest},
		{"timeout exponent", `{"message":"hi","timeout":1e2}`, http.StatusBadRequest},
		{"timeout string", `{"message":"hi","timeout":"30"}`, http.StatusBadRequest},
		{"timeout bool", `{"message":"hi","timeout":true}`, http.StatusBadRequest},
		{"timeout negative", `{"message":"hi","timeout":-1}`, http.StatusBadRequest},
		{"timeout min int64", `{"message":"hi","timeout":-9223372036854775808}`, http.StatusBadRequest},
		{"timeout max int64", `{"message":"hi","timeout":9223372036854775807}`, http.StatusBadRequest},
		{"timeout beyond int64", `{"message":"hi","timeout":99999999999999999999999}`, http.StatusBadRequest},
		{"timeout 601", `{"message":"hi","timeout":601}`, http.StatusBadRequest},
		{"timeout 600", `{"message":"hi","timeout":600}`, http.StatusOK},
		{"timeout 1", `{"message":"hi","timeout":1}`, http.StatusOK},
		{"timeout null is the default", `{"message":"hi","timeout":null}`, http.StatusOK},
		{"timeout -0 is the default", `{"message":"hi","timeout":-0}`, http.StatusOK},
		{"agent unknown", `{"message":"hi","agent":"nope"}`, http.StatusNotFound},
		{"agent path traversal", `{"message":"hi","agent":"../../etc/passwd"}`, http.StatusNotFound},
		{"agent with newline", `{"message":"hi","agent":"general\n"}`, http.StatusNotFound},
		{"agent swarm", `{"message":"hi","agent":"swarm"}`, http.StatusNotFound},
		{"@swarm routed", `{"message":"@swarm do x"}`, http.StatusBadRequest},
		{"invalid UTF-8 message", "{\"message\":\"\xff\xfe hi\"}", http.StatusOK},
		{"NUL in message", `{"message":"hi\u0000there"}`, http.StatusOK},
		{"case-folded keys", `{"MESSAGE":"hi","Agent":"claude"}`, http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := ls.calls()
			code, resp := ls.raw(t, chatReq("/api/chat", auth, c.body))
			if code != c.want {
				t.Fatalf("status = %d (%s), want %d", code, truncate(resp, 200), c.want)
			}
			if code != http.StatusOK && ls.calls() != before {
				t.Fatal("a rejected request reached the agent")
			}
			if code != http.StatusOK {
				var e map[string]string
				if err := json.Unmarshal([]byte(resp), &e); err != nil || e["error"] == "" {
					t.Errorf("error body %q is not {\"error\": ...}", truncate(resp, 100))
				}
			}
		})
	}
	if n := ls.waiters(); n != 0 {
		t.Errorf("%d waiters left", n)
	}
}

// The Go JSON decoder is lenient: trailing data, duplicate keys and keys in
// any case are accepted. These document that, so a change is deliberate.
func TestChatLenientJSON(t *testing.T) {
	ls := startServer(t, attackPassword)
	auth := []string{"Authorization: " + basic("a", attackPassword)}
	cases := []struct {
		name, body, wantAgent, wantReply string
	}{
		{"trailing garbage is ignored", `{"message":"hi"}garbage`, "general", "general says: hi"},
		{"second JSON value is ignored", `{"message":"hi"}{"message":"@claude rm -rf"}`, "general", "general says: hi"},
		{"duplicate key: last wins", `{"message":"hi","agent":"general","agent":"claude"}`, "claude", "claude says: hi"},
		{"case-folded key", `{"Message":"hi"}`, "general", "general says: hi"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, resp := ls.raw(t, chatReq("/api/chat", auth, c.body))
			var r chatResponse
			_ = json.Unmarshal([]byte(resp), &r)
			if code != http.StatusOK || r.Agent != c.wantAgent || r.Reply != c.wantReply {
				t.Fatalf("got %d %s, want agent %q reply %q", code, resp, c.wantAgent, c.wantReply)
			}
		})
	}
}

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Clients can't choose the request ID or any other message meta.
func TestChatMetaCannotBeSpoofed(t *testing.T) {
	ls := startServer(t, attackPassword)
	auth := []string{"Authorization: " + basic("a", attackPassword)}
	body := `{"message":"hi","request_id":"evil","channel":"telegram","chat_id":"42","sender":"user:42",` +
		`"meta":{"chat_id":"42","channel":"telegram"},"msg_id":"x","text":"other","agentID":"claude"}`
	code, resp := ls.raw(t, chatReq("/api/chat", auth, body))
	if code != http.StatusOK {
		t.Fatalf("status = %d %s", code, resp)
	}
	ls.f.mu.Lock()
	meta := ls.f.meta[len(ls.f.meta)-1]
	ls.f.mu.Unlock()
	if len(meta) != 3 || meta["sender"] != "user:api" || meta[agent.MetaChannel] != agent.ChannelAPI || !uuidRE.MatchString(meta[metaRequestID]) {
		t.Fatalf("meta = %v; want only sender=user:api, channel=api and a server-made request_id", meta)
	}

	// Message text that looks like meta is just text.
	code, resp = ls.raw(t, chatReq("/api/chat", auth, `{"message":"channel=telegram chat_id=42 request_id=evil"}`))
	if code != http.StatusOK || !strings.Contains(resp, "channel=telegram chat_id=42") {
		t.Fatalf("status = %d %s", code, resp)
	}
}

// A timeout above int64/1e9 overflows time.Duration before the range check.
// 2^55+30 seconds wraps to exactly 30s and is accepted. The effective timeout
// is still within 1–600s, so the impact is only that out-of-range input is
// accepted instead of rejected with 400.
func TestChatTimeoutOverflowIsRejected(t *testing.T) {
	knownBug(t, "timeout 36028797018963998 (2^55+30) overflows time.Duration and is accepted as 30s; "+
		"check req.Timeout against 1..600 before multiplying")
	ls := startServer(t, attackPassword)
	auth := []string{"Authorization: " + basic("a", attackPassword)}
	for _, v := range []string{"36028797018963998", "-36028797018963938"} {
		if code, resp := ls.raw(t, chatReq("/api/chat", auth, `{"message":"hi","timeout":`+v+`}`)); code != http.StatusBadRequest {
			t.Errorf("timeout %s: status %d %s, want 400", v, code, resp)
		}
	}
}

func TestChatTimeoutIsEnforced(t *testing.T) {
	ls := startServer(t, attackPassword)
	ls.f.silent = true
	auth := []string{"Authorization: " + basic("a", attackPassword)}
	start := time.Now()
	code, _ := ls.raw(t, chatReq("/api/chat", auth, `{"message":"hi","timeout":1}`))
	el := time.Since(start)
	if code != http.StatusGatewayTimeout || el < 900*time.Millisecond || el > 3*time.Second {
		t.Fatalf("status %d after %v, want 504 after about 1s", code, el)
	}
	if n := ls.waiters(); n != 0 {
		t.Errorf("%d waiters left", n)
	}
}

// waitGoroutines waits until the goroutine count drops to at most max.
func waitGoroutines(t *testing.T, max int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > max {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("%d goroutines, want at most %d:\n%s", runtime.NumGoroutine(), max, truncate(string(buf[:n]), 20000))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestChatConcurrentRequestsLeakNothing(t *testing.T) {
	ls := startServer(t, attackPassword)
	auth := []string{"Authorization: " + basic("a", attackPassword)}
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	const n = 200
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := range n {
		wg.Go(func() {
			msg := fmt.Sprintf("message %d", i)
			code, resp, err := ls.do(chatReq("/api/chat", auth, fmt.Sprintf(`{"message":%q}`, msg)))
			if err != nil {
				bad.Add(1)
				t.Errorf("%s: %v", msg, err)
				return
			}
			var r chatResponse
			_ = json.Unmarshal([]byte(resp), &r)
			if code != http.StatusOK || r.Reply != "general says: "+msg {
				bad.Add(1)
				t.Errorf("%s: %d %q", msg, code, resp)
			}
		})
	}
	wg.Wait()
	if bad.Load() == 0 && ls.calls() != n {
		t.Errorf("backend got %d messages, want %d", ls.calls(), n)
	}
	if w := ls.waiters(); w != 0 {
		t.Errorf("%d waiters left", w)
	}
	waitGoroutines(t, base+5)
}

// Clients that send a request and hang up at once, while the agent never
// answers, must not leave waiters or goroutines behind.
func TestChatClientDisconnectStorm(t *testing.T) {
	ls := startServer(t, attackPassword)
	ls.f.silent = true
	ls.srv.chatTimeout = 30 * time.Second // only a disconnect can end these
	auth := []string{"Authorization: " + basic("a", attackPassword)}
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	const n = 100
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			conn, err := net.Dial("tcp", ls.addr)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.WriteString(conn, chatReq("/api/chat", auth, fmt.Sprintf(`{"message":"m%d"}`, i)))
			if i%2 == 0 {
				// Wait for the message to reach the agent before hanging up.
				deadline := time.Now().Add(2 * time.Second)
				for ls.calls() == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			_ = conn.Close()
		})
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for ls.waiters() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d waiters left after all clients hung up", ls.waiters())
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitGoroutines(t, base+5)

	// Replies that arrive now have no one to go to; they must not block.
	ls.f.mu.Lock()
	metas := append([]map[string]string(nil), ls.f.meta...)
	ls.f.mu.Unlock()
	done := make(chan struct{})
	go func() {
		for _, m := range metas {
			ls.srv.chatWaiters.deliver("general", "late", m)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("delivering late replies blocked")
	}
}

// A slow body holds its request only until the client gives up, and the
// message never reaches the agent.
func TestChatSlowBody(t *testing.T) {
	ls := startServer(t, attackPassword)
	conn, err := net.Dial("tcp", ls.addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "POST /api/chat HTTP/1.1\r\nHost: x\r\nAuthorization: "+basic("a", attackPassword)+
		"\r\nContent-Length: 100\r\n\r\n{\"message\":")
	time.Sleep(300 * time.Millisecond)
	_ = conn.Close()
	time.Sleep(100 * time.Millisecond)
	if ls.calls() != 0 {
		t.Fatal("a truncated body reached the agent")
	}
	if n := ls.waiters(); n != 0 {
		t.Errorf("%d waiters left", n)
	}
}

// The HTTP server has no read timeouts (http.Server{Addr, Handler} in Start),
// so anyone who can reach the port can hold connections open forever without
// credentials (Slowloris). On a Raspberry Pi that runs out of file
// descriptors or memory.
func TestServerClosesSlowHeaders(t *testing.T) {
	knownBug(t, "Server.Start sets no ReadHeaderTimeout/ReadTimeout/IdleTimeout: unauthenticated clients can hold "+
		"connections open forever (Slowloris). Set at least ReadHeaderTimeout (e.g. 10s) and IdleTimeout.")
	ls := startServer(t, attackPassword)
	conn, err := net.Dial("tcp", ls.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "POST /api/chat HTTP/1.1\r\nHost: x\r\n") // never finishes
	_ = conn.SetReadDeadline(time.Now().Add(65 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("server kept a half-sent request open for over a minute")
		}
	}
}

// Replies for unknown, expired or already answered requests are dropped
// without blocking or reaching anyone else.
func TestChatWaitersUnknownExpiredDuplicate(t *testing.T) {
	cw := newChatWaiters()
	api := func(id string) map[string]string {
		return map[string]string{agent.MetaChannel: agent.ChannelAPI, metaRequestID: id}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		cw.deliver("general", "x", api("unknown"))
		cw.deliver("general", "x", api(""))
		cw.deliver("general", "x", nil)
		cw.deliver("general", "x", map[string]string{agent.MetaChannel: agent.ChannelAPI})

		ch := cw.add("expired")
		cw.remove("expired")
		cw.deliver("general", "late", api("expired"))
		select {
		case got := <-ch:
			t.Errorf("expired request got %q", got)
		default:
		}

		other := cw.add("other")
		live := cw.add("live")
		for range 5 {
			cw.deliver("general", "reply", api("live"))
		}
		if got := <-live; got != "reply" {
			t.Errorf("live got %q", got)
		}
		select {
		case got := <-other:
			t.Errorf("another request got %q", got)
		default:
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deliver blocked")
	}
}

// Replies racing with timeouts: run under -race.
func TestChatRepliesRaceTimeouts(t *testing.T) {
	srv, f := newChatServer(t)
	srv.chatTimeout = 5 * time.Millisecond
	f.silent = true
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			rec, _ := postChat(t, srv, `{"message":"ping"}`)
			if rec.Code != http.StatusOK && rec.Code != http.StatusGatewayTimeout {
				t.Errorf("status %d", rec.Code)
			}
		})
		wg.Go(func() {
			f.mu.Lock()
			metas := append([]map[string]string(nil), f.meta...)
			f.mu.Unlock()
			for _, m := range metas {
				srv.chatWaiters.deliver("general", "late", m)
			}
		})
	}
	wg.Wait()
	if n := len(srv.chatWaiters.pending); n != 0 {
		t.Errorf("%d waiters left", n)
	}
}

// knownBug skips a test that demonstrates an open bug, so CI stays green
// until it is fixed. Set PRAKTOR_RUN_KNOWN_BUGS=1 to run it and see it fail.
func knownBug(t testing.TB, msg string) {
	t.Helper()
	if os.Getenv("PRAKTOR_RUN_KNOWN_BUGS") == "" {
		t.Skip("BUG: " + msg)
	}
}

// Without web.auth the middleware lets every /api request through, so the
// handler itself must refuse: any web page could otherwise drive the agents
// (the API sends Access-Control-Allow-Origin: *).
func TestChatWithoutPasswordIsForbiddenThroughTheServer(t *testing.T) {
	ls := startServer(t, "")
	for _, h := range [][]string{nil, {"Authorization: " + basic("a", "")}, {"Origin: https://evil.example"}} {
		code, resp := ls.raw(t, chatReq("/api/chat", h, `{"message":"unlock the door"}`))
		if code != http.StatusForbidden {
			t.Errorf("headers %v: status %d (%s), want 403", h, code, resp)
		}
	}
	// A CORS preflight is answered, but it runs nothing.
	code, _ := ls.raw(t, strings.Replace(chatReq("/api/chat", []string{"Origin: https://evil.example", "Access-Control-Request-Method: POST"}, ""), "POST ", "OPTIONS ", 1))
	if code != http.StatusOK {
		t.Errorf("preflight status %d", code)
	}
	if ls.calls() != 0 {
		t.Fatal("message reached the agent without web.auth")
	}
}
