package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/mtzanidakis/praktor/internal/agent"
)

const (
	defaultChatTimeout = 180 * time.Second
	maxChatTimeout     = 600 * time.Second
	maxChatBodyBytes   = 1 << 20
	metaRequestID      = "request_id"
)

// chatBackend is what the chat endpoint needs from the router, registry and
// orchestrator. It is an interface so the handler can be tested without
// Docker or NATS.
type chatBackend interface {
	Route(ctx context.Context, message string) (agentID, cleaned string, err error)
	HasAgent(id string) bool
	HandleMessage(ctx context.Context, agentID, text string, meta map[string]string) error
}

// serverChat is the production chatBackend, backed by the server's router,
// registry and orchestrator.
type serverChat struct{ s *Server }

func (c serverChat) Route(ctx context.Context, message string) (string, string, error) {
	return c.s.router.Route(ctx, message)
}

func (c serverChat) HasAgent(id string) bool {
	_, ok := c.s.registry.GetDefinition(id)
	return ok
}

func (c serverChat) HandleMessage(ctx context.Context, agentID, text string, meta map[string]string) error {
	return c.s.orch.HandleMessage(ctx, agentID, text, meta)
}

// chatWaiters hands agent replies to the HTTP requests waiting for them,
// matched by the request ID carried in the message meta.
type chatWaiters struct {
	mu      sync.Mutex
	pending map[string]chan string
}

func newChatWaiters() *chatWaiters {
	return &chatWaiters{pending: make(map[string]chan string)}
}

func (cw *chatWaiters) add(id string) chan string {
	ch := make(chan string, 1)
	cw.mu.Lock()
	cw.pending[id] = ch
	cw.mu.Unlock()
	return ch
}

func (cw *chatWaiters) remove(id string) {
	cw.mu.Lock()
	delete(cw.pending, id)
	cw.mu.Unlock()
}

// deliver is registered as an orchestrator output listener.
func (cw *chatWaiters) deliver(_ string, content string, meta map[string]string) {
	if meta[agent.MetaChannel] != agent.ChannelAPI {
		return
	}
	id := meta[metaRequestID]
	cw.mu.Lock()
	ch, ok := cw.pending[id]
	delete(cw.pending, id)
	cw.mu.Unlock()
	if ok {
		ch <- content // buffered; one reply per request
	}
}

type chatRequest struct {
	Message string `json:"message"`
	Agent   string `json:"agent,omitempty"`
	Timeout int    `json:"timeout,omitempty"` // seconds; 0 = default
}

type chatResponse struct {
	Agent string `json:"agent"`
	Reply string `json:"reply"`
}

// chat sends a message to an agent and waits for its reply, for HTTP clients
// (Home Assistant, scripts) that need a request/response instead of Telegram.
//
// With the admin password, a message without "agent" is routed like a
// Telegram message: an @agent_name prefix, then smart routing, then the
// default agent. With the chat token (web.chat_token), it never goes through
// smart routing, which would send the text to the default agent's model: the
// agent is "agent", else an @agent_name prefix, else the first of
// web.chat_agents, and must be one of web.chat_agents when that is set.
// Agents keep one session each, so the conversation is shared with Telegram.
//
// It requires web.auth or web.chat_token: the API sends
// Access-Control-Allow-Origin: *, so without credentials any web page could
// make agents run and read the reply.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if s.chatBackend == nil {
		jsonError(w, "chat is not available", http.StatusServiceUnavailable)
		return
	}
	viaToken := hasChatToken(r.Context())
	if !viaToken && s.cfg.Auth == "" {
		if s.cfg.ChatToken != "" {
			jsonError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		jsonError(w, "chat requires web.auth or web.chat_token to be set", http.StatusForbidden)
		return
	}

	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatBodyBytes)).Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			jsonError(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		jsonError(w, "message is required", http.StatusBadRequest)
		return
	}
	// Checked as an integer before converting, so huge values can't overflow
	// time.Duration into a small, valid-looking timeout.
	if req.Timeout != 0 && (req.Timeout < 1 || req.Timeout > int(maxChatTimeout/time.Second)) {
		jsonError(w, "timeout must be between 1 and 600 seconds", http.StatusBadRequest)
		return
	}
	timeout := s.chatTimeout
	if timeout <= 0 {
		timeout = defaultChatTimeout
	}
	if req.Timeout != 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}
	deadline := time.Now().Add(timeout) // covers routing and the reply

	// The orchestrator runs the agent's queue on the context it is given, so
	// it must outlive the request: a client that disconnects must not cancel
	// a run, a container start, or the messages queued behind it.
	runCtx := s.baseCtx
	if runCtx == nil {
		runCtx = context.Background()
	}

	agentID, text := req.Agent, req.Message
	if viaToken {
		var status int
		var msg string
		agentID, text, status, msg = s.tokenAgent(req)
		if status != 0 {
			jsonError(w, msg, status)
			return
		}
	} else if agentID != "" {
		if !s.chatBackend.HasAgent(agentID) {
			jsonError(w, "unknown agent: "+agentID, http.StatusNotFound)
			return
		}
	} else {
		// Smart routing may start the default agent, so it runs on runCtx
		// (bounded by the deadline) rather than the request context.
		routeCtx, cancel := context.WithDeadline(runCtx, deadline)
		var err error
		agentID, text, err = s.chatBackend.Route(routeCtx, req.Message)
		cancel()
		if err != nil {
			jsonError(w, "routing failed: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if agentID == "swarm" {
			jsonError(w, "swarms are not supported over the chat API; use POST /api/swarms", http.StatusBadRequest)
			return
		}
		if text == "" { // just "@agent": send the message as typed, like Telegram
			text = req.Message
		}
		if r.Context().Err() != nil {
			return // client went away while routing; don't run the agent
		}
		if !time.Now().Before(deadline) {
			// Don't run the agent for a request that has already failed: its
			// reply would reach no one, and a retry would run it twice.
			jsonError(w, "timed out while routing the message", http.StatusGatewayTimeout)
			return
		}
	}

	id := uuid.New().String()
	reply := s.chatWaiters.add(id)
	defer s.chatWaiters.remove(id)

	meta := map[string]string{
		"sender":          "user:api",
		agent.MetaChannel: agent.ChannelAPI,
		metaRequestID:     id,
	}
	if err := s.chatBackend.HandleMessage(runCtx, agentID, text, meta); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()

	select {
	case content := <-reply:
		jsonResponse(w, chatResponse{Agent: agentID, Reply: content})
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			jsonError(w, "timed out waiting for the agent's reply", http.StatusGatewayTimeout)
		}
		// Otherwise the client went away; there is no one to answer.
	}
}

type chatTokenKey struct{}

func withChatToken(ctx context.Context) context.Context {
	return context.WithValue(ctx, chatTokenKey{}, true)
}

func hasChatToken(ctx context.Context) bool {
	ok, _ := ctx.Value(chatTokenKey{}).(bool)
	return ok
}

// validChatToken reports whether the request carries web.chat_token as a
// bearer token. The comparison is constant-time.
func (s *Server) validChatToken(r *http.Request) bool {
	if s.cfg.ChatToken == "" {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(token), []byte(s.cfg.ChatToken)) == 1
}

// tokenAgent picks the agent for a chat-token request without smart routing.
// It returns a non-zero HTTP status and message when the request is refused.
func (s *Server) tokenAgent(req chatRequest) (agentID, text string, status int, msg string) {
	allowed := s.cfg.ChatAgents
	agentID, text = req.Agent, req.Message
	if agentID == "" {
		if name, rest, _ := strings.Cut(req.Message, " "); strings.HasPrefix(name, "@") && s.chatBackend.HasAgent(name[1:]) {
			agentID = name[1:]
			if strings.TrimSpace(rest) != "" {
				text = rest
			}
		} else if len(allowed) > 0 {
			agentID = allowed[0]
		} else {
			return "", "", http.StatusBadRequest, "agent is required (or set web.chat_agents)"
		}
	}
	if len(allowed) > 0 && !slices.Contains(allowed, agentID) {
		return "", "", http.StatusForbidden, "agent not allowed for the chat token: " + agentID
	}
	if !s.chatBackend.HasAgent(agentID) {
		return "", "", http.StatusNotFound, "unknown agent: " + agentID
	}
	return agentID, text, 0, ""
}
