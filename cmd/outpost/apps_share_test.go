package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/cloudboxfake"
	"github.com/spf13/cobra"
)

func TestShareTargetSelection(t *testing.T) {
	if _, err := shareTargetFromConfig(&conf.FileConfig{}); err == nil {
		t.Fatal("unpaired accepted")
	}
	got, err := shareTargetFromConfig(&conf.FileConfig{AgentName: "old", PortalURL: "https://portal.example", AccessToken: "legacy"})
	if err != nil || got.host != "old" || got.token != "legacy" || got.endpoint != "https://portal.example" {
		t.Fatalf("target=%+v err=%v", got, err)
	}
	if _, err := shareTargetFromConfig(&conf.FileConfig{AgentName: "old", AccessToken: "legacy", Attachments: []conf.SatelliteAttachment{{Host: "new"}}}); err == nil {
		t.Fatal("incomplete attachment silently fell back")
	}
}

func TestAppsShareRequiresAudience(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"notes"}, "at least one"},
		{[]string{"notes", "--circle", " "}, "at least one"},
		{[]string{"notes", "--circle", " ", "--group", "support"}, "--circle requires a non-empty value"},
	} {
		cmd := appsShareCmd()
		cmd.SetArgs(tc.args)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("args %v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestAppsShareGrantRule(t *testing.T) {
	fake := cloudboxfake.New(t)
	owner := fake.AddOwner("owner@example.test")
	post := func(path, token, body string, status int, result any) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fake.URL()+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := fake.Master.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != status {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST %s: status %d, want %d: %s", path, resp.StatusCode, status, data)
		}
		if result != nil {
			if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
				t.Fatal(err)
			}
		}
	}
	var invite struct{ Code string }
	post("/api/v1/hosts/invites", owner, "", http.StatusCreated, &invite)
	pair, err := json.Marshal(map[string]string{"code": invite.Code, "host": "attached"})
	if err != nil {
		t.Fatal(err)
	}
	post("/api/v1/hosts/pair", "", string(pair), http.StatusCreated, nil)
	for _, member := range []struct{ email, circle string }{
		{"first@example.test", "family"},
		{"second@example.test", "family"},
		{"other@example.test", "guest"},
	} {
		token := fake.AddOwner(member.email)
		var invited struct{ Invitation struct{ ID string } }
		body, err := json.Marshal(map[string]string{"email": member.email, "circle": member.circle})
		if err != nil {
			t.Fatal(err)
		}
		post("/api/v1/members", owner, string(body), http.StatusCreated, &invited)
		post("/api/v1/member/invitations/"+invited.Invitation.ID+"/accept", token, "", http.StatusOK, nil)
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := conf.DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	fc := &conf.FileConfig{
		AgentName: "legacy", PortalURL: "http://invalid.example", AccessToken: "legacy-token",
		Attachments: []conf.SatelliteAttachment{{Endpoint: fake.URL(), Host: "attached", Token: owner}},
	}
	if err := conf.SaveFile(path, fc); err != nil {
		t.Fatal(err)
	}
	run := func(cmd *cobra.Command, args ...string) string {
		t.Helper()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		cmd.SilenceErrors = true
		cmd.SilenceUsage = true
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	decode := func(data string, value any) {
		t.Helper()
		if err := json.Unmarshal([]byte(data), value); err != nil {
			t.Fatalf("decode %q: %v", data, err)
		}
	}
	var created struct{ Share cloudboxfake.Share }
	decode(run(appsCmd(), "share", "notes", "--circle", "family", "--json"), &created)
	row := created.Share
	if row.ID == "" || row.Host != "attached" || row.App != "notes" || row.Created.IsZero() ||
		!reflect.DeepEqual(row.Circles, []string{"family"}) ||
		!reflect.DeepEqual(row.Groups, []string{}) || !reflect.DeepEqual(row.Emails, []string{}) {
		t.Fatalf("created grant = %+v", row)
	}

	who := func(flags ...string) string {
		t.Helper()
		args := []string{"attached/notes", "--master", fake.URL(), "--token", owner}
		return run(fleet.NewShareWhoCmd(), append(args, flags...)...)
	}
	const wantWho = "first@example.test\nsecond@example.test\n"
	if got := who(); got != wantWho {
		t.Fatalf("who = %q, want %q", got, wantWho)
	}
	var admitted struct{ Emails []string }
	decode(who("--json"), &admitted)
	if !reflect.DeepEqual(admitted.Emails, []string{"first@example.test", "second@example.test"}) {
		t.Fatalf("who --json emails = %v", admitted.Emails)
	}

	list := func(app string) []cloudboxfake.Share {
		t.Helper()
		var listed struct{ Shares []cloudboxfake.Share }
		decode(run(appsCmd(), "shares", app, "--json"), &listed)
		return listed.Shares
	}
	if got := list("notes"); !reflect.DeepEqual(got, []cloudboxfake.Share{row}) {
		t.Fatalf("listed grants = %+v, want %+v", got, row)
	}
	if got := run(appsCmd(), "shares", "notes"); !strings.Contains(got, row.ID) || !strings.Contains(got, "family") {
		t.Fatalf("text listing = %q", got)
	}

	var dryDelete struct {
		DryRun bool `json:"dry_run"`
		ID     string
		Method string
	}
	decode(run(appsCmd(), "unshare", row.ID, "--dry-run", "--json"), &dryDelete)
	if !dryDelete.DryRun || dryDelete.ID != row.ID || dryDelete.Method != http.MethodDelete || who() != wantWho {
		t.Fatalf("dry-run unshare = %+v", dryDelete)
	}
	var removed struct {
		OK bool
		ID string
	}
	decode(run(appsCmd(), "unshare", row.ID, "--json"), &removed)
	if !removed.OK || removed.ID != row.ID {
		t.Fatalf("unshare = %+v", removed)
	}
	if got := list("notes"); !reflect.DeepEqual(got, []cloudboxfake.Share{}) {
		t.Fatalf("grants after unshare = %+v", got)
	}
	if got := who(); got != "" {
		t.Fatalf("who after unshare = %q", got)
	}

	decode(run(appsCmd(), "share", "notes", "--group", "support", "--json"), &created)
	if !reflect.DeepEqual(created.Share.Groups, []string{"support"}) ||
		!reflect.DeepEqual(created.Share.Circles, []string{}) || !reflect.DeepEqual(created.Share.Emails, []string{}) {
		t.Fatalf("group-only grant = %+v", created.Share)
	}

	audience := []string{"--circle", "family", "--circle", "guest", "--group", "support", "--group", "staff", "--email", "first@example.test", "--email", "other@example.test"}
	var plan struct {
		DryRun bool `json:"dry_run"`
		cloudboxfake.Share
	}
	args := append([]string{"share", "board", "--json", "--dry-run"}, audience...)
	decode(run(appsCmd(), args...), &plan)
	if !plan.DryRun || plan.App != "board" ||
		!reflect.DeepEqual(plan.Circles, []string{"family", "guest"}) ||
		!reflect.DeepEqual(plan.Groups, []string{"support", "staff"}) ||
		!reflect.DeepEqual(plan.Emails, []string{"first@example.test", "other@example.test"}) {
		t.Fatalf("dry-run rule = %+v", plan)
	}
	if got := list("board"); len(got) != 0 {
		t.Fatalf("dry-run created grants: %+v", got)
	}
	args = append([]string{"share", "board", "--json"}, audience...)
	decode(run(appsCmd(), args...), &created)
	if got := list("board"); !reflect.DeepEqual(got, []cloudboxfake.Share{created.Share}) ||
		!reflect.DeepEqual(created.Share.Circles, plan.Circles) ||
		!reflect.DeepEqual(created.Share.Groups, plan.Groups) ||
		!reflect.DeepEqual(created.Share.Emails, plan.Emails) {
		t.Fatalf("created grant = %+v, listed grants = %+v, plan = %+v", created.Share, got, plan)
	}
}
