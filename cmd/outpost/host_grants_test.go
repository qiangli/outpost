package main

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestHostGrantSourcesOnePerAttachment(t *testing.T) {
	fc := &conf.FileConfig{
		Cloudbox: &conf.CloudboxPairing{OwnerEmail: "o@x"},
		Attachments: []conf.SatelliteAttachment{
			{SatelliteID: "one", Endpoint: "https://one.example", Token: "t1"},
			{SatelliteID: "two", Endpoint: "https://two.example", Token: "t2"},
			{SatelliteID: "notoken", Endpoint: "https://three.example"},
		},
	}
	srcs := hostGrantSources(fc)
	if len(srcs) != 2 || srcs[0].Endpoint != "https://one.example" || srcs[1].Token != "t2" {
		t.Fatalf("sources = %+v", srcs)
	}
	if got := hostGrantOwner(fc); got != "o@x" {
		t.Fatalf("owner = %q", got)
	}
	if hostGrantOwner(&conf.FileConfig{}) != "" || hostGrantSources(nil) != nil {
		t.Fatal("empty config should yield no owner/sources")
	}
}
