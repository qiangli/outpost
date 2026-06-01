// `outpost peers` — Wave 3B.1 surface for the reachability ledger
// and temporal-observation model.
//
//	outpost peers history [--json --limit N]
//	  Shows the most-recent reachability-ledger entries (one per
//	  successful SSH dial). Each row records who we reached, how
//	  (transport + endpoint), the handshake latency, and when.
//
//	outpost peers predicted [--json --hour H]
//	  Shows the EWMA presence prediction per peer at the given
//	  hour-of-week (0..167; default = current hour). Useful for
//	  "is my office printer likely up right now?" and (in Wave
//	  3B.2) the scheduling decisions the daemon makes for pre-
//	  warming connections.
//
// Both commands read the on-disk files directly (no MCP roundtrip)
// since the data is local. Daemon doesn't need to be running.
package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/discovery"
)

func peersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peers",
		Short: "Inspect the discovery cache, reachability history, and temporal predictions",
	}
	cmd.AddCommand(peersListCmd(), peersHistoryCmd(), peersPredictedCmd(), peersRouteToCmd())
	return cmd
}

// peersListCmd surfaces the daemon's live discovery cache via MCP
// resource `outpost://peers`. Combines mDNS browse hits, HTTP probe
// pins, and (Wave 3A.2) cloudbox NAT-hint entries.
func peersListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show the daemon's current discovery cache (mDNS + HTTP probes + hints)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			var peers []discovery.Peer
			if err := session.readResource(cmd.Context(), "outpost://peers", &peers); err != nil {
				return err
			}
			if jsonOut {
				b, _ := json.MarshalIndent(peers, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(peers) == 0 {
				fmt.Println("Discovery cache is empty. Confirm discovery_enabled=on and peers are reachable on the LAN.")
				return nil
			}
			fmt.Printf("%-22s  %-18s  %-26s  %-10s  %-8s  %s\n",
				"AGENT", "ASSIGNED.local", "PEER-ID", "TRUST", "PAIRED", "ENDPOINTS")
			for _, p := range peers {
				paired := "no"
				if p.Paired {
					paired = "yes"
				}
				fmt.Printf("%-22s  %-18s  %-26s  %-10s  %-8s  %s\n",
					truncStr(p.AgentName, 22),
					truncStr(p.AssignedHostname, 18),
					truncStr(string(p.ID), 26),
					string(p.Trust),
					paired,
					summarizeEndpoints(p.Endpoints),
				)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

// peersRouteToCmd surfaces the PRoPHET-lite transitive hint: when we
// want to reach peer C but can't directly, list any peer B such that
// (B → C) appears in the local reachability ledger. Output is
// "candidate hops, by recency" — operator decides whether to act.
func peersRouteToCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "route-to <peer-name-or-id>",
		Short: "List candidate ProxyJump hops to a peer based on the reachability ledger",
		Long: `route-to scans the reachability ledger for any peer that has
successfully dialed the named destination. Useful as a hint when
direct dial fails — operator can then try
'outpost ssh exec <dest> --jump <candidate>' to route through.

Phase 1: surfaces hints only; the daemon does NOT auto-route through
unverified peers. Phase 2 (alongside Memberlist gossip) widens the
search to include observations gossiped from other peers.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := args[0]
			path, err := discovery.DefaultLedgerPath()
			if err != nil {
				return err
			}
			l, err := discovery.OpenLedger(path)
			if err != nil {
				return err
			}
			edges, err := l.Tail(0)
			if err != nil {
				return err
			}
			// Group successful dials by (peer destination, source self).
			type hop struct {
				ViaSelf   discovery.PeerID `json:"via_self"`
				Endpoint  string           `json:"endpoint"`
				LatencyMs int64            `json:"latency_ms"`
				At        time.Time        `json:"at"`
			}
			matches := []hop{}
			for _, e := range edges {
				if e.PeerName == target || string(e.Peer) == target {
					matches = append(matches, hop{
						ViaSelf:   e.Self,
						Endpoint:  e.Endpoint.HostPort(),
						LatencyMs: e.LatencyMs,
						At:        e.At,
					})
				}
			}
			if jsonOut {
				b, _ := json.MarshalIndent(matches, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(matches) == 0 {
				fmt.Printf("No ledger entries for %q. Try dialing once via 'outpost ssh exec %s -- echo ok'.\n", target, target)
				return nil
			}
			fmt.Printf("Candidate routes to %q (most-recent first):\n\n", target)
			fmt.Printf("%-22s  %-22s  %8s  %s\n",
				"VIA (self-fingerprint)", "ENDPOINT", "LATENCY", "WHEN")
			// Newest first.
			for i := len(matches) - 1; i >= 0; i-- {
				m := matches[i]
				fmt.Printf("%-22s  %-22s  %5dms  %s\n",
					truncStr(string(m.ViaSelf), 22),
					truncStr(m.Endpoint, 22),
					m.LatencyMs,
					m.At.Local().Format("2006-01-02 15:04:05"),
				)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	return cmd
}

func peersHistoryCmd() *cobra.Command {
	var (
		jsonOut bool
		limit   int
	)
	cmd := &cobra.Command{
		Use:   "history",
		Short: "Show recent reachability-ledger entries",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := discovery.DefaultLedgerPath()
			if err != nil {
				return err
			}
			l, err := discovery.OpenLedger(path)
			if err != nil {
				return err
			}
			edges, err := l.Tail(limit)
			if err != nil {
				return err
			}
			if jsonOut {
				b, _ := json.MarshalIndent(edges, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(edges) == 0 {
				fmt.Println("Reachability ledger is empty — no successful SSH dials recorded yet.")
				return nil
			}
			fmt.Printf("%-22s  %-18s  %-22s  %8s  %s\n",
				"WHEN", "PEER", "ENDPOINT", "LATENCY", "TRANSPORT")
			for _, e := range edges {
				when := e.At.Local().Format("2006-01-02 15:04:05")
				peer := e.PeerName
				if peer == "" {
					peer = string(e.Peer)
				}
				ep := e.Endpoint.HostPort()
				fmt.Printf("%-22s  %-18s  %-22s  %5dms  %s\n",
					when,
					truncStr(peer, 18),
					truncStr(ep, 22),
					e.LatencyMs,
					e.Transport,
				)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	cmd.Flags().IntVar(&limit, "limit", 25, "Maximum entries to show (newest first). 0 = no limit.")
	return cmd
}

func peersPredictedCmd() *cobra.Command {
	var (
		jsonOut bool
		hour    int
	)
	cmd := &cobra.Command{
		Use:   "predicted",
		Short: "Show EWMA presence predictions per peer at a given hour-of-week",
		Long: `predicted dumps the temporal-observation model: for each peer
the daemon has tracked, the EWMA probability the peer is present at
the requested hour-of-week (0=Monday-00:00 .. 167=Sunday-23:00).

Wave 3B.1 surfaces predictions for debugging; the daemon does NOT yet
act on them. Wave 3B.2 will pre-warm SSH connections to highly-
predicted peers and schedule background work around predicted
absence.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("hour") {
				hour = currentHourOfWeek()
			}
			if hour < 0 || hour >= 168 {
				return fmt.Errorf("hour must be in 0..167")
			}
			path, err := discovery.DefaultObservationsPath()
			if err != nil {
				return err
			}
			o, err := discovery.OpenObservations(path)
			if err != nil {
				return err
			}
			rows := o.PredictedAt(hour)
			if jsonOut {
				b, _ := json.MarshalIndent(rows, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			if len(rows) == 0 {
				fmt.Println("Temporal-observation model is empty — the daemon hasn't recorded any peer ticks yet.")
				return nil
			}
			fmt.Printf("Predictions at hour-of-week=%d (%s)\n\n",
				hour, hourOfWeekLabel(hour))
			fmt.Printf("%-26s  %10s  %s\n", "PEER", "P(present)", "LAST SEEN")
			for _, r := range rows {
				last := "never"
				if !r.LastSeenAt.IsZero() {
					last = r.LastSeenAt.Local().Format("2006-01-02 15:04")
				}
				fmt.Printf("%-26s  %9.1f%%  %s\n",
					truncStr(string(r.PeerID), 26),
					r.Probability*100,
					last,
				)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Emit JSON instead of a table")
	cmd.Flags().IntVar(&hour, "hour", 0, "Hour-of-week to query (0..167). Defaults to current.")
	return cmd
}

// currentHourOfWeek returns the bucket index for time.Now() in UTC,
// matching the daemon-side hourOfWeek() in observations.go.
func currentHourOfWeek() int {
	t := time.Now().UTC()
	wd := int(t.Weekday())
	wd = (wd + 6) % 7
	return wd*24 + t.Hour()
}

// hourOfWeekLabel renders bucket index N as a human-friendly
// "Day HH:00 UTC" string, helpful in the predicted-table header.
func hourOfWeekLabel(h int) string {
	if h < 0 || h >= 168 {
		return "out-of-range"
	}
	days := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	return fmt.Sprintf("%s %02d:00 UTC", days[h/24], h%24)
}
