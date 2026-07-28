package vknode

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const nativeExecHelperArg = "__outpost_native_exec"

type nativeLaunchDescriptor struct {
	Path       string   `json:"path"`
	Args       []string `json:"args,omitempty"`
	Dir        string   `json:"dir,omitempty"`
	LogPath    string   `json:"log_path"`
	StatusPath string   `json:"status_path"`
}

type nativeExitRecord struct {
	ExitCode   int32     `json:"exit_code"`
	FinishedAt time.Time `json:"finished_at"`
}

// RunNativeProcessHelper handles the private argv used by the durable
// vk-native launcher. Both outpost entry points call it before normal argument
// parsing. The helper waits for the workload and atomically writes its exit
// record, so a replacement provider can recover terminal status even when the
// daemon that submitted the process has already exited.
func RunNativeProcessHelper(args []string) (exitCode int, handled bool) {
	if len(args) != 3 || args[1] != nativeExecHelperArg {
		return 0, false
	}
	descPath := args[2]
	data, err := os.ReadFile(descPath)
	if err != nil {
		return 125, true
	}
	var desc nativeLaunchDescriptor
	if err := json.Unmarshal(data, &desc); err != nil {
		return 125, true
	}
	_ = os.Remove(descPath)

	logFile, err := os.OpenFile(desc.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 125, true
	}
	cmd := exec.Command(desc.Path, desc.Args...)
	cmd.Env = os.Environ()
	cmd.Dir = desc.Dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	runErr := cmd.Run()
	_ = logFile.Close()
	code := exitCodeFromWait(runErr, cmd.ProcessState)
	record := nativeExitRecord{ExitCode: code, FinishedAt: time.Now()}
	if err := writeNativeExitRecord(desc.StatusPath, record); err != nil {
		return 125, true
	}
	return int(code), true
}

func prepareNativeHelper(spec launchSpec) (helperPath, descriptorPath string, err error) {
	helperPath, err = os.Executable()
	if err != nil {
		return "", "", fmt.Errorf("locate native process helper: %w", err)
	}
	desc := nativeLaunchDescriptor{
		Path:       spec.Path,
		Args:       spec.Args,
		Dir:        spec.Dir,
		LogPath:    spec.LogPath,
		StatusPath: spec.StatusPath,
	}
	data, err := json.Marshal(desc)
	if err != nil {
		return "", "", err
	}
	descriptorPath = spec.StatusPath + ".launch"
	if err := os.WriteFile(descriptorPath, data, 0o600); err != nil {
		return "", "", fmt.Errorf("write native launch descriptor: %w", err)
	}
	return helperPath, descriptorPath, nil
}

func writeNativeExitRecord(path string, record nativeExitRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readNativeExitRecord(path string) (nativeExitRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nativeExitRecord{}, err
	}
	var record nativeExitRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nativeExitRecord{}, err
	}
	return record, nil
}

func nativeStatusPath(dataDir, uid string) string {
	return filepath.Join(dataDir, uid+".exit.json")
}
