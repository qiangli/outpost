package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/boundedlog"
)

const logSinkArg = "__outpost-log-sink"

// Detached children need an actual descriptor that survives the launcher.
// The sink drains to EOF independently, including final panics and native
// subprocess output. Opening the destination in the child as an append-only
// descriptor would let those writers bypass the size limit.
func detachedLogPipe(name string) (*os.File, error) {
	writer, err := detachedLog(name)
	if err != nil {
		return nil, err
	}
	if _, err = writer.Write(nil); err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer read.Close()
	start := func(fallback bool) (*exec.Cmd, error) {
		cmd := exec.Command(exe, logSinkArg, name)
		cmd.Stdin = read
		if fallback {
			if !detachWithoutBreakaway(cmd) {
				return nil, fmt.Errorf("fallback unavailable")
			}
		} else {
			detach(cmd)
		}
		return cmd, cmd.Start()
	}
	cmd, err := start(false)
	if err != nil {
		if retry, e := start(true); e == nil {
			cmd, err = retry, nil
		}
	}
	if err != nil {
		write.Close()
		return nil, err
	}
	// Reap in launchers that continue running. Detached launchers may exit
	// immediately; the OS then reaps the sink when the last writer closes.
	go func() { _ = cmd.Wait() }()
	return write, nil
}

func detachedLog(name string) (boundedlog.Writer, error) {
	if name != "outpost.log" && name != "supervisord.log" {
		return boundedlog.Writer{}, fmt.Errorf("invalid detached log name")
	}
	dir, err := conf.DefaultCacheDir()
	return boundedlog.Writer{Path: filepath.Join(dir, name)}, err
}

func runLogSink(name string) error {
	writer, err := detachedLog(name)
	if err != nil {
		return err
	}
	// A temporarily full filesystem must not break the daemon's stdout pipe.
	// Continue draining and try the next chunk when the destination recovers.
	var lastError error
	buf := make([]byte, 32<<10)
	for {
		n, readErr := os.Stdin.Read(buf)
		if n > 0 {
			if _, err := writer.Write(buf[:n]); err != nil {
				lastError = err
			}
		}
		if readErr == io.EOF {
			return lastError
		}
		if readErr != nil {
			return readErr
		}
	}
}
