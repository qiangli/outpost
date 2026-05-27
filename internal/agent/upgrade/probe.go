package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/qiangli/outpost/internal/agent"
)

// Probe execs `<path> version --json` and decodes the BuildInfo. This
// is the single check that distinguishes "an outpost binary at the
// expected commit" from "anything else on disk." Bad exit code, bad
// JSON, missing go_version, or a commit mismatch all fail closed.
//
// `expectedCommit` is the envelope's short commit; pass "" from the
// CLI (which doesn't pre-commit to a sha) to skip the commit check.
// The worker always passes the envelope's Commit so a man-in-the-
// middle that substitutes a same-named differently-built binary
// (different go.mod, different version of dependent packages, etc.)
// would still need to match the released sha to land.
func Probe(path, expectedCommit string) (agent.BuildInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version", "--json")
	out, err := cmd.Output()
	if err != nil {
		return agent.BuildInfo{}, fmt.Errorf("exec %s version --json: %w", path, err)
	}
	var b agent.BuildInfo
	if err := json.Unmarshal(out, &b); err != nil {
		return agent.BuildInfo{}, fmt.Errorf("parse version --json output: %w (got %d bytes)", err, len(out))
	}
	if b.GoVersion == "" {
		return agent.BuildInfo{}, errors.New("version --json output had no go_version field; not an outpost binary?")
	}
	// Normalize both sides to short commit. The envelope's Commit
	// field can legitimately arrive in two shapes — short ("6e498ea",
	// what the CLI surfaces and what BuildInfo.Short() returns) or
	// full 40-char sha (what the GH-Action release webhook sends,
	// via `github.sha`). Without this normalization, every cloudbox-
	// pushed upgrade silently failed at probe_failed because the
	// binary's BuildInfo.Commit is the full sha and shortCommit
	// stripped it to 7, never matching the 40-char envelope value.
	if expectedCommit != "" && shortCommit(b.Commit) != shortCommit(expectedCommit) {
		return b, envelopeMismatch(shortCommit(expectedCommit), shortCommit(b.Commit))
	}
	return b, nil
}
