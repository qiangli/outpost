package runtime

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// bashy_scratch removes the dynamic loader; bashy_dks_runtime enables the
// nft, mount and umount argv0 applets.
const runtimeBashyTags = "bashy_scratch,bashy_dks_runtime"

// Build in bashy's own module so its dependency and replace pins apply, rather
// than outpost's module graph. This is bashy c9c65d57, pinning yoke 61be2116.
const runtimeBashyModule = "github.com/qiangli/bashy@v1.0.0-rc.5-dev.0.20261016225530-c9c65d571ffb"

const pauseImage = "localhost/bashy/pause:" + binmgr.PauseVersion
const coreDNSImage = "localhost/bashy/coredns:bashy-v1.14.7-1"

// BinaryResolver returns a static Linux executable for the requested target.
// Implementations must verify third-party artifacts against signed releases and
// committed SHA-256 pins. Tests can substitute a hermetic resolver.
type BinaryResolver interface {
	Resolve(context.Context, string, string) (string, error)
}

// binmgr owns signed release verification and isolates foreign target caches.
type binmgrResolver struct{ sourceDir, workDir string }

func (r binmgrResolver) Resolve(ctx context.Context, name, arch string) (string, error) {
	if arch != "amd64" && arch != "arm64" {
		return "", fmt.Errorf("unsupported runtime architecture %q", arch)
	}
	if name == "bashy" {
		cmd := exec.CommandContext(ctx, "go", "mod", "download", "-json", runtimeBashyModule)
		cmd.Env = runtimeBuildEnv(arch)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("resolve cached runtime bashy source: %w: %s", err, out)
		}
		var module struct{ Dir string }
		if err := json.Unmarshal(out, &module); err != nil {
			return "", err
		}
		if module.Dir == "" {
			return "", fmt.Errorf("runtime bashy module has no source directory")
		}
		dest := filepath.Join(r.workDir, "bashy-"+arch)
		cmd = exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-tags="+runtimeBashyTags, "-trimpath", "-o", dest, "./cmd/bashy")
		cmd.Dir = module.Dir
		cmd.Env = runtimeBuildEnv(arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("build runtime bashy from %s: %w: %s", runtimeBashyModule, err, out)
		}
		if err := validateStaticLinux(dest, arch); err != nil {
			return "", err
		}
		info, err := buildinfo.ReadFile(dest)
		if err != nil {
			return "", err
		}
		return dest, validateRuntimeBashyTags(info)
	}
	if name == "outpost" || name == "outpost-cni" {
		pkg := "./cmd/outpost"
		if name == "outpost-cni" {
			pkg = "./internal/agent/runtime/image/cni"
		}
		dest := filepath.Join(r.workDir, name+"-"+arch)
		cmd := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-tags=fb_archives", "-trimpath", "-o", dest, pkg)
		cmd.Dir = r.sourceDir
		cmd.Env = runtimeBuildEnv(arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("cross-build %s from pinned source with cached dependencies: %w: %s", name, err, out)
		}
		return dest, validateStaticLinux(dest, arch)
	}
	tool, err := binmgr.DKSTool(name)
	if err != nil {
		return "", fmt.Errorf("%s: not yet published / not pinned: %w", name, err)
	}
	path, err := binmgr.EnsureFor(ctx, tool, "linux", arch)
	if err != nil {
		return "", fmt.Errorf("%s: signed pinned binary unavailable: %w", name, err)
	}
	return path, validateStaticLinux(path, arch)
}

func runtimeBuildEnv(arch string) []string {
	return append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOTOOLCHAIN=local")
}

func validateRuntimeBashyTags(info *debug.BuildInfo) error {
	tags := map[string]bool{}
	for _, setting := range info.Settings {
		if setting.Key == "-tags" {
			for _, tag := range strings.FieldsFunc(setting.Value, func(r rune) bool { return r == ',' || r == ' ' }) {
				tags[tag] = true
			}
		}
	}
	for _, tag := range strings.Split(runtimeBashyTags, ",") {
		if !tags[tag] {
			return fmt.Errorf("runtime bashy must be built with -tags=%s", runtimeBashyTags)
		}
	}
	return nil
}

func validateStaticLinux(path, arch string) error {
	f, err := elf.Open(path)
	if err != nil {
		return fmt.Errorf("%s is not a Linux ELF: %w", path, err)
	}
	defer f.Close()
	want := elf.EM_X86_64
	if arch == "arm64" {
		want = elf.EM_AARCH64
	}
	if f.Machine != want {
		return fmt.Errorf("%s: wrong architecture for linux/%s", path, arch)
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("%s: dynamic interpreter forbidden in scratch image", path)
		}
	}
	return nil
}

var runtimeBinaries = []struct{ name, dest string }{
	{"bashy", "bin/bashy"}, {"outpost", "usr/local/bin/outpost"},
	{"outpost-cni", "opt/cni/bin/outpost-cni"},
	{"k3s", "usr/local/bin/k3s"}, {"containerd-shim-runc-v2", "usr/local/bin/containerd-shim-runc-v2"},
	{"runc", "usr/local/bin/runc"}, {"metrics-server", "usr/local/bin/metrics-server"},
	{"tailscale", "usr/local/bin/tailscale"}, {"tailscaled", "usr/local/bin/tailscaled"},
	{"cni-bridge", "opt/cni/bin/bridge"}, {"cni-host-local", "opt/cni/bin/host-local"},
	{"cni-loopback", "opt/cni/bin/loopback"},
	{"cni-bandwidth", "opt/cni/bin/bandwidth"}, {"cni-firewall", "opt/cni/bin/firewall"},
}

func assembleImageContext(ctx context.Context, opts BuildOptions, arch string) (dir string, err error) {
	dir, err = os.MkdirTemp("", "outpost-runtime-build-")
	if err != nil {
		return "", err
	}
	cleanupDir := dir
	defer func() {
		if err != nil {
			_ = os.RemoveAll(cleanupDir)
		}
	}()
	root := filepath.Join(dir, "rootfs")
	work, err := os.MkdirTemp("", "outpost-runtime-binaries-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	resolver := opts.Resolver
	if resolver == nil {
		resolver = binmgrResolver{opts.SourceDir, work}
	}
	put := func(rel string, data []byte, mode os.FileMode) error {
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, mode)
	}
	for _, b := range runtimeBinaries {
		src, e := resolver.Resolve(ctx, b.name, arch)
		if e != nil {
			return dir, fmt.Errorf("build-runtime: %s: %w", b.name, e)
		}
		data, e := os.ReadFile(src)
		if e != nil {
			return dir, fmt.Errorf("build-runtime: %s unavailable: %w", b.name, e)
		}
		if e = put(b.dest, data, 0755); e != nil {
			return dir, e
		}
	}
	for _, img := range []struct{ name, tag string }{{"pause", pauseImage}, {"coredns", coreDNSImage}} {
		src, e := resolver.Resolve(ctx, img.name, arch)
		if e != nil {
			return dir, fmt.Errorf("build-runtime: %s: %w", img.name, e)
		}
		data, e := os.ReadFile(src)
		if e != nil {
			return dir, e
		}
		archive, e := binaryImageArchive(img.name, img.tag, arch, data)
		if e != nil {
			return dir, e
		}
		if e = put("var/lib/rancher/k3s/agent/images/"+img.name+".tar", archive, 0644); e != nil {
			return dir, e
		}
	}
	for _, script := range []string{"entrypoint.sh", "server-entrypoint.sh", "frpc-supervisor.sh", "metrics-server-supervisor.sh", "snapshotter.sh"} {
		data, e := imageFS.ReadFile("image/" + script)
		if e != nil {
			return dir, e
		}
		dest := script
		if script != "entrypoint.sh" && script != "server-entrypoint.sh" {
			dest = "usr/local/bin/" + script
		}
		if e = put(dest, data, 0755); e != nil {
			return dir, e
		}
	}
	data, err := imageFS.ReadFile("image/coredns.yaml")
	if err != nil {
		return dir, err
	}
	if err = put("usr/local/share/outpost/coredns.yaml", data, 0644); err != nil {
		return dir, err
	}
	for _, d := range []string{"tmp", "run", "usr/sbin", "etc/cni/net.d", "etc/rancher/k3s", "var/lib/rancher/k3s/server/manifests"} {
		if err = os.MkdirAll(filepath.Join(root, d), 0755); err != nil {
			return dir, err
		}
	}
	if err = os.Chmod(filepath.Join(root, "tmp"), 0777|os.ModeSticky); err != nil {
		return dir, err
	}
	// External Go programs exec these applets by name; shell builtins alone are
	// insufficient. Runtime-only applets require runtimeBashyTags above.
	for _, name := range strings.Fields("bash sh cat mkdir rm mv cp chmod chown sleep head tr sed awk hostname base64 grep tail timeout setpriv mount umount sysctl ip ipset conntrack ethtool nsenter mountpoint uname ln readlink stat") {
		if err = os.Symlink("/bin/bashy", filepath.Join(root, "bin", name)); err != nil {
			return dir, err
		}
	}
	if err = os.Symlink("/bin/bashy", filepath.Join(root, "usr/sbin/nft")); err != nil {
		return dir, err
	}
	for _, name := range []string{"containerd", "kubectl", "crictl", "ctr"} {
		if err = os.Symlink("k3s", filepath.Join(root, "usr/local/bin", name)); err != nil {
			return dir, err
		}
	}
	recipe := "FROM --platform=linux/" + arch + " scratch\nCOPY rootfs/ /\nENV PATH=/usr/local/bin:/usr/sbin:/bin:/opt/cni/bin\nENTRYPOINT [\"/entrypoint.sh\"]\n"
	err = os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(recipe), 0644)
	return dir, err
}

// binaryImageArchive produces a Docker-save archive accepted by containerd's
// image importer. Its sole layer contains the verified executable, no base.
func binaryImageArchive(name, tag, arch string, binary []byte) ([]byte, error) {
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(binary); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	diff := fmt.Sprintf("sha256:%x", sha256.Sum256(layer.Bytes()))
	config, err := json.Marshal(map[string]any{"architecture": arch, "os": "linux", "config": map[string]any{"Entrypoint": []string{"/" + name}}, "rootfs": map[string]any{"type": "layers", "diff_ids": []string{diff}}})
	if err != nil {
		return nil, err
	}
	configName := fmt.Sprintf("%x.json", sha256.Sum256(config))
	manifest, err := json.Marshal([]any{map[string]any{"Config": configName, "RepoTags": []string{tag}, "Layers": []string{"layer.tar"}}})
	if err != nil {
		return nil, err
	}
	var result bytes.Buffer
	tw = tar.NewWriter(&result)
	for _, f := range []struct {
		name string
		data []byte
	}{{configName, config}, {"manifest.json", manifest}, {"layer.tar", layer.Bytes()}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0644, Size: int64(len(f.data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, bytes.NewReader(f.data)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}
