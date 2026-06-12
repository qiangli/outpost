// Embedded coreutils fallback for the in-process shell.
//
// The matrix shell / `outpost sshd` exec surface runs on whatever the
// host OS provides — which on Windows means `ls`, `cat`, `head`,
// `whoami`, … simply don't exist and every agentic caller gets 127.
// CoreutilsExec closes that gap: commands missing from PATH are
// resolved against the pure-Go tool registry in
// github.com/qiangli/coreutils (the sibling library `outpost git`
// already embeds), so the shell offers one identical core toolset on
// every platform.
//
// Precedence is deliberate: a real executable on PATH always wins.
// On unix hosts with a full userland this middleware is a no-op, so
// existing behavior is unchanged; the fallback only fires where the
// platform has nothing to offer.
package shell

import (
	"context"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"

	_ "github.com/qiangli/coreutils/cmds/all" // register the full tool inventory
	"github.com/qiangli/coreutils/tool"
)

// CoreutilsExec is an interp.ExecHandlers middleware: when the command
// name is not resolvable on PATH but is implemented by the embedded
// coreutils registry, run the embedded implementation in-process.
// Everything else (PATH hits, unknown names, path-qualified invocations
// like ./foo) falls through to the next handler unchanged.
func CoreutilsExec(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if len(args) == 0 {
			return next(ctx, args)
		}
		// Registry names never contain separators, so path-qualified
		// invocations can't match and keep their normal semantics.
		t := tool.Lookup(args[0])
		if t == nil {
			return next(ctx, args)
		}
		hc := interp.HandlerCtx(ctx)
		if _, err := interp.LookPathDir(hc.Dir, hc.Env, args[0]); err == nil {
			// A real binary exists — the platform userland wins.
			return next(ctx, args)
		}
		rc := &tool.RunContext{
			Ctx: ctx,
			Dir: hc.Dir,
			Env: environList(hc.Env),
			Stdio: tool.Stdio{
				In:  hc.Stdin,
				Out: hc.Stdout,
				Err: hc.Stderr,
			},
		}
		code := t.Run(rc, args[1:])
		if code == 0 {
			return nil
		}
		if code < 0 || code > 255 {
			code = 1
		}
		return interp.ExitStatus(code)
	}
}

// environList flattens an expand.Environ into the os.Environ() shape
// tool.RunContext consumes.
func environList(env expand.Environ) []string {
	var out []string
	env.Each(func(name string, vr expand.Variable) bool {
		out = append(out, name+"="+vr.String())
		return true
	})
	return out
}
