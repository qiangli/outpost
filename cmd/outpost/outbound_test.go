package main

import (
	"strings"
	"testing"
)

func TestParseTTL(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"default", 0, false},
		{"DEFAULT", 0, false},
		{"infinite", ttlInfiniteSeconds, false},
		{"inf", ttlInfiniteSeconds, false},
		{"24h", 86400, false},
		{"1h30m", 5400, false},
		{"30s", 30, false},
		{"0s", 0, true},  // zero must be expressed as "default"
		{"-5s", 0, true}, // no negatives
		{"garbage", 0, true},
	}
	for _, tc := range cases {
		got, err := parseTTL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseTTL(%q): want error, got %d", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTTL(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseTTL(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestFormatTTL(t *testing.T) {
	cases := []struct {
		in      int64
		wantSub string
	}{
		{0, "default"},
		{-1, "default"},
		{ttlInfiniteSeconds, "infinite"},
		{86400, "h"},
		{60, "m"},
		{45, "s"},
	}
	for _, tc := range cases {
		got := formatTTL(tc.in)
		if !strings.Contains(got, tc.wantSub) {
			t.Errorf("formatTTL(%d) = %q, want it to contain %q", tc.in, got, tc.wantSub)
		}
	}
}
