package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/recovery"
	"github.com/qiangli/outpost/internal/agent/svcunit"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"
)

type recoveryUnitSource struct{ unit fleet.Service }

func (s recoveryUnitSource) Units(context.Context) ([]fleet.Service, error) {
	return []fleet.Service{s.unit}, nil
}

func (s recoveryUnitSource) Statuses(context.Context) ([]service.Status, error) {
	return nil, nil
}

func TestSSHDRecoversFromKilledListener(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	capability := newTestSSHDCapability(testSSHDHostKey(t), &fakeSSHDFirewall{}, &fakeSSHDAdvertiser{})
	port := freePort(t)
	unit := defaultSSHDUnit()
	unit.Settings["authorized_keys"] = filepath.Join(t.TempDir(), "authorized_keys")
	unit.Ports[0].Port = port
	registry := service.NewRegistry()
	if err := registry.Register(capSSHD, capability); err != nil {
		t.Fatal(err)
	}
	reconciler := svcunit.New(svcunit.Deps{
		Source:   recoveryUnitSource{unit},
		Internal: service.NewInternal(fleet.ServiceOwnerOutpost, registry),
		Interval: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	address := fmt.Sprintf("127.0.0.1:%d", port)
	if err := waitSSHBanner(t, address, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	capability.mu.Lock()
	killed := capability.ln
	err := killed.Close()
	capability.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := waitSSHBanner(t, address, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// The listener may send a banner before Activate has saved the event.
	_ = capability.boundPort()
	events, err := recovery.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].State != "down" || events[1].State != "recovered" {
		t.Fatalf("listener transitions = %+v", events)
	}
	for _, event := range events {
		if event.Component != "lan_sshd" || event.Reason != "listener_exit" {
			t.Fatalf("event = %+v, want lan_sshd/listener_exit", event)
		}
	}
}

func TestSSHDRebindBacksOffOnOccupiedPort(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = occupied.Close() })
	capability := newTestSSHDCapability(testSSHDHostKey(t), &fakeSSHDFirewall{}, &fakeSSHDAdvertiser{})
	cfg := sshdTestConfig(occupied.Addr().(*net.TCPAddr).Port, filepath.Join(t.TempDir(), "authorized_keys"))
	ctx := context.Background()
	defer capability.Deactivate(ctx)

	bindErr := capability.Activate(ctx, cfg)
	if bindErr == nil {
		t.Fatal("occupied port was accepted")
	}
	if err := occupied.Close(); err != nil {
		t.Fatal(err)
	}
	if err := capability.Activate(ctx, cfg); err != bindErr {
		t.Fatalf("reconcile retried before backoff expired: %v", err)
	}
	if capability.retryDelay < 2*time.Second || capability.retryDelay > 30*time.Second {
		t.Fatalf("retry delay out of bounds: %v", capability.retryDelay)
	}
	capability.retryAt = time.Now().Add(-time.Second)
	if err := capability.Activate(ctx, cfg); err != nil {
		t.Fatalf("retry after backoff: %v", err)
	}
	events, err := recovery.Read()
	if err != nil || len(events) != 2 || events[0].Reason != "bind_failed" ||
		events[1].Reason != "bind_failed" || events[1].State != "recovered" {
		t.Fatalf("rebind transitions = %+v, read error = %v", events, err)
	}
}

func TestRecoveryDoctorJSON(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	recovery.Record("ssh_client", "blocked", "client_elevation_expired")
	checks := []doctorCheck{doctorSleepCheck("enabled", true), doctorSleepCheck("unknown", true)}
	var output bytes.Buffer
	if err := writeDoctorJSON(&output, checks); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Checks   []doctorCheck    `json:"checks"`
		Recovery []recovery.Event `json:"recovery"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Checks) != 2 || result.Checks[0].Status != "warn" ||
		!strings.Contains(result.Checks[0].Detail, "always-on") ||
		!strings.Contains(result.Checks[1].Detail, "unknown") ||
		len(result.Recovery) != 1 || result.Recovery[0].Reason != "client_elevation_expired" {
		t.Fatalf("doctor JSON = %s", output.String())
	}
}
