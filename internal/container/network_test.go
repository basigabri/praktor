package container

import (
	"strings"
	"testing"

	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

const (
	selfID  = "3f4e1c2b9a8d7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f"
	otherID = "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f3f4e1c2b9a8d7e6f5a4b3c2d1e0f"
)

func TestContainerIDFromMountinfo(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" = expect an error
	}{
		{
			name: "docker /etc/hostname mount",
			in: "1290 1270 0:31 / / rw,relatime - overlay overlay rw\n" +
				"1300 1290 254:1 /docker/containers/" + selfID + "/hostname /etc/hostname rw,relatime - ext4 /dev/vda1 rw\n",
			want: selfID,
		},
		{
			name: "custom docker root (Home Assistant OS)",
			in:   "512 500 179:8 /docker/containers/" + selfID + "/resolv.conf /etc/resolv.conf rw - ext4 /dev/mmcblk0p8 rw\n",
			want: selfID,
		},
		{
			name: "another container's directory bind-mounted first is ignored",
			in: "1295 1290 254:1 /docker/containers/" + otherID + "/" + otherID + "-json.log /logs/other.log ro - ext4 /dev/vda1 rw\n" +
				"1296 1290 254:1 /docker/containers/" + otherID + "/hosts /mnt/other-hosts ro - ext4 /dev/vda1 rw\n" +
				"1300 1290 254:1 /docker/containers/" + selfID + "/hosts /etc/hosts rw - ext4 /dev/vda1 rw\n",
			want: selfID,
		},
		{
			name: "very long overlay line before the hostname mount",
			in: "1290 1270 0:31 / / rw - overlay overlay rw,lowerdir=" + strings.Repeat("/l/x:", 30000) + "\n" +
				"1300 1290 254:1 /docker/containers/" + selfID + "/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n",
			want: selfID,
		},
		{
			name: "very long line (over 1 MiB) before the hostname mount",
			in: "1290 1270 0:31 / / rw - overlay overlay rw,lowerdir=" + strings.Repeat("/l/x:", 300000) + "\n" +
				"1300 1290 254:1 /docker/containers/" + selfID + "/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n",
			want: selfID,
		},
		{
			name: "last line without trailing newline",
			in:   "1300 1290 254:1 /docker/containers/" + selfID + "/hostname /etc/hostname rw - ext4 /dev/vda1 rw",
			want: selfID,
		},
		{
			name: "file mounted under a different name is ignored",
			in:   "1300 1290 254:1 /docker/containers/" + otherID + "/hosts /etc/hostname rw - ext4 /dev/vda1 rw\n",
		},
		{name: "not a container", in: "22 1 259:2 / / rw,relatime shared:1 - ext4 /dev/nvme0n1p2 rw\n"},
		{name: "short id is ignored", in: "1300 1290 254:1 /docker/containers/abc123/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n"},
		{name: "malformed line", in: "garbage\n"},
		{name: "empty", in: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := containerIDFromMountinfo(strings.NewReader(c.in))
			if c.want == "" {
				if err == nil {
					t.Fatalf("containerIDFromMountinfo() = %q, want an error", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("containerIDFromMountinfo() = %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestDecideAttach(t *testing.T) {
	const host = "local-praktor"
	self := func(mode string, nets map[string]*network.EndpointSettings) dockercontainer.InspectResponse {
		return dockercontainer.InspectResponse{
			HostConfig:      &dockercontainer.HostConfig{NetworkMode: dockercontainer.NetworkMode(mode)},
			NetworkSettings: &dockercontainer.NetworkSettings{Networks: nets},
		}
	}

	cases := []struct {
		name string
		c    dockercontainer.InspectResponse
		want attachAction
	}{
		{"home assistant: only on hassio", self("hassio", map[string]*network.EndpointSettings{"hassio": {}}), attachConnect},
		{"plain docker run on the default bridge", self("bridge", map[string]*network.EndpointSettings{"bridge": {}}), attachConnect},
		{"no network settings", dockercontainer.InspectResponse{}, attachConnect},
		{"compose: hostname is a DNS name", self("praktor-net", map[string]*network.EndpointSettings{
			networkName: {DNSNames: []string{"praktor", host}},
		}), attachDone},
		{"already attached with the alias", self("hassio", map[string]*network.EndpointSettings{
			"hassio": {}, networkName: {Aliases: []string{host}},
		}), attachDone},
		{"attached without the hostname", self("hassio", map[string]*network.EndpointSettings{
			networkName: {DNSNames: []string{"addon_local_praktor"}},
		}), attachNoAlias},
		{"host network", self("host", nil), attachNotPossible},
		{"shares another container's network", self("container:abc", nil), attachNotPossible},
		{"no network", self("none", nil), attachNotPossible},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideAttach(c.c, host); got != c.want {
				t.Errorf("decideAttach() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestResolvesAs(t *testing.T) {
	cases := []struct {
		name string
		ep   *network.EndpointSettings
		want bool
	}{
		{"alias", &network.EndpointSettings{Aliases: []string{"local-praktor"}}, true},
		{"dns name", &network.EndpointSettings{DNSNames: []string{"praktor", "local-praktor"}}, true},
		{"other names only", &network.EndpointSettings{DNSNames: []string{"addon_local_praktor"}}, false},
		{"nil endpoint", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolvesAs(c.ep, "local-praktor"); got != c.want {
				t.Errorf("resolvesAs() = %v, want %v", got, c.want)
			}
		})
	}
}
