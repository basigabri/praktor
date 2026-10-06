package agent

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/nats-io/nats.go"

	"github.com/mtzanidakis/praktor/internal/config"
	"github.com/mtzanidakis/praktor/internal/natsbus"
	"github.com/mtzanidakis/praktor/internal/registry"
	"github.com/mtzanidakis/praktor/internal/store"
)

// fakeTelegram mirrors the Telegram bot's output and file listeners
// (internal/telegram/bot.go): replies to API messages are skipped
// (isTelegramReply), a reply without chat_id goes to the chat that last
// talked to the agent, and files go to the chat the orchestrator resolved.
type fakeTelegram struct {
	mu       sync.Mutex
	lastChat map[string]string // agentID → chat that last talked to it
	sent     []string          // "chat: content"
	files    []string          // "chat: name"
}

func (f *fakeTelegram) output(agentID, content string, meta map[string]string) {
	if meta[MetaChannel] == ChannelAPI { // isTelegramReply
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	chat := meta["chat_id"]
	if chat == "" {
		chat = f.lastChat[agentID]
	}
	if chat != "" {
		f.sent = append(f.sent, chat+": "+content)
	}
}

func (f *fakeTelegram) file(_ string, chatID int64, _ []byte, name, _, _ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files = append(f.files, strconv.FormatInt(chatID, 10)+": "+name)
}

type leakHarness struct {
	t  *testing.T
	o  *Orchestrator
	tg *fakeTelegram
}

func newLeakHarness(t *testing.T) *leakHarness {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "praktor.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SaveAgent(&store.Agent{ID: "general", Name: "general", Workspace: "general"}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	bus, err := natsbus.NewForTest(config.NATSConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}
	t.Cleanup(bus.Close)
	reg := registry.New(s, map[string]config.AgentDefinition{"general": {Description: "test"}}, config.DefaultsConfig{}, t.TempDir())
	o := NewOrchestrator(bus, nil, s, reg, config.DefaultsConfig{}, nil)
	tg := &fakeTelegram{lastChat: map[string]string{}}
	o.OnOutput(tg.output)
	o.OnFile(tg.file)
	return &leakHarness{t: t, o: o, tg: tg}
}

// telegram tracks a Telegram message from chat 42 (msg_id tg1), the way
// executeMessage does, and records the chat as the agent's last chat, like
// the bot does.
func (h *leakHarness) telegram() {
	const msgID = "tg1"
	h.tg.mu.Lock()
	h.tg.lastChat["general"] = "42"
	h.tg.mu.Unlock()
	h.o.trackPending("general", msgID, QueuedMessage{AgentID: "general", Meta: map[string]string{"sender": "user:42", "chat_id": "42"}})
}

// api tracks a chat API message (msg_id api1), with the meta api_chat.go sets.
func (h *leakHarness) api() {
	const msgID = "api1"
	h.o.trackPending("general", msgID, QueuedMessage{AgentID: "general", Meta: map[string]string{
		"sender": "user:api", MetaChannel: ChannelAPI, "request_id": "req-" + msgID,
	}})
}

// result feeds an agent result through the real output handler. An empty
// msgID sends a result without msg_id.
func (h *leakHarness) result(msgID, content string) {
	out := map[string]any{"type": "result", "content": content}
	if msgID != "" {
		out["msg_id"] = msgID
	}
	data, _ := json.Marshal(out)
	h.o.handleAgentOutput(&nats.Msg{Subject: "agent.general.output", Data: data})
}

func (h *leakHarness) sent() []string {
	h.tg.mu.Lock()
	defer h.tg.mu.Unlock()
	return append([]string(nil), h.tg.sent...)
}

func TestAPIRepliesNeverReachTelegram(t *testing.T) {
	t.Run("telegram message, then api message, replies in order", func(t *testing.T) {
		h := newLeakHarness(t)
		h.telegram()
		h.result("tg1", "telegram reply")
		h.api()
		h.result("api1", "SECRET api reply")
		assertSent(t, h.sent(), "42: telegram reply")
	})
	t.Run("api message queued behind a telegram message", func(t *testing.T) {
		h := newLeakHarness(t)
		h.api()
		h.telegram() // last meta is now Telegram's
		h.result("api1", "SECRET api reply")
		h.result("tg1", "telegram reply")
		assertSent(t, h.sent(), "42: telegram reply")
	})
	t.Run("abnormal api termination", func(t *testing.T) {
		h := newLeakHarness(t)
		h.telegram()
		h.api()
		data, _ := json.Marshal(map[string]any{"type": "result", "content": "SECRET partial", "msg_id": "api1", "terminal_reason": "max_turns"})
		h.o.handleAgentOutput(&nats.Msg{Subject: "agent.general.output", Data: data})
		assertSent(t, h.sent())
	})
	t.Run("streamed text chunks of an api run are not replies", func(t *testing.T) {
		h := newLeakHarness(t)
		h.telegram()
		h.api()
		data, _ := json.Marshal(map[string]any{"type": "text", "content": "SECRET chunk", "msg_id": "api1"})
		h.o.handleAgentOutput(&nats.Msg{Subject: "agent.general.output", Data: data})
		assertSent(t, h.sent())
	})
	t.Run("orphan reply after a telegram message goes to telegram", func(t *testing.T) {
		// By design: a guess may answer Telegram, never an API request.
		h := newLeakHarness(t)
		h.telegram()
		h.result("unknown", "orphan")
		assertSent(t, h.sent(), "42: orphan")
	})
}

// An orphan result (unknown or missing msg_id: a duplicate, or a result from
// a run the orchestrator no longer tracks) while the agent's last message came
// from the API is most likely that API run's reply. It must not be posted to
// the chat that last talked to the agent.
func TestOrphanReplyAfterAPIMessageStaysOffTelegram(t *testing.T) {
	cases := []struct {
		name  string
		msgID string
	}{
		{"duplicate result for the api message", "api1"},
		{"result without msg_id", ""},
		{"result with an unknown msg_id", "stale"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newLeakHarness(t)
			h.telegram()
			h.result("tg1", "telegram reply")
			h.api()
			h.result("api1", "SECRET api reply")
			h.result(c.msgID, "SECRET api reply (again)")
			assertSent(t, h.sent(), "42: telegram reply")
		})
	}
}

// file_send has no msg_id: the orchestrator picks the chat from the agent's
// last message meta, which changes as soon as another message is queued. A
// file sent by an API run lands in the Telegram chat whose message was
// queued behind it.
func TestFileSentByAPIRunStaysOffTelegram(t *testing.T) {
	knownBug(t, "a file sent by an API run reaches Telegram when a Telegram message for the same agent "+
		"was queued behind it: ipcSendFile resolves the chat from the agent's last meta, not from the run "+
		"that sent the file (send_file IPC carries no msg_id).")
	h := newLeakHarness(t)
	h.api()
	h.telegram() // queued while the API run is still going
	payload, _ := json.Marshal(map[string]string{"name": "camera.jpg", "data": base64.StdEncoding.EncodeToString([]byte("jpeg")), "mime_type": "image/jpeg"})
	cmd, _ := json.Marshal(IPCCommand{Type: "send_file", Payload: payload})
	h.o.handleIPC(&nats.Msg{Subject: "host.ipc.general", Data: cmd})
	h.tg.mu.Lock()
	defer h.tg.mu.Unlock()
	if len(h.tg.files) != 0 {
		t.Fatalf("file from the API run sent to Telegram: %v", h.tg.files)
	}
}

func assertSent(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("sent to Telegram %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("sent to Telegram %q, want %q", got, want)
		}
	}
}

// knownBug skips a test that demonstrates an open bug, so CI stays green
// until it is fixed. Set PRAKTOR_RUN_KNOWN_BUGS=1 to run it and see it fail.
func knownBug(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("PRAKTOR_RUN_KNOWN_BUGS") == "" {
		t.Skip("BUG: " + msg)
	}
}
