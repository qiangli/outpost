package main

import (
	"fmt"
	"strings"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/spf13/cobra"
)

// doctorCheck is one diagnostic line. status is ok (good) / warn (needs
// attention) / info (neutral fact).
type doctorCheck struct {
	name   string
	status string
	detail string
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose boot-readiness: daemon up, build, boot service registered + will-run, host awake",
		Long: `Boot-readiness diagnostic. Reports whether the daemon is up, the build, whether
the boot service/task is registered and will actually fire at boot, and (on
Windows) whether the host is kept awake. Cross-platform.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			runDoctor()
			return nil
		},
	}
}

func runDoctor() {
	var checks []doctorCheck

	if daemonRunning() {
		checks = append(checks, doctorCheck{"daemon", "ok", "admin endpoint " + daemonAdminAddr() + " responding"})
	} else {
		checks = append(checks, doctorCheck{"daemon", "warn", "not reachable on " + daemonAdminAddr() + " — run `outpost start` or install the boot service"})
	}

	bi := agent.ReadBuildInfo()
	build := bi.Short()
	if c := bi.ShortCommit(); c != "" && c != build {
		build += " (" + c + ")"
	}
	checks = append(checks, doctorCheck{"build", "info", build})

	// platform-specific boot-service / keep-awake checks
	checks = append(checks, serviceDoctor()...)

	printChecks(checks)
}

func printChecks(checks []doctorCheck) {
	mark := map[string]string{"ok": "✓", "warn": "!", "info": "·"}
	for _, c := range checks {
		m := mark[c.status]
		if m == "" {
			m = " "
		}
		fmt.Printf("%s %-13s %s\n", m, c.name, c.detail)
	}
}

// parseKV turns "KEY=value" lines (one per line) into a map. Tolerates blank
// lines and CRLF. Used to consume structured output from a platform probe.
func parseKV(s string) map[string]string {
	m := map[string]string{}
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if i := strings.IndexByte(ln, '='); i > 0 {
			m[ln[:i]] = strings.TrimSpace(ln[i+1:])
		}
	}
	return m
}
