// Copyright (c) 2026, the outpost authors
// See LICENSE for licensing information

package main

import (
	"strings"
	"testing"
)

func TestValidateLabel(t *testing.T) {
	cases := []struct {
		name, in string
		wantErr  bool
	}{
		{"ok-simple", "kg3-pipeline", false},
		{"ok-dots", "my.app.v2", false},
		{"empty", "", true},
		{"with-slash", "kg3/pipeline", true},
		{"with-backslash", `kg3\pipeline`, true},
		{"with-space", "kg3 pipeline", true},
		{"with-tab", "kg3\tpipeline", true},
		{"with-newline", "kg3\npipeline", true},
		{"too-long", strings.Repeat("a", 81), true},
		{"ok-max-len", strings.Repeat("a", 80), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLabel(tc.in)
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Errorf("validateLabel(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
		})
	}
}

func TestRenderPlist_Shape(t *testing.T) {
	out, err := renderPlist(runSpec{
		Args:         []string{"/usr/local/bin/myprog", "--flag", "value"},
		WorkDir:      "/Users/me",
		KeepAlive:    true,
		ThrottleSecs: 30,
		StdoutPath:   "/tmp/out.log",
		StderrPath:   "/tmp/err.log",
	}, "outpost.run.kg3")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	mustContain := []string{
		`<key>Label</key><string>outpost.run.kg3</string>`,
		`<string>/usr/local/bin/myprog</string>`,
		`<string>--flag</string>`,
		`<string>value</string>`,
		`<key>WorkingDirectory</key><string>/Users/me</string>`,
		`<key>KeepAlive</key><true/>`,
		`<key>ThrottleInterval</key><integer>30</integer>`,
		`<key>StandardOutPath</key><string>/tmp/out.log</string>`,
		`<key>StandardErrorPath</key><string>/tmp/err.log</string>`,
	}
	for _, m := range mustContain {
		if !strings.Contains(s, m) {
			t.Errorf("plist missing %q\n--- output ---\n%s", m, s)
		}
	}
}

// TestRenderPlist_EscapesPathSpecials proves html/template escapes
// XML metacharacters in argv — otherwise a path containing `&` or `<`
// would produce invalid plist XML that launchctl rejects.
func TestRenderPlist_EscapesPathSpecials(t *testing.T) {
	out, err := renderPlist(runSpec{
		Args:    []string{"/bin/echo", "<bad>", "a&b"},
		WorkDir: "/tmp",
	}, "outpost.run.x")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "<bad>") {
		t.Errorf("plist contains raw `<bad>` — argv was not escaped\n%s", s)
	}
	if !strings.Contains(s, "&lt;bad&gt;") {
		t.Errorf("plist should contain escaped `&lt;bad&gt;`\n%s", s)
	}
	if !strings.Contains(s, "a&amp;b") {
		t.Errorf("plist should contain escaped `a&amp;b`\n%s", s)
	}
}

// TestRenderPlist_XMLPreamble locks in that the leading "<?xml" is
// emitted verbatim. An earlier draft used html/template, which
// escaped "<?xml" to "&lt;?xml" and produced a plist macOS launchd
// rejected with "Bootstrap failed: 5: Input/output error". This test
// is the regression guard.
func TestRenderPlist_XMLPreamble(t *testing.T) {
	out, err := renderPlist(runSpec{
		Args:    []string{"/bin/true"},
		WorkDir: "/tmp",
	}, "outpost.run.z")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if !strings.HasPrefix(s, `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Errorf("plist must start with literal XML processing instruction; got:\n%s", s[:80])
	}
	if strings.Contains(s, "&lt;?xml") {
		t.Errorf("plist must NOT escape its leading <?xml; got:\n%s", s[:80])
	}
}

// TestRenderPlist_OmitsOptional confirms that StandardOut/ErrorPath
// keys are dropped entirely when not set, so we don't generate
// launchd plists that point at empty paths.
func TestRenderPlist_OmitsOptional(t *testing.T) {
	out, err := renderPlist(runSpec{
		Args:    []string{"/bin/true"},
		WorkDir: "/tmp",
	}, "outpost.run.y")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "StandardOutPath") {
		t.Errorf("plist should omit StandardOutPath when unset\n%s", s)
	}
	if strings.Contains(s, "StandardErrorPath") {
		t.Errorf("plist should omit StandardErrorPath when unset\n%s", s)
	}
	if !strings.Contains(s, "<key>KeepAlive</key><false/>") {
		t.Errorf("plist should set KeepAlive=false when not requested\n%s", s)
	}
}
