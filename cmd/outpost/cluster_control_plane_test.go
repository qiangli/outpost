package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// TestPrintControlPlaneStatus_NoCredentialLeaks verifies the CLI renderer never
// emits token values, only presence booleans and node details.
func TestPrintControlPlaneStatus_NoCredentialLeaks(t *testing.T) {
	const joinToken = "FAKE-JOIN-TOKEN-xyz"
	const nodeToken = "FAKE-NODE-TOKEN-abc"
	const stcpSecret = "FAKE-STCP-SECRET-def"

	type statusStruct struct {
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
	}

	status := statusStruct{
		Hosted:              true,
		ContainerExists:     true,
		ContainerRunning:    true,
		APIServerServing:    true,
		APIServerStatusCode: 200,
		Nodes: []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
		}{
			{Name: "worker1", Ready: true},
			{Name: "worker2", Ready: false},
		},
		NodeCount:     2,
		JoinEndpoint:  "https://127.0.0.1:6443",
		HasJoinToken:  true,
		HasNodeToken:  true,
		HasSTCPSecret: true,
		CheckedAt:     1234567890,
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

	// Verify presence booleans appear.
	if !strings.Contains(output, "join_token=true") {
		t.Errorf("output does not show join_token presence: %s", output)
	}

	// Verify node names appear.
	if !strings.Contains(output, "worker1") {
		t.Errorf("output does not show worker1: %s", output)
	}

	// Verify credential values do NOT appear.
	if strings.Contains(output, joinToken) || strings.Contains(output, nodeToken) || strings.Contains(output, stcpSecret) {
		t.Errorf("output contains a credential value: %s", output)
	}
}
