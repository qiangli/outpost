package main

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/fleetreg"
)

func TestFederationFactsFromConfig(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		cc   *conf.ClusterConfig
		role string
	}{
		{"nil", nil, ""},
		{"off", &conf.ClusterConfig{Federation: &conf.FederationConfig{CAHash: hash}}, ""},
		{"on but hash unknown", &conf.ClusterConfig{Federation: &conf.FederationConfig{Enabled: true, Host: true}}, ""},
		{"host", &conf.ClusterConfig{Federation: &conf.FederationConfig{Enabled: true, Host: true, CAHash: hash, ClusterName: "h"}}, fleetreg.FederationRoleHost},
		{"member", &conf.ClusterConfig{Federation: &conf.FederationConfig{Enabled: true, CAHash: hash, ClusterName: "m"}}, fleetreg.FederationRoleMember},
	}
	for _, tc := range cases {
		h, role, _, mode := federationFactsFromConfig(tc.cc)
		if role != tc.role {
			t.Errorf("%s: role = %q, want %q", tc.name, role, tc.role)
		}
		if tc.role == "" && (h != "" || mode != "") {
			t.Errorf("%s: reported facts without identity: %q %q", tc.name, h, mode)
		}
		if tc.role != "" && (h != hash || mode != fleetreg.FederationSyncPull) {
			t.Errorf("%s: facts = %q %q", tc.name, h, mode)
		}
	}
	if info := fleetreg.NewFederationInfo(federationFactsFromConfig(cases[3].cc)); info == nil {
		t.Error("host with a known hash must produce federation info")
	}
}

func TestFederationVerbsRegistered(t *testing.T) {
	if c := clusterFederationCmd(); c.Use == "" || c.Flags().Lookup("quiet") == nil || c.Flags().Lookup("json") == nil {
		t.Fatal("federation command missing flags")
	}
}
