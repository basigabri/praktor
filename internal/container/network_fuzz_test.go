package container

import (
	"bytes"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	dockercontainer "github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// mountinfoSeeds are realistic and hostile /proc/self/mountinfo inputs.
func mountinfoSeeds() []string {
	return []string{
		"",
		"\n",
		"garbage\n",
		"1300 1290 254:1 /docker/containers/" + selfID + "/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n",
		"512 500 179:8 /docker/containers/" + selfID + "/resolv.conf /etc/resolv.conf rw - ext4 /dev/mmcblk0p8 rw\n",
		"1300 1290 254:1 /docker/containers/" + selfID + "/hosts /etc/hosts rw - ext4 /dev/vda1 rw",
		// CRLF line endings
		"1300 1290 254:1 /docker/containers/" + selfID + "/hostname /etc/hostname rw - ext4 /dev/vda1 rw\r\n",
		// Another container's directory, mounted elsewhere or under the wrong name.
		"1295 1290 254:1 /docker/containers/" + otherID + "/hosts /mnt/other-hosts ro - ext4 /dev/vda1 rw\n",
		"1300 1290 254:1 /docker/containers/" + otherID + "/hosts /etc/hostname rw - ext4 /dev/vda1 rw\n",
		// IDs of the wrong length or case.
		"1300 1290 254:1 /docker/containers/" + selfID[:63] + "/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n",
		"1300 1290 254:1 /docker/containers/" + selfID + "a/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n",
		"1300 1290 254:1 /docker/containers/" + strings.ToUpper(selfID) + "/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n",
		// Path tricks.
		"1300 1290 254:1 /docker/containers/" + selfID + "/../hostname /etc/hostname rw - ext4 /dev/vda1 rw\n",
		"1300 1290 254:1 /docker/containers/" + selfID + "/hostname /etc/hostname/ rw - ext4 /dev/vda1 rw\n",
		"1300 1290 254:1 /docker/containers/" + selfID + "/hostname /etc//hostname rw - ext4 /dev/vda1 rw\n",
		"1300 1290 254:1 /docker/containers/" + selfID + "/host\\040name /etc/hostname rw - ext4 /dev/vda1 rw\n",
		"1300\t1290\t254:1\t/docker/containers/" + selfID + "/hostname\t/etc/hostname\trw\n",
		// Podman and containerd layouts must not match.
		"700 600 0:50 /containers/storage/overlay-containers/" + selfID + "/userdata/hostname /etc/hostname rw - tmpfs tmpfs rw\n",
		// Too few fields, NUL bytes, invalid UTF-8.
		"1 2 3 /docker/containers/" + selfID + "/hostname\n",
		"1300 1290 254:1 /docker/containers/" + selfID + "/hostname\x00 /etc/hostname rw\n",
		"\xff\xfe /etc/hostname \xff\n",
	}
}

// FuzzContainerIDFromMountinfo checks that parsing never panics and only
// returns a 64-hex ID taken from the mount of a Docker-managed file.
func FuzzContainerIDFromMountinfo(f *testing.F) {
	for _, s := range mountinfoSeeds() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		id, err := containerIDFromMountinfo(bytes.NewReader(data))
		if err != nil {
			if id != "" {
				t.Fatalf("returned id %q together with error %v", id, err)
			}
			return
		}
		if !hex64.MatchString(id) {
			t.Fatalf("returned %q, not a 64-hex container ID", id)
		}
		// The ID must come from a line that mounts a Docker-managed file
		// from that container's directory under the same file name.
		found := false
		for line := range strings.SplitSeq(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 5 || !slices.Contains(dockerManagedFiles, fields[4]) {
				continue
			}
			if strings.Contains(fields[3], "/containers/"+id+"/") && path.Base(fields[3]) == path.Base(fields[4]) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("id %q does not come from a Docker-managed mount in %q", id, data)
		}
	})
}

// FuzzMountinfoLine builds a line from its parts, so the fuzzer can explore
// roots and mount points directly.
func FuzzMountinfoLine(f *testing.F) {
	f.Add("/docker/containers/"+selfID+"/hostname", "/etc/hostname")
	f.Add("/docker/containers/"+selfID+"/hosts", "/etc/hosts")
	f.Add("/docker/containers/"+selfID+"/resolv.conf", "/etc/resolv.conf")
	f.Add("/docker/containers/"+selfID+"/hostname", "/mnt/hostname")
	f.Add("/docker/containers/"+otherID+"/hosts", "/etc/hostname")
	f.Add("/", "/")
	f.Fuzz(func(t *testing.T, root, mountpoint string) {
		line := "1300 1290 254:1 " + root + " " + mountpoint + " rw - ext4 /dev/vda1 rw"
		id := mountinfoLineContainerID(line)
		if id == "" {
			return
		}
		if !hex64.MatchString(id) {
			t.Fatalf("returned %q, not a 64-hex container ID", id)
		}
		fields := strings.Fields(line)
		if !slices.Contains(dockerManagedFiles, fields[4]) {
			t.Fatalf("matched non-managed mount point %q", fields[4])
		}
	})
}

// FuzzDecideAttach checks decideAttach against a direct statement of its
// rules, with arbitrary network modes, names and nil sub-structs.
func FuzzDecideAttach(f *testing.F) {
	f.Add("hassio", "local-praktor", false, "", "", false, false, false)
	f.Add("bridge", "praktor", true, "praktor", "", false, false, false)
	f.Add("praktor-net", "praktor", true, "", "praktor,abc", false, false, false)
	f.Add("host", "praktor", true, "praktor", "", false, false, false)
	f.Add("container:abc", "praktor", false, "", "", false, false, false)
	f.Add("none", "praktor", false, "", "", false, false, false)
	f.Add("default", "x", true, "y", "z", false, false, true)
	f.Add("", "x", true, "", "", true, true, false)
	f.Fuzz(func(t *testing.T, mode, host string, onNet bool, aliases, dnsNames string, nilHC, nilNS, nilEP bool) {
		var c dockercontainer.InspectResponse
		if !nilHC {
			c.HostConfig = &dockercontainer.HostConfig{NetworkMode: dockercontainer.NetworkMode(mode)}
		}
		var ep *network.EndpointSettings
		if !nilEP {
			ep = &network.EndpointSettings{Aliases: split(aliases), DNSNames: split(dnsNames)}
		}
		if !nilNS {
			c.NetworkSettings = &dockercontainer.NetworkSettings{Networks: map[string]*network.EndpointSettings{}}
			if onNet {
				c.NetworkSettings.Networks[networkName] = ep
			}
		}

		got := decideAttach(c, host)

		nm := dockercontainer.NetworkMode(mode)
		var want attachAction
		switch {
		case !nilHC && (nm.IsHost() || nm.IsContainer() || nm.IsNone()):
			want = attachNotPossible
		case nilNS || !onNet:
			want = attachConnect
		case !nilEP && (slices.Contains(ep.Aliases, host) || slices.Contains(ep.DNSNames, host)):
			want = attachDone
		default:
			want = attachNoAlias
		}
		if got != want {
			t.Fatalf("decideAttach(mode=%q nilHC=%v nilNS=%v onNet=%v nilEP=%v aliases=%q dns=%q, host=%q) = %v, want %v",
				mode, nilHC, nilNS, onNet, nilEP, aliases, dnsNames, host, got, want)
		}
	})
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
