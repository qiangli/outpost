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
	"regexp"
	"runtime"
	"runtime/debug"
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
			for _, p := range strings.Fields(`bin/bashy bin/bash bin/sh bin/cat bin/mkdir bin/rm bin/mv bin/cp bin/chmod bin/chown bin/sleep bin/head bin/tr bin/sed bin/awk bin/hostname bin/base64 bin/grep bin/tail bin/timeout bin/mount bin/umount bin/sysctl usr/sbin/nft bin/ip bin/ipset bin/conntrack bin/ethtool bin/nsenter bin/mountpoint bin/uname bin/ln bin/readlink bin/stat
 etc/passwd etc/group entrypoint.sh server-entrypoint.sh
 usr/local/bin/outpost usr/local/bin/k3s usr/local/bin/containerd usr/local/bin/kubectl usr/local/bin/crictl usr/local/bin/ctr usr/local/bin/containerd-shim-runc-v2 usr/local/bin/runc usr/local/bin/tailscale usr/local/bin/tailscaled usr/local/bin/metrics-server usr/local/bin/frpc-supervisor.sh usr/local/bin/metrics-server-supervisor.sh usr/local/bin/snapshotter.sh
 opt/cni/bin/outpost-cni opt/cni/bin/bridge opt/cni/bin/host-local opt/cni/bin/loopback opt/cni/bin/bandwidth opt/cni/bin/firewall
 usr/local/share/outpost/coredns.yaml var/lib/rancher/k3s/agent/images/pause.tar var/lib/rancher/k3s/agent/images/coredns.tar`) {
				want = append(want, "rootfs/"+p)
			}
			sort.Strings(want)
			sort.Strings(got)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("context files:\ngot %v\nwant %v", got, want)
			}
			recipe, _ := os.ReadFile(filepath.Join(dir, "Dockerfile"))
			if string(recipe) != "FROM --platform=linux/"+arch+" scratch\nCOPY rootfs/ /\nENV PATH=/usr/local/bin:/usr/sbin:/bin:/opt/cni/bin\nENV HOME=/root XDG_CACHE_HOME=/root/.cache\nENTRYPOINT [\"/entrypoint.sh\"]\n" {
				t.Fatalf("unexpected build instructions: %s", recipe)
			}
			for name, record := range map[string]string{
				"etc/passwd": "nonroot:x:65532:65532:",
				"etc/group":  "nonroot:x:65532:",
			} {
				data, err := os.ReadFile(filepath.Join(dir, "rootfs", name))
				if err != nil || !strings.Contains(string(data), record) {
					t.Fatalf("missing metrics account in %s: %s (%v)", name, data, err)
				}
			}
			for _, p := range []string{"root", "root/.cache"} {
				info, err := os.Stat(filepath.Join(dir, "rootfs", p))
				if err != nil || !info.IsDir() {
					t.Fatalf("missing image directory %s: %v", p, err)
				}
			}
			for _, p := range []string{"bin/bash", "bin/sh", "usr/sbin/nft", "bin/mount", "bin/umount"} {
				target, err := os.Readlink(filepath.Join(dir, "rootfs", p))
				if err != nil || target != "/bin/bashy" {
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
	for _, name := range []string{"no-such-runtime-binary"} {
		_, err := r.Resolve(context.Background(), name, "arm64")
		if err == nil || !strings.Contains(err.Error(), "not yet published / not pinned") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Require explicit loopback names: unqualified names also default to Docker Hub.
func assertLocalImage(t *testing.T, source, ref string) {
	t.Helper()
	if !strings.HasPrefix(ref, "localhost/bashy/") {
		t.Errorf("%s: image %q must use localhost/bashy/", source, ref)
	}
}

var runtimeImageReference = regexp.MustCompile(`(?:--[a-z-]*image(?:=|\s+)|\bimage:\s*)["']?([^\s"'\\]+)`)

func assertLocalImageReferences(t *testing.T, source, content string) {
	t.Helper()
	for _, match := range runtimeImageReference.FindAllStringSubmatch(content, -1) {
		assertLocalImage(t, source, match[1])
	}
}

func TestRuntimeImagesAreLocal(t *testing.T) {
	assertLocalImage(t, "pause archive", pauseImage)
	assertLocalImage(t, "CoreDNS archive", coreDNSImage)
	err := fs.WalkDir(imageFS, "image", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !(strings.HasSuffix(path, ".sh") || strings.HasSuffix(path, ".yaml")) {
			return nil
		}
		content, err := imageFS.ReadFile(path)
		if err != nil {
			return err
		}
		assertLocalImageReferences(t, path, string(content))
		return nil
	})
	if err != nil {
		t.Fatal(err)
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
	assertLocalImageReferences(t, "server argv", string(out))
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
	assertLocalImageReferences(t, "agent argv", string(out))
	for _, arg := range []string{"agent", "--pause-image=" + pauseImage, "--kube-proxy-arg=proxy-mode=nftables", "--kubelet-arg=make-iptables-util-chains=false", "--snapshotter=overlayfs"} {
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

func TestRuntimeBashyTags(t *testing.T) {
	for _, tc := range []struct {
		tags  string
		valid bool
	}{
		{"", false}, {"fb_archives", false}, {"not_bashy_dks_runtime", false},
		{"bashy_dks_runtime", false}, {"bashy_scratch", false},
		{"bashy_scratch,bashy_dks_runtime", true}, {"fb_archives,bashy_dks_runtime,bashy_scratch", true},
	} {
		info := &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "-tags", Value: tc.tags}}}
		if err := validateRuntimeBashyTags(info); (err == nil) != tc.valid {
			t.Errorf("tags %q: %v", tc.tags, err)
		}
	}
}

func TestRuntimeConflists(t *testing.T) {
	script, err := imageFS.ReadFile("image/entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)cat > /etc/cni/net.d/[^ ]+ <<EOF\n(.*?)\nEOF`)
	matches := re.FindAllStringSubmatch(string(script), -1)
	if len(matches) != 2 {
		t.Fatalf("expected two conflists, got %d", len(matches))
	}
	for _, match := range matches {
		var conf struct{ Plugins []struct{ Type string } }
		if err := json.Unmarshal([]byte(match[1]), &conf); err != nil {
			t.Fatal(err)
		}
		if len(conf.Plugins) != 1 || (conf.Plugins[0].Type != "bridge" && conf.Plugins[0].Type != "outpost-cni") {
			t.Fatalf("unexpected CNI chain: %s", match[1])
		}
		if strings.Contains(match[1], "portmap") {
			t.Fatalf("iptables-only portmap: %s", match[1])
		}
	}
}

func TestRuntimeModuleWarning(t *testing.T) {
	script, _ := imageFS.ReadFile("image/entrypoint.sh")
	s := string(script)
	s = s[strings.Index(s, `MISSING_MODULES=""`):strings.Index(s, "# The CNI ledger")]
	for _, missing := range []string{"", "overlay", "br_netfilter overlay nf_tables vxlan"} {
		root := t.TempDir()
		for _, module := range strings.Fields("br_netfilter overlay nf_tables vxlan") {
			if !strings.Contains(missing, module) {
				if err := os.Mkdir(filepath.Join(root, module), 0755); err != nil {
					t.Fatal(err)
				}
			}
		}
		body := strings.ReplaceAll(s, "/sys/module/", root+"/")
		out, err := exec.Command("bash", "-eu", "-c", `log(){ printf '%s\n' "$*"; }; `+body).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		if missing == "" {
			if len(out) != 0 {
				t.Fatalf("unexpected warning: %s", out)
			}
		} else if !strings.Contains(string(out), "WARNING: host kernel modules missing from /sys/module: "+missing+";") {
			t.Fatalf("missing modules %q: %s", missing, out)
		}
	}
}

// This gate downloads all 28 signed DKS pins into a disposable cache and
// checks the runtime bashy applets (executes only on matching Linux hosts).
func TestDKSResolve(t *testing.T) {
	if os.Getenv("OUTPOST_DKS_RESOLVE") != "1" {
		t.Skip("set OUTPOST_DKS_RESOLVE=1 for signed release resolution")
	}
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	r := binmgrResolver{workDir: t.TempDir()}
	names := []string{"bashy", "k3s", "containerd-shim-runc-v2", "runc", "metrics-server", "tailscale", "tailscaled", "pause", "coredns", "cni-bridge", "cni-host-local", "cni-loopback", "cni-bandwidth", "cni-firewall", "cni-portmap"}
	for _, arch := range []string{"amd64", "arm64"} {
		for _, name := range names {
			t.Run(name+"/"+arch, func(t *testing.T) {
				path, err := r.Resolve(context.Background(), name, arch)
				if err != nil {
					t.Fatal(err)
				}
				if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
					t.Fatalf("missing binary %s: %v", path, err)
				}
				if err := validateStaticLinux(path, arch); err != nil {
					t.Fatal(err)
				}
				if name == "bashy" {
					checkAssembledBashy(t, path, arch)
				}
				t.Logf("%s/linux-%s: static ELF, no PT_INTERP", name, arch)
			})
		}
	}
}

// Only bashy is real here; unrelated payloads use the hermetic fixture.
type bashyContextResolver struct {
	bashy    string
	fallback *fakeBinaryResolver
}

func (r bashyContextResolver) Resolve(ctx context.Context, name, arch string) (string, error) {
	if name == "bashy" {
		return r.bashy, nil
	}
	return r.fallback.Resolve(ctx, name, arch)
}
func checkAssembledBashy(t *testing.T, path, arch string) {
	t.Helper()
	dir, err := assembleImageContext(context.Background(), BuildOptions{Resolver: bashyContextResolver{path, fixtureResolver(t)}}, arch)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	binary := filepath.Join(dir, "rootfs/bin/bashy")
	out, err := exec.Command("go", "version", "-m", binary).CombinedOutput()
	if err != nil {
		t.Fatalf("build info: %v: %s", err, out)
	}
	for _, dep := range []string{"github.com/qiangli/yoke\tv0.0.0-20261016224718-61be2116b8d9", "github.com/google/nftables", "-tags=" + runtimeBashyTags, "CGO_ENABLED=0"} {
		if !strings.Contains(string(out), dep) {
			t.Fatalf("missing %s in build info: %s", dep, out)
		}
	}
	symbols, err := exec.Command("go", "tool", "nm", binary).CombinedOutput()
	if err != nil {
		t.Fatalf("linked symbols: %v: %s", err, symbols)
	}
	for _, pkg := range []string{"github.com/qiangli/yoke/cmds/nft.", "github.com/qiangli/yoke/cmds/mount."} {
		if !strings.Contains(string(symbols), pkg) {
			t.Fatalf("missing linked applet %s", pkg)
		}
	}
	for _, applet := range []string{"usr/sbin/nft", "bin/mount", "bin/umount"} {
		target, err := os.Readlink(filepath.Join(dir, "rootfs", applet))
		if err != nil || target != "/bin/bashy" {
			t.Fatalf("%s -> %s: %v", applet, target, err)
		}
		if runtime.GOOS == "linux" && runtime.GOARCH == arch {
			cmd := exec.Command(binary, "--help")
			cmd.Args[0] = filepath.Base(applet)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("argv0 %s: %v: %s", applet, err, out)
			}
		}
	}
	t.Log("assembled nft/mount/umount argv0 links and linked applet dependencies verified")
}

func TestRuntimeScriptsUseK3sArgv0(t *testing.T) {
	forbidden := regexp.MustCompile(`\bk3s\s+(kubectl|crictl|ctr)\b`)
	err := fs.WalkDir(imageFS, "image", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".sh") {
			return nil
		}
		content, err := imageFS.ReadFile(path)
		if err != nil {
			return err
		}
		if match := forbidden.Find(content); match != nil {
			t.Errorf("%s: invoke the argv0 symlink instead of %q", path, match)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMetricsApplyRetryReportsLastError(t *testing.T) {
	script, err := imageFS.ReadFile("image/metrics-server-supervisor.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(script)
	start := strings.Index(s, "i=0\nuntil ")
	if start < 0 {
		t.Fatal("missing apply retry loop")
	}
	end := strings.Index(s[start:], "\ndone")
	if end < 0 {
		t.Fatal("missing retry loop end")
	}
	s = s[start : start+end+len("\ndone")]
	// Mock only the commands: exercise the real retry bound and diagnostic.
	prefix := `log(){ printf '%s\n' "$*"; }
sleep(){ :; }
internal_kubeconfig=test
work=test
kubectl(){ echo "apply attempt $((i + 1)): admission unavailable" >&2; return 1; }
`
	out, err := exec.Command("bash", "-eu", "-c", prefix+s).CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("expected fatal exit 1, got %v: %s", err, out)
	}
	if want := "FATAL: could not register metrics API: apply attempt 120: admission unavailable\n"; string(out) != want {
		t.Fatalf("unexpected retry diagnostic: %s", out)
	}
	prefix = strings.Replace(prefix, "return 1;", "return 0;", 1)
	out, err = exec.Command("bash", "-eu", "-c", prefix+s).CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("successful apply: %v: %s", err, out)
	}
}
