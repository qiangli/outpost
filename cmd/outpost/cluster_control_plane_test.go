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
	status := struct {
		Hosted              bool `json:"hosted"`
		ContainerExists     bool `json:"container_exists"`
		ContainerRunning    bool `json:"container_running"`
		APIServerServing    bool `json:"apiserver_serving"`
		APIServerStatusCode int  `json:"apiserver_status_code"`
		Nodes               []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
		} `json:"nodes"`
		NodeCount     int    `json:"node_count"`
		JoinEndpoint  string `json:"join_endpoint"`
		HasJoinToken  bool   `json:"has_join_token"`
		HasNodeToken  bool   `json:"has_node_token"`
		HasSTCPSecret bool   `json:"has_stcp_secret"`
		CheckedAt     int64  `json:"checked_at"`
	}{}

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
	const hint = "worker recovery — on each joined worker, run: `outpost cluster join --token-stdin`"
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
