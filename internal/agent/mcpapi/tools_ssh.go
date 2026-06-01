package mcpapi

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// Phase-1 SSH tools: friendly-target CRUD plus one-shot exec.
//
// These tools back the `outpost ssh ...` subcommand tree (cmd/outpost/
// ssh_tree.go) AND surface the same capabilities to agentic MCP
// clients, so an LLM agent can drive a paired host with structured
// "add target → exec command → read output" calls — no /usr/bin/ssh
// negotiation, no ~/.ssh/config probing.
//
// Wave-2 additions (long-lived sessions, sftp, port-forwarding) live
// in this file when they land.

// sshTargetView is what list / add return to MCP callers. Mirrors
// conf.SSHTarget with explicit JSON tags so the schema is concrete.
type sshTargetView struct {
	Name        string `json:"name" jsonschema:"Friendly local alias used by outpost ssh subcommands"`
	Host        string `json:"host" jsonschema:"Destination: paired host (when via is empty) or hop-side address (when via is set)"`
	Port        int    `json:"port,omitempty" jsonschema:"SSH port on the hop destination (default 22; ignored when via is empty)"`
	User        string `json:"user,omitempty" jsonschema:"OS user on the remote host (resolves from /api/v1/ssh/hosts when empty at exec time)"`
	Via         string `json:"via,omitempty" jsonschema:"ProxyJump-style hop: alias of another configured target to dial through first"`
	Description string `json:"description,omitempty" jsonschema:"Freeform note"`
}

type listSSHTargetsOut struct {
	Targets []sshTargetView `json:"targets"`
}

type upsertSSHTargetIn struct {
	Name        string `json:"name" jsonschema:"Alias; letters, digits, -, _, ."`
	Host        string `json:"host" jsonschema:"Destination: paired host (when via is empty) or hop-side address (when via is set)"`
	Port        int    `json:"port,omitempty" jsonschema:"SSH port on the hop destination (default 22; ignored when via is empty)"`
	User        string `json:"user,omitempty" jsonschema:"OS user on the remote host"`
	Via         string `json:"via,omitempty" jsonschema:"ProxyJump-style hop alias"`
	Description string `json:"description,omitempty"`
}

type upsertSSHTargetOut struct {
	OK     bool          `json:"ok"`
	Target sshTargetView `json:"target"`
}

type sshExecIn struct {
	Name           string `json:"name" jsonschema:"Configured target alias"`
	Command        string `json:"command" jsonschema:"Literal command line passed to the remote shell"`
	Jump           string `json:"jump,omitempty" jsonschema:"ProxyJump alias that overrides the target's persisted via for this call"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"Cap on remote wall-clock runtime; default 60, max 600"`
	MaxStdout      int64  `json:"max_stdout,omitempty" jsonschema:"Cap on captured stdout (bytes); default 1 MiB"`
	MaxStderr      int64  `json:"max_stderr,omitempty" jsonschema:"Cap on captured stderr (bytes); default 256 KiB"`
}

type sshExecOut struct {
	Stdout          string `json:"stdout"`
	Stderr          string `json:"stderr"`
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
}

func (s *Server) registerSSHTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_list_ssh_targets",
		Description: "List configured SSH target aliases. Each entry maps a friendly name (used by outpost ssh / outpost_ssh_exec) to a cloudbox-paired host plus optional OS-user override.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, listSSHTargetsOut, error) {
		ts, err := s.core.ListSSHTargets()
		if err != nil {
			return apiErrResult[listSSHTargetsOut](err)
		}
		views := make([]sshTargetView, 0, len(ts))
		for _, t := range ts {
			views = append(views, sshTargetView{
				Name:        t.Name,
				Host:        t.Host,
				Port:        t.Port,
				User:        t.User,
				Via:         t.Via,
				Description: t.Description,
			})
		}
		return nil, listSSHTargetsOut{Targets: views}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_add_ssh_target",
		Description: "Register a friendly alias for a cloudbox-paired host. Idempotent — re-calling with the same name overwrites. The cached target is what outpost_ssh_exec reads. User is optional at add time; pass it explicitly when the remote OS user differs from what cloudbox reports via /api/v1/ssh/hosts.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in upsertSSHTargetIn) (*mcp.CallToolResult, upsertSSHTargetOut, error) {
		t, err := s.core.UpsertSSHTarget(admincore.SSHTargetView{
			Name:        in.Name,
			Host:        in.Host,
			Port:        in.Port,
			User:        in.User,
			Via:         in.Via,
			Description: in.Description,
		})
		if err != nil {
			return apiErrResult[upsertSSHTargetOut](err)
		}
		return nil, upsertSSHTargetOut{
			OK: true,
			Target: sshTargetView{
				Name:        t.Name,
				Host:        t.Host,
				Port:        t.Port,
				User:        t.User,
				Via:         t.Via,
				Description: t.Description,
			},
		}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_remove_ssh_target",
		Description: "Delete an SSH target alias. Idempotent — no error when the alias doesn't exist.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in byNameIn) (*mcp.CallToolResult, okOut, error) {
		if err := s.core.DeleteSSHTarget(in.Name); err != nil {
			return apiErrResult[okOut](err)
		}
		return nil, okOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_ssh_exec",
		Description: "Run a one-shot command on the named target's remote host via the in-process SSH client (no /usr/bin/ssh required). Returns structured stdout/stderr + exit code. Requires a current elevation cookie for the underlying host — run `outpost connect <host>` first; tool returns IsError=true with HTTP 401 guidance otherwise.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sshExecIn) (*mcp.CallToolResult, sshExecOut, error) {
		var timeout time.Duration
		if in.TimeoutSeconds > 0 {
			timeout = time.Duration(in.TimeoutSeconds) * time.Second
		}
		res, err := s.core.ExecSSH(ctx, admincore.ExecSSHParams{
			Name:         in.Name,
			Command:      in.Command,
			JumpOverride: in.Jump,
			Timeout:      timeout,
			MaxStdout:    in.MaxStdout,
			MaxStderr:    in.MaxStderr,
		})
		if err != nil {
			return apiErrResult[sshExecOut](err)
		}
		return nil, sshExecOut{
			Stdout:          string(res.Stdout),
			Stderr:          string(res.Stderr),
			ExitCode:        res.ExitCode,
			StdoutTruncated: res.StdoutTruncated,
			StderrTruncated: res.StderrTruncated,
		}, nil
	})
}
