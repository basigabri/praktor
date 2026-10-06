package container

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// fakeDocker is a minimal Docker Engine API for attachSelf: network
// inspect/create, container inspect and network connect. Each handler can be
// replaced to inject failures.
type fakeDocker struct {
	mu         sync.Mutex
	networkOK  bool                                       // praktor-net exists
	createErr  int                                        // status for network create (0 = 201)
	inspect    map[string]dockercontainer.InspectResponse // by ID or name
	rawInspect string                                     // if set, returned verbatim for any inspect
	connectErr int                                        // status for network connect (0 = 200)
	hang       bool                                       // never answer
	connects   []network.ConnectRequest
	calls      []string
}

var apiVersionPrefix = regexp.MustCompile(`^/v[0-9.]+`)

func (d *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := apiVersionPrefix.ReplaceAllString(r.URL.Path, "")
	d.mu.Lock()
	d.calls = append(d.calls, r.Method+" "+p)
	hang := d.hang
	d.mu.Unlock()
	if hang {
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Api-Version", "1.47")

	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case p == "/_ping":
		_, _ = w.Write([]byte("OK"))
	case r.Method == http.MethodGet && p == "/networks/"+networkName:
		if !d.networkOK {
			http.Error(w, `{"message":"network praktor-net not found"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"Name":"praktor-net","Id":"n1"}`))
	case r.Method == http.MethodPost && p == "/networks/create":
		if d.createErr != 0 {
			http.Error(w, `{"message":"boom"}`, d.createErr)
			return
		}
		d.networkOK = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"n1"}`))
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
		if d.rawInspect != "" {
			_, _ = w.Write([]byte(d.rawInspect))
			return
		}
		key := strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")
		c, ok := d.inspect[key]
		if !ok {
			http.Error(w, `{"message":"No such container: `+key+`"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(c)
	case r.Method == http.MethodPost && p == "/networks/"+networkName+"/connect":
		var req network.ConnectRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.connects = append(d.connects, req)
		if d.connectErr != 0 {
			http.Error(w, `{"message":"connect refused"}`, d.connectErr)
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, `{"message":"unexpected `+r.Method+" "+p+`"}`, http.StatusNotImplemented)
	}
}

func (d *fakeDocker) connectCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.connects)
}

func newFakeDockerManager(t *testing.T, d *fakeDocker) *Manager {
	t.Helper()
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithAPIVersion("1.47"))
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &Manager{docker: cli, active: map[string]*ContainerInfo{}}
}

// gw builds the gateway's own container as found by hostname (findSelf's
// fallback, which is what runs outside a container, like in CI).
func gw(host, mode string, nets map[string]*network.EndpointSettings) dockercontainer.InspectResponse {
	return dockercontainer.InspectResponse{
		ID:              selfID,
		Config:          &dockercontainer.Config{Hostname: host},
		HostConfig:      &dockercontainer.HostConfig{NetworkMode: dockercontainer.NetworkMode(mode)},
		NetworkSettings: &dockercontainer.NetworkSettings{Networks: nets},
	}
}

func TestAttachSelfChaos(t *testing.T) {
	const host = "e2e-praktor"
	cases := []struct {
		name        string
		d           *fakeDocker
		wantDone    bool
		wantConnect int
	}{
		{
			name:        "gateway on hassio is connected with its hostname as alias",
			d:           &fakeDocker{networkOK: true, inspect: map[string]dockercontainer.InspectResponse{host: gw(host, "hassio", map[string]*network.EndpointSettings{"hassio": {}})}},
			wantDone:    true,
			wantConnect: 1,
		},
		{
			name:        "missing network is created first",
			d:           &fakeDocker{inspect: map[string]dockercontainer.InspectResponse{host: gw(host, "bridge", nil)}},
			wantDone:    true,
			wantConnect: 1,
		},
		{
			name: "network create fails: retry later",
			d:    &fakeDocker{createErr: http.StatusInternalServerError, inspect: map[string]dockercontainer.InspectResponse{host: gw(host, "bridge", nil)}},
		},
		{
			name: "own container not found: retry later",
			d:    &fakeDocker{networkOK: true},
		},
		{
			name: "container found by name has another hostname: never connect it",
			d: &fakeDocker{networkOK: true, inspect: map[string]dockercontainer.InspectResponse{
				host: gw("someone-else", "bridge", nil),
			}},
		},
		{
			name: "container found by name has no config: never connect it",
			d: &fakeDocker{networkOK: true, inspect: map[string]dockercontainer.InspectResponse{
				host: {ID: otherID},
			}},
		},
		{
			name: "connect refused: retry later",
			d: &fakeDocker{networkOK: true, connectErr: http.StatusForbidden, inspect: map[string]dockercontainer.InspectResponse{
				host: gw(host, "bridge", nil),
			}},
			wantConnect: 1,
		},
		{
			name: "connect fails with 500: retry later",
			d: &fakeDocker{networkOK: true, connectErr: http.StatusInternalServerError, inspect: map[string]dockercontainer.InspectResponse{
				host: gw(host, "bridge", nil),
			}},
			wantConnect: 1,
		},
		{
			name: "already attached with alias: nothing to do",
			d: &fakeDocker{networkOK: true, inspect: map[string]dockercontainer.InspectResponse{
				host: gw(host, "bridge", map[string]*network.EndpointSettings{networkName: {Aliases: []string{host}}}),
			}},
			wantDone: true,
		},
		{
			name: "attached without alias: warn, never reconnect",
			d: &fakeDocker{networkOK: true, inspect: map[string]dockercontainer.InspectResponse{
				host: gw(host, "bridge", map[string]*network.EndpointSettings{networkName: {DNSNames: []string{"other"}}}),
			}},
			wantDone: true,
		},
		{
			name: "host network: warn, never connect",
			d: &fakeDocker{networkOK: true, inspect: map[string]dockercontainer.InspectResponse{
				host: gw(host, "host", nil),
			}},
			wantDone: true,
		},
		{
			name: "nil host config and network settings: connect",
			d: &fakeDocker{networkOK: true, inspect: map[string]dockercontainer.InspectResponse{
				host: {ID: selfID, Config: &dockercontainer.Config{Hostname: host}},
			}},
			wantDone:    true,
			wantConnect: 1,
		},
		{
			name: "garbage inspect JSON: retry later",
			d:    &fakeDocker{networkOK: true, rawInspect: `{"Id": [1,2,3], "Config": "nope"`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newFakeDockerManager(t, c.d)
			done, err := m.attachSelf(context.Background(), host)
			if done != c.wantDone {
				t.Fatalf("attachSelf() done = %v (err %v), want %v", done, err, c.wantDone)
			}
			if !done && err == nil {
				t.Error("attachSelf() not done without an error to log")
			}
			if n := c.d.connectCount(); n != c.wantConnect {
				t.Fatalf("NetworkConnect called %d times, want %d (calls %v)", n, c.wantConnect, c.d.calls)
			}
			if c.wantConnect > 0 {
				req := c.d.connects[0]
				if req.Container != selfID {
					t.Errorf("connected container %q, want own container %q", req.Container, selfID)
				}
				if req.EndpointConfig == nil || len(req.EndpointConfig.Aliases) != 1 || req.EndpointConfig.Aliases[0] != host {
					t.Errorf("endpoint config = %+v, want alias %q", req.EndpointConfig, host)
				}
				if req.EndpointConfig != nil && req.EndpointConfig.GwPriority != -1 {
					t.Errorf("GwPriority = %d, want -1 (praktor-net must not take the default route)", req.EndpointConfig.GwPriority)
				}
			}
		})
	}
}

// A hung Docker daemon must not hang the attach loop: each attempt is bounded.
func TestAttachSelfHungDaemon(t *testing.T) {
	d := &fakeDocker{hang: true}
	m := newFakeDockerManager(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	done, err := m.attachSelf(ctx, "e2e-praktor")
	if done || err == nil {
		t.Fatalf("attachSelf() = %v, %v; want not done with an error", done, err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("attachSelf() took %v with a hung daemon", el)
	}
	// The manager lock must be free again.
	if !m.mu.TryLock() {
		t.Fatal("manager lock still held after a failed attempt")
	}
	m.mu.Unlock()
}

// An unreachable daemon (socket gone) is an error to retry, not a panic.
func TestAttachSelfDaemonUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cli, err := client.New(client.WithHost("tcp://"+addr), client.WithAPIVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	m := &Manager{docker: cli, active: map[string]*ContainerInfo{}}
	done, err := m.attachSelf(context.Background(), "e2e-praktor")
	if done || err == nil {
		t.Fatalf("attachSelf() = %v, %v; want not done with an error", done, err)
	}
}
