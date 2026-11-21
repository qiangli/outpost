package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/recovery"
	"github.com/spf13/cobra"
)

// doctorCheck is one diagnostic line. status is ok (good) / warn (needs
// attention) / info (neutral fact).
type doctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

func doctorCmd() *cobra.Command {
	var asJSON, alwaysOn bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose boot-readiness: daemon up, build, boot service registered + will-run, host awake",
		Long: `Boot-readiness diagnostic. Reports whether the daemon is up, the build, whether
the boot service/task is registered and will actually fire at boot, and whether sleep is disabled on every platform. Readback failures never
report disabled sleep or a boot-enabled service.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.OutOrStdout(), asJSON, alwaysOn)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit checks and local recovery events as JSON")
	cmd.Flags().BoolVar(&alwaysOn, "always-on", false, "check this host against an always-on sleep policy")
	return cmd
}

func runDoctor(out io.Writer, asJSON, alwaysOn bool) error {
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
	checks = append(checks, serviceDoctor(alwaysOn)...)

	if asJSON {
		return writeDoctorJSON(out, checks)
	}
	printChecks(out, checks)
	return nil
}

func writeDoctorJSON(out io.Writer, checks []doctorCheck) error {
	result := struct {
		Schema        string           `json:"schema"`
		Checks        []doctorCheck    `json:"checks"`
		Recovery      []recovery.Event `json:"recovery"`
		RecoveryError string           `json:"recovery_error,omitempty"`
	}{Schema: "outpost-doctor-v1", Checks: checks}
	events, err := recovery.Read()
	if err != nil {
		result.RecoveryError = "local recovery journal could not be read"
	}
	if events == nil {
		events = []recovery.Event{}
	}
	result.Recovery = events
	return json.NewEncoder(out).Encode(result)
}

func printChecks(out io.Writer, checks []doctorCheck) {
	mark := map[string]string{"ok": "✓", "warn": "!", "info": "·"}
	for _, c := range checks {
		m := mark[c.Status]
		if m == "" {
			m = " "
		}
		fmt.Fprintf(out, "%s %-13s %s\n", m, c.Name, c.Detail)
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
