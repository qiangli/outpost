// Package repair provides the POST /admin/repair receiver — the cloudbox-driven
// trigger that starts a CI-failure self-fix on this host.
//
// Auth model mirrors /admin/warm and /admin/upgrade: no bearer at the HTTP
// layer. The route lives on the daemon's matrix-tunnel-fronted HTTP server,
// which binds 127.0.0.1 only, so cloudbox (through the tunnel) is the only
// reachable caller, and it is mounted solely on paired hosts.
//
// The receiver does not diagnose or edit code. It spawns a configured repair
// program (typically the band-escalating CI-repair router) detached, threading
// the collector issue / task id / repo / failing OS through both argv
// (`--issue N`) and OUTPOST_REPAIR_* env, then returns 202 — progress lands on
// the collector issue and the task-event feed, not this response. This is the
// PUSH half of the notify path; the outpost `schedule` cron running the same
// program periodically is the PULL backstop, so an offline/missed push still
// gets picked up (see the umbrella ci-failure-autofix-pipeline doc).
package repair

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"sync"

	"github.com/gin-gonic/gin"
)

// Config configures the receiver.
type Config struct {
	// Command is argv for the repair program. Empty DISABLES the route (the
	// handler replies 503); the pull backstop still applies. Example:
	//   []string{"bash", "/opt/dhnt/bashy/scripts/ci-failure-router.sh", "--once"}
	Command []string

	// MaxConcurrent caps simultaneous repair spawns on this host (0 → 1). A
	// host over the cap replies 503; the pull backstop retries later.
	MaxConcurrent int
}

// Executor spawns repair programs and enforces the concurrency cap.
type Executor struct {
	cfg  Config
	mu   sync.Mutex
	live int
}

// New builds an Executor. A nil/empty-Command Executor is valid — the route
// mounts but replies 503 until a repair command is configured.
func New(cfg Config) *Executor {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	return &Executor{cfg: cfg}
}

// RepairRequest is the body cloudbox pushes (from the CI-failure webhook/task).
// Mirrors the collector client_payload.
type RepairRequest struct {
	CollectorIssue string `json:"collector_issue"`
	SourceRepo     string `json:"source_repo"`
	TaskID         string `json:"task_id"`
	OS             string `json:"os,omitempty"`
	Arch           string `json:"arch,omitempty"`
}

// MountRoute attaches POST /admin/repair on rg. No-op if e is nil.
func MountRoute(rg *gin.RouterGroup, e *Executor) {
	if e == nil {
		return
	}
	rg.POST("/admin/repair", repairHandler(e))
}

func repairHandler(e *Executor) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req RepairRequest
		if err := c.BindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "bad request: " + err.Error()})
			return
		}
		if req.CollectorIssue == "" && req.TaskID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "collector_issue or task_id is required"})
			return
		}
		if err := e.Spawn(req); err != nil {
			var ae *apiError
			if errors.As(err, &ae) {
				c.JSON(ae.status, gin.H{"error": ae.msg})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusAccepted, gin.H{
			"status":          "started",
			"collector_issue": req.CollectorIssue,
			"task_id":         req.TaskID,
		})
	}
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

// Spawn starts the configured repair command detached, with the request threaded
// through argv (`--issue N`) and OUTPOST_REPAIR_* env. It returns an
// apiError(503) if no command is configured or the concurrency cap is hit — in
// both cases the pull backstop still applies, so the failure is never lost.
func (e *Executor) Spawn(req RepairRequest) error {
	if len(e.cfg.Command) == 0 {
		return &apiError{http.StatusServiceUnavailable, "repair command not configured on this host"}
	}

	e.mu.Lock()
	if e.live >= e.cfg.MaxConcurrent {
		e.mu.Unlock()
		return &apiError{http.StatusServiceUnavailable, "repair host at capacity; the pull backstop will pick this up"}
	}
	e.live++
	e.mu.Unlock()

	args := append([]string{}, e.cfg.Command[1:]...)
	if req.CollectorIssue != "" {
		args = append(args, "--issue", req.CollectorIssue)
	}
	cmd := exec.Command(e.cfg.Command[0], args...)
	cmd.Env = append(os.Environ(),
		"OUTPOST_REPAIR_ISSUE="+req.CollectorIssue,
		"OUTPOST_REPAIR_REPO="+req.SourceRepo,
		"OUTPOST_REPAIR_TASK="+req.TaskID,
		"OUTPOST_REPAIR_OS="+req.OS,
		"OUTPOST_REPAIR_ARCH="+req.Arch,
	)

	if err := cmd.Start(); err != nil {
		e.mu.Lock()
		e.live--
		e.mu.Unlock()
		return err
	}
	// Reap the child and free the slot when it exits; the HTTP caller already
	// got its 202.
	go func() {
		_ = cmd.Wait()
		e.mu.Lock()
		e.live--
		e.mu.Unlock()
	}()
	return nil
}
