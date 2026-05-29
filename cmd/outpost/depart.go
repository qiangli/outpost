package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// departCmd implements `outpost depart` — the public CLI for the
// Phase 2b cooperative-failover hook. Reads the persisted
// access_token + agent_name from agent.json and POSTs to
// <cloudbox>/api/v1/cluster/departing so cloudbox stops routing new
// cluster-svc traffic to this host's pods.
//
// Wire it to platform-specific sleep events to make laptop mobility
// truly transparent:
//
//	macOS launchd (~/Library/LaunchAgents/outpost.depart-on-sleep.plist
//	with `WatchPaths` on /var/log/system.log isn't reliable; use
//	sleepwatcher or a launchd com.apple.sleep listener):
//
//	  # /usr/local/etc/sleepwatcher/rc.sleep
//	  #!/bin/sh
//	  /usr/local/bin/outpost depart
//
//	systemd-logind (Linux):
//
//	  # /etc/systemd/system/outpost-depart-on-sleep.service
//	  [Unit]
//	  Description=Notify cloudbox before suspend
//	  Before=sleep.target
//	  StopWhenUnneeded=yes
//
//	  [Service]
//	  Type=oneshot
//	  User=qiangli
//	  ExecStart=/usr/local/bin/outpost depart
//
//	  [Install]
//	  WantedBy=sleep.target
//
// Idempotent — running multiple times in a row only refreshes the
// 30 s departingTTL on cloudbox. Returns 0 on success (signal
// delivered) or non-zero with a one-line stderr message on failure.
// All HTTP errors are reported — a sleep hook can choose to log
// them and continue, since the reactive ~40 s NodeNotReady path is
// still the fallback.
func departCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "depart",
		Short: "Notify cloudbox that this outpost is about to go offline",
		Long: `Tell cloudbox to stop routing cluster-svc traffic to this
outpost's pods for the next 30 s. Intended to be wired to
platform-specific pre-sleep / pre-shutdown hooks so a laptop
closing its lid doesn't surface as a transient 503 on
/api/cluster/svc/<name>/* for users mid-session.

Daemon need not be stopped — the matrix tunnel naturally drops
on sleep and reconnects on wake, at which point cloudbox
re-registers this host and the departing flag clears.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath, err := conf.DefaultConfigPath()
			if err != nil {
				return err
			}
			fc, err := conf.LoadFile(cfgPath)
			if err != nil {
				return fmt.Errorf("load %s: %w", cfgPath, err)
			}
			if fc == nil || fc.AccessToken == "" {
				return errors.New("no access_token saved — run `outpost register` first")
			}
			if fc.AgentName == "" {
				return errors.New("no agent_name in saved config")
			}
			base := cloudboxHTTPBase(fc)
			if base == "" {
				return errors.New("no cloudbox URL in saved config (server_addr / protocol missing)")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			if err := notifyDeparting(ctx, base, fc.AccessToken, fc.AgentName); err != nil {
				return fmt.Errorf("cloudbox: %w", err)
			}
			fmt.Printf("departed: %s — cloudbox will skip this host for ~30 s\n", fc.AgentName)
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 3*time.Second,
		"Hard budget for the round-trip to cloudbox before giving up")
	return cmd
}
