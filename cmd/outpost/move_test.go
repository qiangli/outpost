package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// moveRecorder captures runMove's dep calls in order so tests can assert
// the spec ordering: leave → re-pair → old-record delete.
type moveRecorder struct {
	calls      []string
	leaveErr   error
	xchgErr    error
	deleteErr  error
	oldBase    string
	oldToken   string
	oldName    string
	xchgServer string
	xchgCode   string
	xchgName   string
	// exchange writes this config to cfgPath, standing in for the merged
	// pairing doExchange persists.
	afterExchange *conf.FileConfig
}

func (r *moveRecorder) deps() moveDeps {
	return moveDeps{
		leaveCluster: func(_ context.Context, _ *conf.FileConfig, _ io.Writer) error {
			r.calls = append(r.calls, "leave")
			return r.leaveErr
		},
		exchange: func(_ context.Context, serverURL, code, name string) error {
			r.calls = append(r.calls, "exchange")
			r.xchgServer, r.xchgCode, r.xchgName = serverURL, code, name
			return r.xchgErr
		},
		deleteOld: func(_ context.Context, oldBase, oldToken, oldName string) error {
			r.calls = append(r.calls, "delete")
			r.oldBase, r.oldToken, r.oldName = oldBase, oldToken, oldName
			return r.deleteErr
		},
		restartOrStart: func(_ context.Context) error {
			r.calls = append(r.calls, "restart")
			return nil
		},
	}
}

// pairedClusteredConfig is a host paired to oldBase that is IN a cluster.
func pairedClusteredConfig(oldHost string) *conf.FileConfig {
	on := true
	return &conf.FileConfig{
		AgentName:   "worker",
		AccessToken: "old-access-token",
		ServerAddr:  oldHost,
		ServerPort:  443,
		Protocol:    "wss",
		Cluster: &conf.ClusterConfig{
			Enabled:   &on,
			Runtimes:  conf.ClusterRuntimes{Agent: true},
			NodeName:  "worker-node",
			NodeToken: "K10old::node:old",
		},
	}
}

func runMoveTest(t *testing.T, fc *conf.FileConfig, p moveParams, rec *moveRecorder) (string, error) {
	t.Helper()
	path := writeConfig(t, fc)
	// Stand in for doExchange's persistence: the fake exchange writes the
	// post-pairing config so runMove's landing report reads it.
	after := rec.afterExchange
	if after == nil {
		after = &conf.FileConfig{AgentName: p.Name, ServerAddr: "new.example.com", ServerPort: 443, Protocol: "wss"}
	}
	origDeps := rec.deps()
	deps := origDeps
	prevExchange := deps.exchange
	deps.exchange = func(ctx context.Context, serverURL, code, name string) error {
		if err := prevExchange(ctx, serverURL, code, name); err != nil {
			return err
		}
		return conf.SaveFile(path, after)
	}
	var buf bytes.Buffer
	err := runMove(context.Background(), path, fc, p, &buf, deps)
	return buf.String(), err
}

func callOrder(calls []string) string { return strings.Join(calls, ",") }

// A clustered host moves leave-first, then re-pairs, then its old record is
// deleted with the OLD credentials.
func TestRunMove_WithCluster_LeaveExchangeDeleteOrder(t *testing.T) {
	rec := &moveRecorder{}
	out, err := runMoveTest(t, pairedClusteredConfig("old.example.com"), moveParams{
		To: "https://new.example.com", Code: "invite-1", Name: "worker",
	}, rec)
	if err != nil {
		t.Fatalf("runMove failed: %v", err)
	}
	if got := callOrder(rec.calls); got != "leave,exchange,restart,delete" {
		t.Errorf("wrong dep order: %q", got)
	}
	if rec.oldBase != "https://old.example.com" || rec.oldToken != "old-access-token" || rec.oldName != "worker" {
		t.Errorf("old-record delete used wrong credentials: base=%q token=%q name=%q",
			rec.oldBase, rec.oldToken, rec.oldName)
	}
	if rec.xchgServer != "https://new.example.com" || rec.xchgCode != "invite-1" || rec.xchgName != "worker" {
		t.Errorf("exchange used wrong params: %+v", rec)
	}
	if !strings.Contains(out, "orphan") {
		t.Errorf("landing report missing orphan note: %q", out)
	}
}

// A host in no cluster skips the leave but still re-pairs and deletes.
func TestRunMove_WithoutCluster_SkipsLeave(t *testing.T) {
	rec := &moveRecorder{}
	fc := &conf.FileConfig{
		AgentName: "solo", AccessToken: "tok",
		ServerAddr: "old.example.com", ServerPort: 443, Protocol: "wss",
	}
	out, err := runMoveTest(t, fc, moveParams{
		To: "https://new.example.com", Code: "invite-1", Name: "solo",
	}, rec)
	if err != nil {
		t.Fatalf("runMove failed: %v", err)
	}
	if got := callOrder(rec.calls); got != "exchange,restart,delete" {
		t.Errorf("leave should be skipped for a clusterless host: %q", got)
	}
	if !strings.Contains(out, "not in a cluster") {
		t.Errorf("expected skip-leave note: %q", out)
	}
}

// A same-server "move" keeps its record — deleting it would remove the row
// the exchange just created.
func TestRunMove_SameServer_SkipsOldDelete(t *testing.T) {
	rec := &moveRecorder{}
	_, err := runMoveTest(t, pairedClusteredConfig("same.example.com"), moveParams{
		To: "https://same.example.com", Code: "invite-1", Name: "worker",
	}, rec)
	if err != nil {
		t.Fatalf("runMove failed: %v", err)
	}
	for _, c := range rec.calls {
		if c == "delete" {
			t.Errorf("old-record delete must be skipped on a same-server move: %q", callOrder(rec.calls))
		}
	}
}

// A failed exchange aborts before touching the old record or restarting.
func TestRunMove_ExchangeFailure_AbortsBeforeDelete(t *testing.T) {
	rec := &moveRecorder{xchgErr: errors.New("bad code")}
	_, err := runMoveTest(t, pairedClusteredConfig("old.example.com"), moveParams{
		To: "https://new.example.com", Code: "bad", Name: "worker",
	}, rec)
	if err == nil {
		t.Fatal("expected the exchange failure to abort the move")
	}
	for _, c := range rec.calls {
		if c == "delete" || c == "restart" {
			t.Errorf("no delete/restart after a failed exchange: %q", callOrder(rec.calls))
		}
	}
}

// An old server without host-credential self-delete is the expected case:
// the move still succeeds and the record is left for host prune.
func TestRunMove_OldDeleteUnsupported_StillSucceeds(t *testing.T) {
	rec := &moveRecorder{deleteErr: errHostDeleteUnsupported}
	out, err := runMoveTest(t, pairedClusteredConfig("old.example.com"), moveParams{
		To: "https://new.example.com", Code: "invite-1", Name: "worker",
	}, rec)
	if err != nil {
		t.Fatalf("unsupported old delete must not fail the move: %v", err)
	}
	if !strings.Contains(out, "prune") {
		t.Errorf("expected the prune note: %q", out)
	}
}

// A leave error warns and continues — the re-pair orphan covers the local
// state, and the operator was told to rejoin explicitly if needed.
func TestRunMove_LeaveError_WarnsAndContinues(t *testing.T) {
	rec := &moveRecorder{leaveErr: errors.New("daemon unreachable")}
	out, err := runMoveTest(t, pairedClusteredConfig("old.example.com"), moveParams{
		To: "https://new.example.com", Code: "invite-1", Name: "worker",
	}, rec)
	if err != nil {
		t.Fatalf("leave failure must not abort the move: %v", err)
	}
	if got := callOrder(rec.calls); got != "leave,exchange,restart,delete" {
		t.Errorf("move did not continue past the leave error: %q", got)
	}
	if !strings.Contains(out, "warning: cluster leave") {
		t.Errorf("expected the leave warning: %q", out)
	}
}

// deleteOldHostRecord against a fake old server: 204 deletes, 403 (the
// real cloudbox behavior for host credentials on a user-session endpoint)
// reports unsupported, 500 is a plain error. The request must carry the old
// token and the ?confirm= guard for the caller's own old name.
func TestDeleteOldHostRecord_StatusHandling(t *testing.T) {
	var gotAuth, gotConfirm, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotConfirm = r.URL.Query().Get("confirm")
		switch r.URL.Path {
		case "/api/hosts/gone":
			w.WriteHeader(http.StatusNoContent)
		case "/api/hosts/denied":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	if err := deleteOldHostRecord(ctx, srv.URL, "old-token", "gone"); err != nil {
		t.Errorf("204 must mean deleted: %v", err)
	}
	if gotAuth != "Bearer old-token" || gotPath != "/api/hosts/gone" || gotConfirm != "gone" {
		t.Errorf("wrong delete request: auth=%q path=%q confirm=%q", gotAuth, gotPath, gotConfirm)
	}
	if err := deleteOldHostRecord(ctx, srv.URL, "old-token", "denied"); !errors.Is(err, errHostDeleteUnsupported) {
		t.Errorf("403 must report unsupported, got: %v", err)
	}
	if err := deleteOldHostRecord(ctx, srv.URL, "old-token", "boom"); errors.Is(err, errHostDeleteUnsupported) {
		t.Errorf("500 must be a plain error, got: %v", err)
	}
}

// S498.4 at the merge surface, second half: a host that opted into cluster
// mode but never held membership (e.g. `builtins set --cluster on` with no
// join yet) must NOT gain the new server's membership on a move — that path
// is exactly how a third-party host re-paired to another owner's satellite
// silently joined its cluster.
func TestMergePairingServerChange_OptedInWithoutMembership_LandsOrphan(t *testing.T) {
	on := true
	path := writeConfig(t, &conf.FileConfig{
		AgentName:  "optin",
		ServerAddr: "old.example.com",
		ServerPort: 443,
		Protocol:   "wss",
		Cluster:    &conf.ClusterConfig{Enabled: &on},
	})
	merged := mergePairing(path, exchangedFrom("new.example.com"))
	if merged.ClusterOn() {
		t.Error("opted-in host stayed enabled across a server change (auto-join)")
	}
	cc := merged.Cluster
	if cc.NodeToken != "" || cc.STCPSecret != "" || cc.OverlayLoginServer != "" {
		t.Errorf("new server's membership was issued without an explicit join: %+v", cc)
	}
}

// S498.4 at the boot surface: the reattach must not refresh membership onto
// a host whose cluster mode is off, while an enabled host still refreshes
// (the explicit-join path).
func TestReconcileReattach_ClusterOff_SkipsMembershipRefresh(t *testing.T) {
	off := false
	fc := &conf.FileConfig{AgentName: "orphan", Cluster: &conf.ClusterConfig{Enabled: &off}}
	refreshed := &conf.FileConfig{Cluster: &conf.ClusterConfig{
		NodeToken: "K10cloud::node:new", STCPSecret: "s",
		OverlayLoginServer: "https://new.example.com/overlay",
	}}
	if changed := reconcileClusterMembershipOnReattach(fc, refreshed); changed {
		t.Error("reattach must not touch an orphaned host's cluster block")
	}
	if fc.Cluster.NodeToken != "" || fc.Cluster.STCPSecret != "" || fc.Cluster.OverlayLoginServer != "" {
		t.Errorf("membership leaked onto an orphan: %+v", fc.Cluster)
	}

	on := true
	joined := &conf.FileConfig{AgentName: "member", Cluster: &conf.ClusterConfig{Enabled: &on}}
	if !reconcileClusterMembershipOnReattach(joined, refreshed) {
		t.Error("reattach must still refresh an explicitly joined host")
	}
	if joined.Cluster.NodeToken != "K10cloud::node:new" {
		t.Errorf("joined host lost its refresh: %+v", joined.Cluster)
	}
}
