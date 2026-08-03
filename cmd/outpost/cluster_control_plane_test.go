package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// TestPrintControlPlaneStatus_StripsMalformedTokens verifies that even if a
// server sends token values alongside legitimate fields, the CLI's struct
// definition causes them to be dropped during unmarshal, so printControlPlaneStatus
// never sees them to render.
func TestPrintControlPlaneStatus_StripsMalformedTokens(t *testing.T) {
	const joinToken = "FAKE-JOIN-TOKEN-xyz"
	const nodeToken = "FAKE-NODE-TOKEN-abc"
	const stcpSecret = "FAKE-STCP-SECRET-def"

	// Build a malicious JSON payload that includes token values alongside
	// legitimate fields. This simulates a server breach or bug.
	maliciousJSON := `{
		"hosted": true,
		"container_exists": true,
		"container_running": true,
		"apiserver_serving": true,
		"apiserver_status_code": 200,
		"nodes": [
			{"name": "worker1", "ready": true},
			{"name": "worker2", "ready": false}
		],
		"node_count": 2,
		"join_endpoint": "https://127.0.0.1:6443",
		"has_join_token": true,
		"has_node_token": true,
		"has_stcp_secret": true,
		"checked_at": 1234567890,
		"join_token": "FAKE-JOIN-TOKEN-xyz",
		"node_token": "FAKE-NODE-TOKEN-abc",
		"stcp_secret": "FAKE-STCP-SECRET-def"
	}`

	// Unmarshal into the CLI's struct type, which has no fields for the
	// token values. The struct's type definition causes them to be dropped.
	var status controlPlaneStatusResult

	if err := json.Unmarshal([]byte(maliciousJSON), &status); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Redirect stdout to capture output.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	go func() {
		printControlPlaneStatus(status)
		w.Close()
	}()

	// Collect output.
	var buf bytes.Buffer
	io.Copy(&buf, r)
	os.Stdout = oldStdout
	output := buf.String()

	// Verify presence booleans appear (struct fields survived unmarshal).
	if !strings.Contains(output, "join_token=true") {
		t.Errorf("output does not show join_token presence: %s", output)
	}

	// Verify node names appear (struct fields survived unmarshal).
	if !strings.Contains(output, "worker1") {
		t.Errorf("output does not show worker1: %s", output)
	}

	// Verify credential values do NOT appear (struct def dropped the extra
	// JSON fields during unmarshal, so they never reached the renderer).
	if strings.Contains(output, joinToken) || strings.Contains(output, nodeToken) || strings.Contains(output, stcpSecret) {
		t.Errorf("output contains a credential value: %s", output)
	}
}

// TestControlPlaneOut_CarriesWorkerRejoinHint pins the CLI's rotate-response
// struct to the admincore field it mirrors. If admincore.ControlPlaneResult
// gains or renames the hint field without cmd/outpost following, the rotate
// command silently stops printing a recovery path — this test exists to make
// that drift a compile-visible or a test-visible failure instead.
func TestControlPlaneOut_CarriesWorkerRejoinHint(t *testing.T) {
	const hint = "worker recovery — on this control-plane host, run once for each joined worker: `outpost cluster join --token-stdin`"
	body := `{
		"control_plane": true,
		"bind_addr": "127.0.0.1",
		"bind_port": 7000,
		"has_token": true,
		"tunnel_token": "NEW-TOKEN-abc",
		"restart_pending": true,
		"worker_rejoin_hint": ` + `"` + hint + `"` + `
	}`
	var out controlPlaneOut
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.WorkerRejoinHint != hint {
		t.Errorf("worker rejoin hint = %q, want %q", out.WorkerRejoinHint, hint)
	}
	if strings.Contains(out.WorkerRejoinHint, out.TunnelToken) {
		t.Errorf("hint embeds the literal token: %q", out.WorkerRejoinHint)
	}
}

// A control plane whose address reconciler is NOT running is the exact state
// that produces "no address matched types [ExternalIP]" while nodes look
// perfectly healthy. The human status output has to say so out loud — if this
// line is ever dropped or softened, the only remaining evidence of the fault is
// a metrics-server log on a different machine.
func TestPrintControlPlaneStatus_ReportsStoppedNodeAddrReconciler(t *testing.T) {
	status := controlPlaneStatusResult{
		Hosted:           true,
		ContainerRunning: true,
		APIServerServing: true,
		// The reconciler never started — the defect.
		NodeAddrReconcilerRunning: false,
	}

	out := captureStdout(t, func() { printControlPlaneStatus(status) })

	if !strings.Contains(out, "NOT RUNNING") {
		t.Errorf("a stopped address reconciler must be called out; got:\n%s", out)
	}
	// It must also say what breaks, so the reader connects this line to the
	// symptom they actually arrived with.
	if !strings.Contains(out, "kubectl") {
		t.Errorf("output must name the commands that fail; got:\n%s", out)
	}
}

// Running-and-failing is a different problem with a different fix than
// never-started, and the status surface is the only place that distinguishes
// them.
func TestPrintControlPlaneStatus_DistinguishesRunningFromFailing(t *testing.T) {
	healthy := captureStdout(t, func() {
		printControlPlaneStatus(controlPlaneStatusResult{
			Hosted: true, NodeAddrReconcilerRunning: true,
		})
	})
	if !strings.Contains(healthy, "node addressing:     running") ||
		strings.Contains(healthy, "NOT RUNNING") {
		t.Errorf("a healthy reconciler must read as plainly running; got:\n%s", healthy)
	}

	failing := captureStdout(t, func() {
		printControlPlaneStatus(controlPlaneStatusResult{
			Hosted:                    true,
			NodeAddrReconcilerRunning: true,
			NodeAddrLastError:         "nodes is forbidden",
		})
	})
	if !strings.Contains(failing, "LAST PASS FAILED") ||
		!strings.Contains(failing, "nodes is forbidden") {
		t.Errorf("a failing reconciler must surface its error; got:\n%s", failing)
	}
	if strings.Contains(failing, "NOT RUNNING") {
		t.Errorf("running-and-failing must not be reported as never-started; got:\n%s", failing)
	}
}

// The runbook in docs/cluster-peer.md tells operators to branch on
// `... status --json | jq .nodeaddr_reconciler_running`. That only works if the
// field is emitted when false — an omitempty here would render "the reconciler
// is dead", the single most important state, as an absent key indistinguishable
// from an old daemon that cannot report it at all.
func TestControlPlaneStatusResult_EmitsNodeAddrRunningWhenFalse(t *testing.T) {
	blob, err := json.Marshal(controlPlaneStatusResult{Hosted: true})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	running, ok := got["nodeaddr_reconciler_running"]
	if !ok {
		t.Fatalf("nodeaddr_reconciler_running missing from JSON: %s", blob)
	}
	if running != false {
		t.Errorf("nodeaddr_reconciler_running = %v, want false", running)
	}
}

// --json re-encodes the decoded struct, so it must inherit the same redaction
// the human path gets: a token the daemon should never have sent has no field
// to land in and cannot reappear in the machine-readable output either.
func TestControlPlaneStatusResult_JSONRoundTripDropsCredentials(t *testing.T) {
	const secret = "FAKE-STCP-SECRET-def"
	var status controlPlaneStatusResult
	if err := json.Unmarshal([]byte(`{
		"hosted": true,
		"has_stcp_secret": true,
		"stcp_secret": "`+secret+`",
		"nodeaddr_reconciler_running": true
	}`), &status); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	blob, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(blob), secret) {
		t.Errorf("--json output leaked a credential: %s", blob)
	}
	if !strings.Contains(string(blob), `"has_stcp_secret":true`) {
		t.Errorf("presence flag must survive the round trip: %s", blob)
	}
}
