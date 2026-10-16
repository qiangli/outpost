package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type fakeBinaryResolver struct {
	path, missing string
	calls         []string
}

func (r *fakeBinaryResolver) Resolve(_ context.Context, name, arch string) (string, error) {
	r.calls = append(r.calls, name+"/"+arch)
	if name == r.missing {
		return "", errors.New("not yet published / not pinned")
	}
	return r.path, nil
}
func fixtureResolver(t *testing.T) *fakeBinaryResolver {
	t.Helper()
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte("verified test executable"), 0755); err != nil {
		t.Fatal(err)
	}
	return &fakeBinaryResolver{path: path}
}
func TestScratchContext(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			r := fixtureResolver(t)
			dir, err := assembleImageContext(context.Background(), BuildOptions{Resolver: r}, arch)
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			var got []string
			err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() {
					rel, _ := filepath.Rel(dir, path)
					got = append(got, filepath.ToSlash(rel))
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"Dockerfile"}
			for _, p := range strings.Fields(`bin/bashy bin/bash bin/sh bin/cat bin/mkdir bin/rm bin/mv bin/cp bin/chmod bin/chown bin/sleep bin/head bin/tr bin/sed bin/awk bin/hostname bin/base64 bin/grep bin/tail bin/timeout bin/setpriv bin/mount bin/umount bin/modprobe bin/sysctl bin/nft bin/ip bin/ipset bin/conntrack bin/ethtool bin/nsenter bin/mountpoint bin/uname bin/ln bin/readlink bin/stat bin/iptables bin/ip6tables bin/iptables-save bin/iptables-restore bin/ip6tables-save bin/ip6tables-restore
 entrypoint.sh server-entrypoint.sh
 usr/local/bin/outpost usr/local/bin/k3s usr/local/bin/containerd usr/local/bin/kubectl usr/local/bin/crictl usr/local/bin/ctr usr/local/bin/containerd-shim-runc-v2 usr/local/bin/runc usr/local/bin/tailscale usr/local/bin/tailscaled usr/local/bin/metrics-server usr/local/bin/frpc-supervisor.sh usr/local/bin/metrics-server-supervisor.sh usr/local/bin/snapshotter.sh
 opt/cni/bin/outpost-cni opt/cni/bin/bridge opt/cni/bin/host-local opt/cni/bin/loopback opt/cni/bin/portmap opt/cni/bin/bandwidth opt/cni/bin/firewall
 usr/local/share/outpost/coredns.yaml var/lib/rancher/k3s/agent/images/pause.tar var/lib/rancher/k3s/agent/images/coredns.tar`) {
				want = append(want, "rootfs/"+p)
			}
			sort.Strings(want)
			sort.Strings(got)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("context files:\ngot %v\nwant %v", got, want)
			}
			recipe, _ := os.ReadFile(filepath.Join(dir, "Dockerfile"))
			if string(recipe) != "FROM --platform=linux/"+arch+" scratch\nCOPY rootfs/ /\nENV PATH=/usr/local/bin:/bin:/opt/cni/bin\nENTRYPOINT [\"/entrypoint.sh\"]\n" {
				t.Fatalf("unexpected build instructions: %s", recipe)
			}
			for _, p := range []string{"bin/bash", "bin/sh"} {
				target, err := os.Readlink(filepath.Join(dir, "rootfs", p))
				if err != nil || target != "bashy" {
					t.Fatalf("%s -> %s: %v", p, target, err)
				}
			}
			for _, c := range r.calls {
				if !strings.HasSuffix(c, "/"+arch) {
					t.Fatalf("wrong target: %s", c)
				}
			}
			for name, tag := range map[string]string{"pause": pauseImage, "coredns": coreDNSImage} {
				archive, err := os.ReadFile(filepath.Join(dir, "rootfs/var/lib/rancher/k3s/agent/images", name+".tar"))
				if err != nil {
					t.Fatal(err)
				}
				files := readTar(t, archive)
				var manifest []struct {
					Config           string
					RepoTags, Layers []string
				}
				if err = json.Unmarshal(files["manifest.json"], &manifest); err != nil {
					t.Fatal(err)
				}
				if len(manifest) != 1 || !reflect.DeepEqual(manifest[0].RepoTags, []string{tag}) {
					t.Fatalf("manifest %s", files["manifest.json"])
				}
				var config struct {
					Architecture, OS string
					Config           struct{ Entrypoint []string }
				}
				if err = json.Unmarshal(files[manifest[0].Config], &config); err != nil {
					t.Fatal(err)
				}
				if config.Architecture != arch || config.OS != "linux" || !reflect.DeepEqual(config.Config.Entrypoint, []string{"/" + name}) {
					t.Fatalf("config: %+v", config)
				}
				layer := readTar(t, files[manifest[0].Layers[0]])
				if len(layer) != 1 || string(layer[name]) != "verified test executable" {
					t.Fatalf("layer: %v", layer)
				}
			}
		})
	}
}
func readTar(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name], err = io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
	}
}
func TestScratchMissingBinaryFailsBeforeEngine(t *testing.T) {
	names := []string{"pause", "coredns"}
	for _, b := range runtimeBinaries {
		names = append(names, b.name)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			r := fixtureResolver(t)
			r.missing = name
			_, err := BuildImage(context.Background(), BuildOptions{Resolver: r, PodmanBin: "must-never-be-invoked", TargetArch: "amd64"})
			if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "not yet published / not pinned") {
				t.Fatalf("got %v", err)
			}
		})
	}
}
func TestUnpublishedBinmgrFailsClosed(t *testing.T) {
	r := binmgrResolver{}
	for _, name := range []string{"bashy", "no-such-runtime-binary"} {
		_, err := r.Resolve(context.Background(), name, "arm64")
		if err == nil || !strings.Contains(err.Error(), "not yet published / not pinned") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
func TestRuntimeK3sArgv(t *testing.T) {
	server, _ := imageFS.ReadFile("image/server-entrypoint.sh")
	s := string(server)
	s = s[strings.Index(s, "set -- server"):strings.Index(s, "log \"exec k3s")]
	s = "OUTPOST_API_PORT=6443; OUTPOST_CLUSTER_DNS=10.43.0.10; ADVERTISE_ADDR=192.0.2.1\n" + s + "\nprintf '%s\\n' \"$@\"\n"
	out, err := exec.Command("bash", "-eu", "-c", s).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, arg := range []string{"server", "--cluster-init", "--disable-helm-controller", "--disable=coredns,servicelb,traefik,local-storage,metrics-server", "--disable-network-policy"} {
		if !strings.Contains("\n"+string(out), "\n"+arg+"\n") {
			t.Errorf("server missing %s: %s", arg, out)
		}
	}
	agent, _ := imageFS.ReadFile("image/entrypoint.sh")
	s = string(agent)
	s = s[strings.Index(s, "exec /usr/local/bin/k3s agent"):]
	s = strings.Replace(s, "exec /usr/local/bin/k3s", "printf '%s\\n'", 1)
	s = "OUTPOST_API_PORT=6443; OUTPOST_NODE_TOKEN=test; OUTPOST_AGENT_NAME=test; OUTPOST_HOST_NAME=test; K3S_RESOLV=/run/test; SNAPSHOTTER_ARGS=--snapshotter=overlayfs; KUBELET_ROUTING_ARGS=; K3S_FLANNEL_ARGS=\n" + s
	out, err = exec.Command("bash", "-eu", "-c", s).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, arg := range []string{"agent", "--pause-image=" + pauseImage, "--kube-proxy-arg=proxy-mode=nftables", "--snapshotter=overlayfs"} {
		if !strings.Contains("\n"+string(out), "\n"+arg+"\n") {
			t.Errorf("agent missing %s: %s", arg, out)
		}
	}
	manifest, _ := imageFS.ReadFile("image/coredns.yaml")
	if !strings.Contains(string(manifest), "image: "+coreDNSImage) || !strings.Contains(string(manifest), "imagePullPolicy: Never") {
		t.Fatal("CoreDNS must use the imported image without pulls")
	}
}
func TestSnapshotterFallback(t *testing.T) {
	script, _ := imageFS.ReadFile("image/snapshotter.sh")
	for _, tc := range []struct{ mount, want string }{{"0", "overlayfs"}, {"1", "native"}} {
		s := string(script) + "\nmkdir(){ :; }; rm(){ :; }; mount(){ return " + tc.mount + "; }; umount(){ :; }; SNAPSHOTTER=; select_snapshotter; printf '%s' \"$SNAPSHOTTER\""
		out, err := exec.Command("bash", "-eu", "-c", s).CombinedOutput()
		if err != nil || string(out) != tc.want {
			t.Fatalf("mount=%s: %s, %v", tc.mount, out, err)
		}
	}
}
