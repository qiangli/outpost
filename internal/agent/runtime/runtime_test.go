package runtime

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
)

func TestWorkloadStorageVolumeNameIsStable(t *testing.T) {
	const agent = "dragon"
	got := runtimeVolumeName(agent, workloadStorageVolumeSuffix)
	if want := "outpost-dragon-k3s-storage"; got != want {
		t.Fatalf("runtimeVolumeName() = %q, want %q", got, want)
	}
	if again := runtimeVolumeName(agent, workloadStorageVolumeSuffix); again != got {
		t.Fatalf("runtimeVolumeName() changed between calls: %q then %q", got, again)
	}
}

func TestPersistentVolumeMountsIncludeOnlyK3sWorkloadStorage(t *testing.T) {
	mounts := persistentVolumeMounts("dragon")
	const want = "outpost-dragon-k3s-storage:/var/lib/rancher/k3s/storage"
	if !slices.Contains(mounts, want) {
		t.Fatalf("persistentVolumeMounts() = %#v, missing %q", mounts, want)
	}

	for _, mount := range mounts {
		if mount == "outpost-dragon-k3s-storage:/var/lib/rancher/k3s" {
			t.Fatalf("persistentVolumeMounts() persisted the whole k3s tree: %#v", mounts)
		}
	}
}

func TestPurgeVolumesPreservesWorkloadStorage(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("fake Podman executable uses a POSIX shell")
	}

	const agent = "dragon"
	tempDir := t.TempDir()
	commandLog := filepath.Join(tempDir, "commands.log")
	fakePodman := filepath.Join(tempDir, "podman")
	if err := os.WriteFile(fakePodman, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$OUTPOST_TEST_COMMAND_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_TEST_COMMAND_LOG", commandLog)

	if err := PurgeVolumes(context.Background(), Options{
		AgentName: agent,
		PodmanBin: fakePodman,
	}); err != nil {
		t.Fatalf("PurgeVolumes() error = %v", err)
	}
	logBytes, err := os.ReadFile(commandLog)
	if err != nil {
		t.Fatal(err)
	}
	commands := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	workloadStorage := runtimeVolumeName(agent, workloadStorageVolumeSuffix)
	for _, command := range commands {
		if strings.Contains(command, workloadStorage) {
			t.Fatalf("PurgeVolumes() deleted workload storage %q: %#v", workloadStorage, commands)
		}
	}

	for _, want := range []string{
		runtimeVolumeName(agent, tailscaleStateVolumeSuffix),
		runtimeVolumeName(agent, cniStateVolumeSuffix),
	} {
		wantCommand := "volume rm -f " + want
		if !slices.Contains(commands, wantCommand) {
			t.Errorf("PurgeVolumes() commands = %#v, missing %q", commands, wantCommand)
		}
	}
}

func TestUpReusesMatchingRunningRuntimeOnOrdinaryRestart(t *testing.T) {
	h := newRuntimeHarness(t)
	opts := h.options()
	h.existingFingerprint(opts)
	t.Setenv("OUTPOST_TEST_RUNNING", "true")

	if err := Up(context.Background(), opts); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	commands := h.commands()
	assertCommandContains(t, commands, "inspect --format")
	assertNoCommandStarts(t, commands, "stop ", "rm ", "run ", "start ")
}

func TestUpStartsMatchingStoppedRuntimeWithoutRecreation(t *testing.T) {
	h := newRuntimeHarness(t)
	opts := h.options()
	h.existingFingerprint(opts)
	t.Setenv("OUTPOST_TEST_RUNNING", "false")

	if err := Up(context.Background(), opts); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	commands := h.commands()
	assertCommandStarts(t, commands, "start dragon-runtime")
	assertNoCommandStarts(t, commands, "stop ", "rm ", "run ")
}

func TestUpRecreatesLegacyRuntimeAndMarksIPAMRecovery(t *testing.T) {
	h := newRuntimeHarness(t)
	opts := h.options()
	t.Setenv("OUTPOST_TEST_CONTAINER_EXISTS", "1")
	t.Setenv("OUTPOST_TEST_FINGERPRINT", "<no value>")
	t.Setenv("OUTPOST_TEST_RUNNING", "true")

	if err := Up(context.Background(), opts); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	commands := h.commands()
	assertCommandStarts(t, commands, "stop dragon-runtime")
	assertCommandStarts(t, commands, "rm -f dragon-runtime")
	run := commandStarting(t, commands, "run ")
	if !strings.Contains(run, "-e OUTPOST_RUNTIME_RECREATED=1") {
		t.Fatalf("runtime recreation marker absent from: %s", run)
	}
	assertPersistentMounts(t, run, opts.AgentName)
	assertNoCommandContains(t, commands, "volume rm")
}

func TestUpTreatsOrphanedCNIVolumeAsRuntimeRecreation(t *testing.T) {
	h := newRuntimeHarness(t)
	opts := h.options()
	t.Setenv("OUTPOST_TEST_CNI_VOLUME_EXISTS", "1")

	if err := Up(context.Background(), opts); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	run := commandStarting(t, h.commands(), "run ")
	if !strings.Contains(run, "-e OUTPOST_RUNTIME_RECREATED=1") {
		t.Fatalf("orphaned CNI volume did not trigger recovery: %s", run)
	}
	assertPersistentMounts(t, run, opts.AgentName)
}

func TestUpFirstBootDoesNotMarkIPAMRecovery(t *testing.T) {
	h := newRuntimeHarness(t)
	opts := h.options()

	if err := Up(context.Background(), opts); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	run := commandStarting(t, h.commands(), "run ")
	if strings.Contains(run, "OUTPOST_RUNTIME_RECREATED") {
		t.Fatalf("first boot incorrectly triggered recovery: %s", run)
	}
}

func TestUpPeerAPIBridgePublishesHostLoopbackOnly(t *testing.T) {
	h := newRuntimeHarness(t)
	opts := h.options()
	opts.APIBridgeHostPort = DefaultPeerAPIBridgePort

	if err := Up(context.Background(), opts); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	run := commandStarting(t, h.commands(), "run ")
	if want := "-p 127.0.0.1:16444:6443"; !strings.Contains(run, want) {
		t.Fatalf("peer API bridge missing loopback publication %q: %s", want, run)
	}
	if !strings.Contains(run, "-e OUTPOST_API_BIND_ADDR=0.0.0.0") {
		t.Fatalf("peer API visitor is not reachable from container NAT: %s", run)
	}
	for _, forbidden := range []string{"-p 0.0.0.0:16444:6443", "-p :16444:6443"} {
		if strings.Contains(run, forbidden) {
			t.Fatalf("peer API bridge escaped host loopback (%q): %s", forbidden, run)
		}
	}
}

func TestUpCloudPathDoesNotPublishAPIBridge(t *testing.T) {
	h := newRuntimeHarness(t)
	if err := Up(context.Background(), h.options()); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	run := commandStarting(t, h.commands(), "run ")
	if strings.Contains(run, "OUTPOST_API_BIND_ADDR") || strings.Contains(run, "127.0.0.1:16444") {
		t.Fatalf("cloud runtime unexpectedly published peer API bridge: %s", run)
	}
}

func TestUpForceRecreateMarksIPAMRecovery(t *testing.T) {
	h := newRuntimeHarness(t)
	opts := h.options()
	h.existingFingerprint(opts)
	t.Setenv("OUTPOST_TEST_RUNNING", "true")
	opts.ForceRecreate = true

	if err := Up(context.Background(), opts); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	run := commandStarting(t, h.commands(), "run ")
	if !strings.Contains(run, "-e OUTPOST_RUNTIME_RECREATED=1") {
		t.Fatalf("forced recreation did not trigger recovery: %s", run)
	}
}

type runtimeHarness struct {
	t          *testing.T
	commandLog string
	podman     string
}

func newRuntimeHarness(t *testing.T) *runtimeHarness {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("fake Podman executable uses a POSIX shell")
	}
	tempDir := t.TempDir()
	h := &runtimeHarness{
		t:          t,
		commandLog: filepath.Join(tempDir, "commands.log"),
		podman:     filepath.Join(tempDir, "podman"),
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$OUTPOST_TEST_COMMAND_LOG"
if [ "$1 $2" = "image inspect" ]; then
  printf '%s\n' 'sha256:test-runtime-image'
  exit 0
fi
if [ "$1" = "inspect" ]; then
  [ "${OUTPOST_TEST_CONTAINER_EXISTS:-0}" = "1" ] || exit 1
  printf '%s|%s\n' "${OUTPOST_TEST_FINGERPRINT:-<no value>}" "${OUTPOST_TEST_RUNNING:-false}"
  exit 0
fi
if [ "$1 $2" = "volume inspect" ]; then
  [ "${OUTPOST_TEST_CNI_VOLUME_EXISTS:-0}" = "1" ]
  exit $?
fi
if [ "$1" = "run" ]; then
  printf '%s\n' '0123456789abcdef'
fi
`
	if err := os.WriteFile(h.podman, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_TEST_COMMAND_LOG", h.commandLog)
	return h
}

func (h *runtimeHarness) options() Options {
	return Options{
		AgentName:          "dragon",
		HostName:           "dragonfly",
		Image:              "outpost-runtime:test",
		NodeToken:          "node-secret",
		APIServer:          "https://127.0.0.1:6443",
		CloudboxHost:       "cloudbox.test",
		CloudboxPort:       443,
		STCPSecret:         "stcp-secret",
		MatrixToken:        "matrix-secret",
		APIPort:            6443,
		KubeletPort:        10250,
		PodCIDR:            "10.42.1.0/24",
		OverlayLoginServer: "https://headscale.test",
		OverlayAuthKey:     "single-use-key",
		PodmanBin:          h.podman,
		ExtraEnv:           []string{"EXTRA=value"},
	}
}

func (h *runtimeHarness) existingFingerprint(opts Options) {
	h.t.Helper()
	fingerprint, err := runtimeFingerprint(context.Background(), h.podman, opts.Image, opts)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Setenv("OUTPOST_TEST_CONTAINER_EXISTS", "1")
	h.t.Setenv("OUTPOST_TEST_FINGERPRINT", fingerprint)
	if err := os.Remove(h.commandLog); err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
}

func (h *runtimeHarness) commands() []string {
	h.t.Helper()
	data, err := os.ReadFile(h.commandLog)
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func assertPersistentMounts(t *testing.T, run, agent string) {
	t.Helper()
	for _, mount := range persistentVolumeMounts(agent) {
		if !strings.Contains(run, "-v "+mount) {
			t.Errorf("runtime run is missing persistent mount %q: %s", mount, run)
		}
	}
}

func assertCommandStarts(t *testing.T, commands []string, prefix string) {
	t.Helper()
	_ = commandStarting(t, commands, prefix)
}

func commandStarting(t *testing.T, commands []string, prefix string) string {
	t.Helper()
	for _, command := range commands {
		if strings.HasPrefix(command, prefix) {
			return command
		}
	}
	t.Fatalf("commands %#v do not include prefix %q", commands, prefix)
	return ""
}

func assertCommandContains(t *testing.T, commands []string, substring string) {
	t.Helper()
	for _, command := range commands {
		if strings.Contains(command, substring) {
			return
		}
	}
	t.Fatalf("commands %#v do not include %q", commands, substring)
}

func assertNoCommandStarts(t *testing.T, commands []string, prefixes ...string) {
	t.Helper()
	for _, command := range commands {
		for _, prefix := range prefixes {
			if strings.HasPrefix(command, prefix) {
				t.Errorf("unexpected command %q with prefix %q", command, prefix)
			}
		}
	}
}

func assertNoCommandContains(t *testing.T, commands []string, substring string) {
	t.Helper()
	for _, command := range commands {
		if strings.Contains(command, substring) {
			t.Errorf("unexpected command %q containing %q", command, substring)
		}
	}
}
