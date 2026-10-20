package main

import (
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/runtime"
)

func TestApplyFederationVisitor(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("federation is refused on windows")
	}
	member := &conf.ClusterConfig{Federation: &conf.FederationConfig{
		Enabled: true, Endpoint: "192.0.2.10:7000", Token: "tok", STCPSecret: "sec",
	}}
	var opts runtime.Options
	applyFederationVisitor(member, &opts)
	if opts.FederationHost != "192.0.2.10" || opts.FederationPort != 7000 || opts.FederationToken != "tok" ||
		opts.FederationSecret != "sec" || opts.FederationBindPort != 15443 || opts.FederationProtocol != "tcp" {
		t.Fatalf("member visitor opts = %+v", opts)
	}
	for name, cc := range map[string]*conf.ClusterConfig{
		"nil":  nil,
		"host": {Federation: &conf.FederationConfig{Enabled: true, Host: true, Endpoint: "192.0.2.10:7000", STCPSecret: "s"}},
		"off":  {Federation: &conf.FederationConfig{Endpoint: "192.0.2.10:7000", STCPSecret: "s"}},
		"bad":  {Federation: &conf.FederationConfig{Enabled: true, Endpoint: "no-port", STCPSecret: "s"}},
	} {
		var o runtime.Options
		applyFederationVisitor(cc, &o)
		if o.FederationActive() {
			t.Errorf("%s: visitor turned on: %+v", name, o)
		}
	}
}

func TestResolveFederationJoin(t *testing.T) {
	t.Setenv(envFederationJoinToken, "env-tunnel")
	t.Setenv(envFederationSTCP, "env-stcp")
	t.Setenv(envFederationBootstrap, "abcdef.0123456789abcdef")
	cmd := &cobra.Command{}
	p, err := resolveFederationJoin(cmd, "192.0.2.10:7000", "m", &federationJoinFlags{caHash: "sha256:x"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Token != "env-tunnel" || p.STCPSecret != "env-stcp" || p.BootstrapToken != "abcdef.0123456789abcdef" || p.ClusterName != "m" {
		t.Fatalf("env fallback: %+v", p)
	}

	cmd.SetIn(strings.NewReader("stdin-tunnel\n"))
	p, err = resolveFederationJoin(cmd, "192.0.2.10:7000", "", &federationJoinFlags{tokenStdin: true, stcpSecret: "flag-stcp", caHash: "sha256:x"})
	if err != nil || p.Token != "stdin-tunnel" || p.STCPSecret != "flag-stcp" {
		t.Fatalf("stdin/flag: %+v %v", p, err)
	}
	if _, err := resolveFederationJoin(cmd, "e", "", &federationJoinFlags{token: "a", tokenStdin: true, caHash: "x"}); err == nil {
		t.Fatal("--token with --token-stdin accepted")
	}
	if _, err := resolveFederationJoin(&cobra.Command{}, "e", "", &federationJoinFlags{}); err == nil || !strings.Contains(err.Error(), "--ca-hash") {
		t.Fatalf("missing ca hash: %v", err)
	}
}
