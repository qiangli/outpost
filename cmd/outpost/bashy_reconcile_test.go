package main

import "testing"

func TestParseBashyBanner(t *testing.T) {
	cases := map[string]string{
		"GNU bash, version 5.3.0(1)-bashy-0.13.0\n":         "0.13.0",
		"GNU bash, version 5.3.0(1)-bashy-0.13.0":           "0.13.0",
		"GNU bash, version 5.3.0(1)-bashy-1.2.3 (x86_64)\n": "1.2.3",
		"GNU bash, version 5.3.0(1)-bashy-dev":              "dev",
		"GNU bash, version 5.3.0(1)-release":                "", // no bashy tag
		"":                                                  "",
	}
	for in, want := range cases {
		if got := parseBashyBanner(in); got != want {
			t.Errorf("parseBashyBanner(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameBashyVersion(t *testing.T) {
	same := [][2]string{{"0.13.0", "v0.13.0"}, {"v0.13.0", "0.13.0"}, {"1.2.3", "1.2.3"}}
	for _, c := range same {
		if !sameBashyVersion(c[0], c[1]) {
			t.Errorf("sameBashyVersion(%q,%q) = false, want true", c[0], c[1])
		}
	}
	if sameBashyVersion("0.13.0", "v0.12.0") {
		t.Error("sameBashyVersion(0.13.0, v0.12.0) = true, want false")
	}
}

// An explicit operator pin wins; otherwise the resolver tracks the outpost-
// carried DefaultBashyVersion (the matched pair — the auto-roll anchor).
func TestEffectiveVersion(t *testing.T) {
	r := &bashyBinaryResolver{}
	if got := r.effectiveVersion(); got != DefaultBashyVersion {
		t.Errorf("unpinned effectiveVersion = %q, want DefaultBashyVersion %q", got, DefaultBashyVersion)
	}
	r.SetVersion("latest")
	if got := r.effectiveVersion(); got != DefaultBashyVersion {
		t.Errorf("latest effectiveVersion = %q, want DefaultBashyVersion", got)
	}
	r.SetVersion("v0.12.0")
	if got := r.effectiveVersion(); got != "v0.12.0" {
		t.Errorf("pinned effectiveVersion = %q, want v0.12.0", got)
	}
}
