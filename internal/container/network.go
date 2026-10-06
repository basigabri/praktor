package container

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/mtzanidakis/praktor/internal/natsbus"
)

const (
	attachSelfTimeout  = 10 * time.Second
	attachRetryInitial = 5 * time.Second
	attachRetryMax     = 10 * time.Minute
)

// dockerManagedFiles are the files Docker bind-mounts into every container
// from its own directory, <docker root>/containers/<64-hex id>/<file>.
var dockerManagedFiles = []string{"/etc/hostname", "/etc/hosts", "/etc/resolv.conf"}

var mountinfoContainerID = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

// attachAction is what attachSelf should do given the gateway's container.
type attachAction int

const (
	attachConnect     attachAction = iota // not on the agent network: connect
	attachDone                            // on the agent network under its hostname
	attachNoAlias                         // on the agent network, but the hostname doesn't resolve there
	attachNotPossible                     // host/container/none network mode: can't join a network
)

// decideAttach is the pure decision behind attachSelf.
func decideAttach(self dockercontainer.InspectResponse, host string) attachAction {
	if hc := self.HostConfig; hc != nil {
		if mode := hc.NetworkMode; mode.IsHost() || mode.IsContainer() || mode.IsNone() {
			return attachNotPossible
		}
	}
	if self.NetworkSettings != nil {
		if ep, ok := self.NetworkSettings.Networks[networkName]; ok {
			if resolvesAs(ep, host) {
				return attachDone
			}
			return attachNoAlias
		}
	}
	return attachConnect
}

// AttachToAgentNetwork connects the gateway's own container to the agent
// network when the gateway runs in Docker but outside that network. Run it in
// a goroutine at startup: it retries with backoff until it is done or ctx ends.
//
// Agents dial NATS at the gateway's hostname (natsbus.AgentNATSHost), which
// only resolves for containers sharing a network with the gateway. Compose
// attaches the gateway to praktor-net, but other deployments (a Home Assistant
// app, a plain `docker run`) start it on a different network, so every agent
// would fail to connect. Joining praktor-net with the hostname as a DNS alias
// keeps the advertised NATS URL valid without changing it.
//
// Failures are logged, never fatal: an unattached gateway behaves as it did
// before this existed.
func (m *Manager) AttachToAgentNetwork(ctx context.Context) {
	host := natsbus.AgentNATSHost()
	if host == "localhost" {
		return // not in Docker: agents reach NATS through the host
	}

	delay := attachRetryInitial
	for attempt := 1; ; attempt++ {
		done, err := m.attachSelf(ctx, host)
		if done {
			return
		}
		if attempt == 1 {
			slog.Warn("cannot attach gateway to agent network yet; retrying in the background",
				"network", networkName, "hostname", host, "error", err)
		} else {
			slog.Debug("attaching gateway to agent network failed", "attempt", attempt, "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, attachRetryMax)
	}
}

// attachSelf makes one attempt. It reports done when the gateway is attached
// or attaching doesn't apply, and an error worth retrying otherwise.
func (m *Manager) attachSelf(ctx context.Context, host string) (done bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, attachSelfTimeout)
	defer cancel()

	m.mu.Lock()
	err = m.ensureNetwork(ctx)
	m.mu.Unlock()
	if err != nil {
		return false, err
	}

	self, err := m.findSelf(ctx, host)
	if err != nil {
		return false, fmt.Errorf("find own container: %w", err)
	}

	switch decideAttach(self, host) {
	case attachDone:
		return true, nil
	case attachNotPossible:
		slog.Warn("gateway can't join the agent network in this network mode; agents may not reach NATS",
			"network_mode", string(self.HostConfig.NetworkMode), "hostname", host)
		return true, nil
	case attachNoAlias:
		// Not changed here: reconnecting would briefly cut the gateway off a
		// network someone else set up. This was already broken before.
		slog.Warn("gateway is on the agent network but its hostname doesn't resolve there; agents may not reach NATS",
			"network", networkName, "hostname", host)
		return true, nil
	}

	_, err = m.docker.NetworkConnect(ctx, networkName, client.NetworkConnectOptions{
		Container: self.ID,
		EndpointConfig: &network.EndpointSettings{
			Aliases: []string{host},
			// Below the default 0, so this network doesn't take over the
			// gateway's default route (Docker 28+; older daemons ignore it).
			GwPriority: -1,
		},
	})
	if err != nil {
		return false, fmt.Errorf("connect to %s: %w", networkName, err)
	}
	slog.Info("attached gateway to agent network", "network", networkName, "alias", host)
	return true, nil
}

// findSelf returns the container the gateway runs in. The container ID in
// /proc/self/mountinfo is authoritative. Inspecting by hostname is only a
// fallback, accepted when the container's configured hostname matches,
// because Docker also resolves the argument as another container's name or
// ID prefix.
func (m *Manager) findSelf(ctx context.Context, hostname string) (dockercontainer.InspectResponse, error) {
	var idErr error
	if id, err := selfContainerID(); err != nil {
		idErr = err
	} else {
		res, err := m.docker.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err == nil {
			return res.Container, nil
		}
		idErr = fmt.Errorf("inspect %s: %w", id, err)
	}

	res, err := m.docker.ContainerInspect(ctx, hostname, client.ContainerInspectOptions{})
	if err != nil {
		return dockercontainer.InspectResponse{}, errors.Join(idErr, fmt.Errorf("inspect %q: %w", hostname, err))
	}
	if res.Container.Config == nil || res.Container.Config.Hostname != hostname {
		return dockercontainer.InspectResponse{}, errors.Join(idErr, fmt.Errorf("container %q found by name has a different hostname", hostname))
	}
	return res.Container, nil
}

func selfContainerID() (string, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return containerIDFromMountinfo(f)
}

// containerIDFromMountinfo returns the Docker container ID from the mounts
// Docker makes for its managed files (/etc/hostname, /etc/hosts,
// /etc/resolv.conf). Other mounts are ignored: a bind mount of another
// container's directory (log shippers, backups) must not be mistaken for ours.
func containerIDFromMountinfo(r io.Reader) (string, error) {
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n') // no line length limit
		if id := mountinfoLineContainerID(line); id != "" {
			return id, nil
		}
		if errors.Is(err, io.EOF) {
			return "", errors.New("no container ID in mountinfo")
		}
		if err != nil {
			return "", fmt.Errorf("read mountinfo: %w", err)
		}
	}
}

// mountinfoLineContainerID returns the container ID if the line is the mount
// of a Docker-managed file from its container directory, else "".
func mountinfoLineContainerID(line string) string {
	// Fields: id parent major:minor root mountpoint ...
	f := strings.Fields(line)
	if len(f) < 5 || !slices.Contains(dockerManagedFiles, f[4]) {
		return ""
	}
	root, mountpoint := f[3], f[4]
	if path.Base(root) != path.Base(mountpoint) {
		return ""
	}
	if m := mountinfoContainerID.FindStringSubmatch(root); m != nil {
		return m[1]
	}
	return ""
}

// resolvesAs reports whether name is one of the endpoint's DNS names.
func resolvesAs(ep *network.EndpointSettings, name string) bool {
	if ep == nil {
		return false
	}
	return slices.Contains(ep.Aliases, name) || slices.Contains(ep.DNSNames, name)
}
