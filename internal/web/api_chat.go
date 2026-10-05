package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
// Without "agent", the message is routed like a Telegram message: an
// @agent_name prefix, then smart routing, then the default agent. Agents keep
// one session each, so the conversation is shared with Telegram.
//
// It requires web.auth: the API sends Access-Control-Allow-Origin: *, so
// without a password any web page could make agents run and read the reply.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	if s.chatBackend == nil {
		jsonError(w, "chat is not available", http.StatusServiceUnavailable)
		return
	}
	if s.cfg.Auth == "" {
		jsonError(w, "chat requires web.auth to be set", http.StatusForbidden)
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
	timeout := s.chatTimeout
	if timeout <= 0 {
		timeout = defaultChatTimeout
	}
	if req.Timeout != 0 {
		timeout = time.Duration(req.Timeout) * time.Second
		if timeout < time.Second || timeout > maxChatTimeout {
			jsonError(w, "timeout must be between 1 and 600 seconds", http.StatusBadRequest)
			return
		}
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
	if agentID != "" {
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
