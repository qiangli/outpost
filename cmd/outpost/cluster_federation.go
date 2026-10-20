package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/federation"
)

// Environment fallbacks for the member's join credentials — the names
// `federation token`'s join hint prints, so a join never needs them in argv.
const (
	envFederationJoinToken = "OUTPOST_FEDERATION_JOIN_TOKEN"
	envFederationSTCP      = "OUTPOST_FEDERATION_STCP_SECRET"
	envFederationBootstrap = "OUTPOST_FEDERATION_BOOTSTRAP_TOKEN"
)

type federationJoinFlags struct {
	token, stcpSecret, bootstrap, caHash string
	tokenStdin                           bool
}

// resolveFederationJoin turns flags + env + stdin into join params. --token
// and --token-stdin carry the TUNNEL token, as with `cluster join`.
func resolveFederationJoin(cmd *cobra.Command, endpoint, clusterName string, f *federationJoinFlags) (admincore.FederationJoinParams, error) {
	p := admincore.FederationJoinParams{Endpoint: endpoint, ClusterName: clusterName, CAHash: f.caHash}
	token := f.token
	if f.tokenStdin {
		if token != "" {
			return p, errors.New("pass either --token or --token-stdin, not both")
		}
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return p, fmt.Errorf("read token from stdin: %w", err)
		}
		if token = strings.TrimSpace(string(b)); token == "" {
			return p, errors.New("--token-stdin: stdin was empty")
		}
	}
	p.Token = firstNonEmpty(token, strings.TrimSpace(os.Getenv(envFederationJoinToken)))
	p.STCPSecret = firstNonEmpty(f.stcpSecret, strings.TrimSpace(os.Getenv(envFederationSTCP)))
	p.BootstrapToken = firstNonEmpty(f.bootstrap, strings.TrimSpace(os.Getenv(envFederationBootstrap)))
	switch {
	case p.STCPSecret == "":
		return p, fmt.Errorf("--stcp-secret (or $%s) is required", envFederationSTCP)
	case p.BootstrapToken == "":
		return p, fmt.Errorf("--bootstrap-token (or $%s) is required", envFederationBootstrap)
	case p.CAHash == "":
		return p, errors.New("--ca-hash is required")
	}
	return p, nil
}

// clusterFederationCmd is the operator surface for Karmada federation on a
// host that hosts a DKS control plane.
//
//	outpost cluster federation              # status
//	outpost cluster federation on           # publish Karmada, install, self-register
//	outpost cluster federation off
//	outpost cluster federation token        # what a member needs to join
//	outpost cluster federation join <endpoint> --stcp-secret … --bootstrap-token … --ca-hash …
//	outpost cluster federation leave
func clusterFederationCmd() *cobra.Command {
	var jsonOut, force bool
	var clusterName, kubeconfig, karmadaConfig string
	var apiPort int
	var jf federationJoinFlags

	cmd := &cobra.Command{
		Use:   "federation [on|off|status|token|join <endpoint>|leave]",
		Short: "Federate this host's DKS with Karmada, or report federation state",
		Long: `Report or change this host's Karmada federation role.

` + "`on`" + ` requires a hosted control plane (` + "`outpost cluster control-plane on`" + `). It
records intent and restarts; the daemon then installs the karmada built-in,
writes the federation kubeconfig, learns the CA hash that identifies the
federation (fed-<12hex>) and registers this host's own plane as a member.
Run ` + "`status`" + ` to follow progress — it never prints credentials.

` + "`token`" + ` mints a bootstrap token and prints what a member needs. Those are
credentials; use --quiet to pipe the bare bootstrap token.

` + "`join <endpoint>`" + ` makes this host's plane (it needs ` + "`control-plane on`" + `) a
pull-mode member of the federation whose ` + "`token`" + ` output you have. The tunnel
token comes from --token, --token-stdin or $` + envFederationJoinToken + `; the STCP
secret and bootstrap token from flags or $` + envFederationSTCP + ` /
$` + envFederationBootstrap + `. The bootstrap token is never stored in the
config: it waits in a 0600 file until ` + "`karmadactl register`" + ` succeeds. The
daemon restarts, brings up the federation visitor on 127.0.0.1:15443 and
finishes the registration; ` + "`status`" + ` shows progress. ` + "`leave`" + ` unregisters.

Federation is not supported on Windows (karmadactl has no Windows release).`,
		ValidArgs: []string{"on", "off", "status", "token", "join", "leave"},
		Args:      cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			verb := "status"
			if len(args) >= 1 {
				verb = args[0]
			}
			if len(args) == 2 && verb != "join" {
				return fmt.Errorf("%s takes no argument", verb)
			}
			var joinParams admincore.FederationJoinParams
			switch verb {
			case "join", "leave":
				if err := federation.CheckOS(); err != nil {
					return err
				}
			}
			if verb == "join" {
				if len(args) != 2 {
					return errors.New("usage: outpost cluster federation join <endpoint> --stcp-secret … --bootstrap-token … --ca-hash …")
				}
				var err error
				if joinParams, err = resolveFederationJoin(cmd, args[1], clusterName, &jf); err != nil {
					return err
				}
			}
			session, err := dialMCP(cmd.Context())
			if err != nil {
				return err
			}
			defer session.close()
			ctx := cmd.Context()

			switch verb {
			case "status":
				var out admincore.FederationResult
				if err := session.callTool(ctx, "outpost_federation", map[string]any{}, &out); err != nil {
					return err
				}
				return printFederation(out, jsonOut)
			case "on":
				params := map[string]any{}
				if clusterName != "" {
					params["cluster_name"] = clusterName
				}
				if apiPort != 0 {
					params["api_port"] = apiPort
				}
				if kubeconfig != "" {
					params["kubeconfig"] = kubeconfig
				}
				var out admincore.FederationResult
				if err := session.callTool(ctx, "outpost_federation_on", params, &out); err != nil {
					return err
				}
				if err := printFederation(out, jsonOut); err != nil {
					return err
				}
				if out.RestartPending && !jsonOut {
					fmt.Println("\nRestarting outpost to apply.")
				}
				return nil
			case "off":
				var out admincore.FederationResult
				if err := session.callTool(ctx, "outpost_federation_off", map[string]any{"force": force, "karmada_config": karmadaConfig}, &out); err != nil {
					return err
				}
				if err := printFederation(out, jsonOut); err != nil {
					return err
				}
				if out.Warning != "" && !jsonOut {
					fmt.Fprintln(os.Stderr, "warning:", out.Warning)
				}
				return nil
			case "join":
				var out admincore.FederationResult
				if err := session.callTool(ctx, "outpost_federation_join", joinParams, &out); err != nil {
					return err
				}
				if err := printFederation(out, jsonOut); err != nil {
					return err
				}
				if out.RestartPending && !jsonOut {
					fmt.Println("\nRestarting outpost to bring up the federation visitor; registration follows (check `outpost cluster federation status`).")
				}
				return nil
			case "leave":
				var out admincore.FederationResult
				if err := session.callTool(ctx, "outpost_federation_leave", map[string]any{"force": force, "karmada_config": karmadaConfig}, &out); err != nil {
					return err
				}
				if err := printFederation(out, jsonOut); err != nil {
					return err
				}
				if out.Warning != "" && !jsonOut {
					fmt.Fprintln(os.Stderr, "warning:", out.Warning)
				}
				return nil
			case "token":
				var out admincore.FederationTokenResult
				if err := session.callTool(ctx, "outpost_federation_token", map[string]any{}, &out); err != nil {
					return err
				}
				if quiet, _ := cmd.Flags().GetBool("quiet"); quiet {
					fmt.Println(out.BootstrapToken)
					return nil
				}
				if jsonOut {
					return json.NewEncoder(os.Stdout).Encode(out)
				}
				fmt.Printf("endpoint:         %s\n", out.Endpoint)
				fmt.Printf("tunnel token:     %s\n", out.TunnelToken)
				fmt.Printf("stcp secret:      %s\n", out.STCPSecret)
				fmt.Printf("bootstrap token:  %s\n", out.BootstrapToken)
				fmt.Printf("ca hash:          sha256:%s\n", out.CAHash)
				fmt.Printf("federation id:    %s\n\n%s\n", out.FederationID, out.JoinHint)
				return nil
			}
			return fmt.Errorf("unknown verb %q (want on|off|status|token|join|leave)", verb)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print JSON")
	cmd.Flags().BoolVarP(new(bool), "quiet", "q", false, "token: print only the bootstrap token")
	cmd.Flags().BoolVar(&force, "force", false, "off/leave: clear the config even if uninstall/unregister fails")
	cmd.Flags().StringVar(&clusterName, "cluster-name", "", "on/join: name this host's plane registers under (default: agent name)")
	cmd.Flags().StringVar(&jf.token, "token", "", "join: the federation host's tunnel token (prefer --token-stdin or $"+envFederationJoinToken+")")
	cmd.Flags().BoolVar(&jf.tokenStdin, "token-stdin", false, "join: read the tunnel token from stdin")
	cmd.Flags().StringVar(&jf.stcpSecret, "stcp-secret", "", "join: the federation host's STCP secret (or $"+envFederationSTCP+")")
	cmd.Flags().StringVar(&jf.bootstrap, "bootstrap-token", "", "join: Karmada bootstrap token (or $"+envFederationBootstrap+")")
	cmd.Flags().StringVar(&jf.caHash, "ca-hash", "", "join: Karmada CA hash, sha256:<hex>")
	cmd.Flags().IntVar(&apiPort, "api-port", 0, "on: loopback port of the federation apiserver forward (default 15443)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "on: path for the materialised Karmada kubeconfig")
	cmd.Flags().StringVar(&karmadaConfig, "karmada-config", "", "member off/leave: host-accessible Karmada admin kubeconfig for control-plane RBAC cleanup")
	return cmd
}

func printFederation(out admincore.FederationResult, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if !out.Enabled {
		fmt.Println("federation: off")
		return nil
	}
	fmt.Printf("federation:     %s\n", out.Role)
	if out.Role == "member" {
		fmt.Printf("endpoint:       %s\n", orDash(out.Endpoint))
		if out.FederationID != "" {
			fmt.Printf("federation id:  %s\n", out.FederationID)
		}
		fmt.Printf("cluster name:   %s\n", orDash(out.ClusterName))
		fmt.Printf("api port:       %d\n", out.APIPort)
		fmt.Printf("join pending:   %v\n", out.JoinPending)
		fmt.Printf("visitor bound:  %v\n", out.VisitorBound)
		fmt.Printf("mirror ip:      %s\n", orDash(out.MirrorIP))
		fmt.Printf("registered:     %v (agent ready: %v)\n", out.SelfRegistered, out.AgentReady)
		if out.LastError != "" {
			fmt.Printf("last error:     %s\n", out.LastError)
		}
		return nil
	}
	fmt.Printf("karmada:        %s\n", orDash(out.Phase))
	if out.FederationID != "" {
		fmt.Printf("federation id:  %s\n", out.FederationID)
	}
	fmt.Printf("cluster name:   %s\n", orDash(out.ClusterName))
	fmt.Printf("api port:       %d\n", out.APIPort)
	fmt.Printf("kubeconfig:     %s (present: %v)\n", out.Kubeconfig, out.KubeconfigOK)
	fmt.Printf("registered:     %v\n", out.SelfRegistered)
	fmt.Printf("members:        %d\n", len(out.Members))
	for _, member := range out.Members {
		fmt.Printf("  %s  ready=%v  sync=%s\n", member.Name, member.Ready, orDash(member.SyncMode))
	}
	if out.LastError != "" {
		fmt.Printf("last error:     %s\n", out.LastError)
	}
	return nil
}
