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
