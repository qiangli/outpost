// Package sshclient is Outpost's importable in-process SSH client.
//
// It exposes an SSH connection layered over any net.Conn, including an
// Outpost LAN listener. It needs no system ssh binary, password prompt, or TTY.
package sshclient

import internal "github.com/qiangli/outpost/internal/agent/sshclient"

type Client = internal.Client
type Config = internal.Config
type ExecOptions = internal.ExecOptions
type ExecResult = internal.ExecResult
type ShellOptions = internal.ShellOptions

var Dial = internal.Dial

// Exec, SFTP, DirectTCPIP, and LocalForward are methods on Client.
