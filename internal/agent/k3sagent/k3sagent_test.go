//go:build linux

package k3sagent

import (
	"strings"
	"testing"
)

func TestOptionsValidate(t *testing.T) {
	good := Options{
		Server:   "https://127.0.0.1:6443",
		Token:    "K10x::node:s",
		NodeName: "host-a",
	}
	cases := []struct {
		name   string
		mutate func(*Options)
		want   string // substring expected in error; "" = no error
	}{
		{"happy", func(o *Options) {}, ""},
		{"missing server", func(o *Options) { o.Server = "" }, "Options.Server required"},
		{"missing token", func(o *Options) { o.Token = "" }, "Options.Token required"},
		{"missing node", func(o *Options) { o.NodeName = "" }, "Options.NodeName required"},
		{"relative dir", func(o *Options) { o.DataDir = "relative/dir" }, "must be absolute"},
		{"absolute dir ok", func(o *Options) { o.DataDir = "/tmp/x" }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := good
			tc.mutate(&o)
			err := o.validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("expected nil, got %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("expected error containing %q, got nil", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
		})
	}
}
