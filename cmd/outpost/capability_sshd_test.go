package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/discovery"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/svcunit"
)

// fakeSSHDFirewall records Ensure/Remove without touching the OS.
type fakeSSHDFirewall struct {
	mu        sync.Mutex
	ensures   []int
	removes   int
	ensureErr error
}

func (f *fakeSSHDFirewall) Ensure(port int, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures = append(f.ensures, port)
	return f.ensureErr
}

func (f *fakeSSHDFirewall) Remove() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removes++
	return nil
}

// fakeSSHDAdvertiser records the advertised endpoint + close.
type fakeSSHDAdvertiser struct {
	mu     sync.Mutex
	opts   []discovery.AdvertiseOptions
	closes int
}

func (f *fakeSSHDAdvertiser) advertise(_ context.Context, opts discovery.AdvertiseOptions) (sshdAdvertiser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opts = append(f.opts, opts)
	return f, nil
}

func (f *fakeSSHDAdvertiser) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func testSSHDHostKey(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func testSSHDClientKey(t *testing.T) (line string, signer ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err = ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
	return line, signer
}

// newTestSSHDCapability builds the unit with loopback bind, a fake
// firewall and a recording advertiser.
func newTestSSHDCapability(signer ssh.Signer, fw *fakeSSHDFirewall, adv *fakeSSHDAdvertiser) *sshdCapability {
	return &sshdCapability{
		Daemon: sshdDaemonDeps{
			HostKeyLoader: func() (ssh.Signer, error) { return signer, nil },
			SFTP:          true,
			AllowLocal:    true,
			AllowRemote:   true,
			AllowAgent:    true,
			AgentName:     "testhost",
		},
		Firewall:  fw,
		Advertise: adv.advertise,
		BindHost:  "127.0.0.1",
	}
}

func sshdTestConfig(port int, ak string) service.Config {
	return service.Config{
		Unit:     "sshd",
		Settings: map[string]string{"port": fmt.Sprintf("%d", port), "authorized_keys": ak},
		Ports:    []fleet.ServicePort{{Name: "ssh", Port: port}},
	}
}

// Key auth over a real listener: an authorized key is accepted (BatchMode
// shape — no password, no prompt), an unknown key is rejected, and the
// wrong OS user is rejected.
func TestSSHDKeyAuthAcceptReject(t *testing.T) {
	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Fatalf("CurrentUser: %v", err)
	}
	signer := testSSHDHostKey(t)
	line, clientSigner := testSSHDClientKey(t)
	_, otherSigner := testSSHDClientKey(t)

	ak := filepath.Join(t.TempDir(), "sshd_authorized_keys")
	if _, err := conf.AddSSHAuthorizedKey(ak, line); err != nil {
		t.Fatal(err)
	}

	fw, adv := &fakeSSHDFirewall{}, &fakeSSHDAdvertiser{}
	cap := newTestSSHDCapability(signer, fw, adv)
	port := freePort(t)
	if err := cap.Activate(context.Background(), sshdTestConfig(port, ak)); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	defer cap.Deactivate(context.Background())

	dial := func(user string, auth ssh.AuthMethod) (*ssh.Client, error) {
		cfg := &ssh.ClientConfig{
			User:            user,
			Auth:            []ssh.AuthMethod{auth},
			HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()),
			Timeout:         10 * time.Second,
		}
		return ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), cfg)
	}

	// Accept: the authorized key, no password offered (BatchMode shape).
	cli, err := dial(currentUser, ssh.PublicKeys(clientSigner))
	if err != nil {
		t.Fatalf("dial with authorized key: %v", err)
	}
	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	out, err := sess.Output("echo key-auth-ok")
	_ = sess.Close()
	_ = cli.Close()
	if err != nil || strings.TrimSpace(string(out)) != "key-auth-ok" {
		t.Fatalf("exec = %q, %v; want key-auth-ok", out, err)
	}

	// Reject: a well-formed but unauthorized key.
	if _, err := dial(currentUser, ssh.PublicKeys(otherSigner)); err == nil {
		t.Error("dial with unknown key should fail")
	}

	// Reject: a valid key for the wrong OS user.
	if _, err := dial("definitely-not-"+currentUser, ssh.PublicKeys(clientSigner)); err == nil {
		t.Error("dial as the wrong user should fail")
	}

	// Removing the key revokes access immediately (no restart).
	fp := ssh.FingerprintSHA256(clientSigner.PublicKey())
	if ok, err := conf.RemoveSSHAuthorizedKey(ak, fp); err != nil || !ok {
		t.Fatalf("rm key: %v %v", ok, err)
	}
	if _, err := dial(currentUser, ssh.PublicKeys(clientSigner)); err == nil {
		t.Error("dial after key removal should fail")
	}
}

// Activate binds the listener, ensures the firewall rule and advertises
// the lan-ssh endpoint; Deactivate withdraws all three.
func TestSSHDActivateDeactivatePublishesWithdraws(t *testing.T) {
	signer := testSSHDHostKey(t)
	ak := filepath.Join(t.TempDir(), "sshd_authorized_keys")
	fw, adv := &fakeSSHDFirewall{}, &fakeSSHDAdvertiser{}
	cap := newTestSSHDCapability(signer, fw, adv)
	port := freePort(t)

	if err := cap.Activate(context.Background(), sshdTestConfig(port, ak)); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if got := cap.boundPort(); got != port {
		t.Fatalf("bound port = %d, want %d", got, port)
	}
	// Listener serves (SSH banner pre-auth).
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial listener: %v", err)
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	_ = conn.Close()
	if !strings.HasPrefix(string(buf[:n]), "SSH-2.0-") {
		t.Fatalf("banner = %q, want SSH-2.0-", buf[:n])
	}
	// Firewall ensured for the declared port.
	fw.mu.Lock()
	ensured := append([]int(nil), fw.ensures...)
	fw.mu.Unlock()
	if len(ensured) != 1 || ensured[0] != port {
		t.Fatalf("firewall ensures = %v, want [%d]", ensured, port)
	}
	// mDNS advertises the plain-TCP lan-ssh endpoint.
	adv.mu.Lock()
	adverts := append([]discovery.AdvertiseOptions(nil), adv.opts...)
	adv.mu.Unlock()
	if len(adverts) != 1 {
		t.Fatalf("advertisements = %d, want 1", len(adverts))
	}
	if adverts[0].Port != port || !strings.Contains(adverts[0].SSHListenAddr, fmt.Sprintf("%d", port)) {
		t.Errorf("advertised endpoint = %+v, want lan-ssh on %d", adverts[0], port)
	}

	if err := cap.Deactivate(context.Background()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if got := cap.boundPort(); got != 0 {
		t.Fatalf("bound port after deactivate = %d, want 0", got)
	}
	if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		_ = c.Close()
		t.Error("listener still accepts after deactivate")
	}
	fw.mu.Lock()
	removes := fw.removes
	fw.mu.Unlock()
	if removes != 1 {
		t.Errorf("firewall removes = %d, want 1", removes)
	}
	adv.mu.Lock()
	closes := adv.closes
	adv.mu.Unlock()
	if closes != 1 {
		t.Errorf("advertiser closes = %d, want 1", closes)
	}
	// Deactivate is idempotent.
	if err := cap.Deactivate(context.Background()); err != nil {
		t.Errorf("second Deactivate: %v", err)
	}
}

// A refusing firewall does not stop the listener: Activate reports the
// degraded condition (exact reason) while still serving.
func TestSSHDegradedFirewallStillServes(t *testing.T) {
	signer := testSSHDHostKey(t)
	ak := filepath.Join(t.TempDir(), "sshd_authorized_keys")
	fw := &fakeSSHDFirewall{ensureErr: errors.New("Access is denied.")}
	adv := &fakeSSHDAdvertiser{}
	cap := newTestSSHDCapability(signer, fw, adv)
	port := freePort(t)

	err := cap.Activate(context.Background(), sshdTestConfig(port, ak))
	var deg *svcunit.DegradedError
	if !errors.As(err, &deg) {
		t.Fatalf("Activate = %v, want a degraded condition", err)
	}
	if !strings.Contains(err.Error(), "Access is denied") {
		t.Fatalf("Activate = %q, want the exact firewall reason", err)
	}
	// The listener still serves despite the degraded firewall.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	c, derr := net.DialTimeout("tcp", addr, 5*time.Second)
	if derr != nil {
		t.Fatalf("listener down while degraded: %v", derr)
	}
	_ = c.Close()
	// Health reports the same degraded condition (the reconciler maps it
	// to state degraded instead of cycling).
	if herr := cap.Health(context.Background()); !errors.As(herr, &deg) {
		t.Fatalf("Health = %v, want degraded", herr)
	}
	// Healing the firewall heals the unit with no rebind.
	fw.mu.Lock()
	fw.ensureErr = nil
	fw.mu.Unlock()
	if err := cap.Health(context.Background()); err != nil {
		t.Fatalf("Health after heal = %v, want nil", err)
	}
	_ = cap.Deactivate(context.Background())
}

// Port clashes refuse activation with the reason (the unit reports failed,
// never half-bound).
func TestSSHDActivatePortClash(t *testing.T) {
	signer := testSSHDHostKey(t)
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	clash := held.Addr().(*net.TCPAddr).Port
	ak := filepath.Join(t.TempDir(), "sshd_authorized_keys")
	cap := newTestSSHDCapability(signer, &fakeSSHDFirewall{}, &fakeSSHDAdvertiser{})
	if err := cap.Activate(context.Background(), sshdTestConfig(clash, ak)); err == nil {
		t.Error("Activate on a held port should fail")
		_ = cap.Deactivate(context.Background())
	}
	if got := cap.boundPort(); got != 0 {
		t.Errorf("bound port = %d after failed activate, want 0", got)
	}
}

// ensureSSHDUnit seeds the declaration once: enabled by default on 2222,
// and never touches an existing declaration (an operator disable stays).
func TestEnsureSSHDUnit(t *testing.T) {
	store := testUnitStore(t, nil)
	ensureSSHDUnit(store)
	u, ok := store.Unit("sshd")
	if !ok {
		t.Fatal("sshd unit not declared after ensure")
	}
	if !u.Enabled {
		t.Error("sshd unit not enabled by default")
	}
	if u.ProviderName() != "internal" || u.Capability != capSSHD || u.Owner != "outpost" {
		t.Errorf("sshd unit = provider %q capability %q owner %q", u.ProviderName(), u.Capability, u.Owner)
	}
	var port int
	for _, p := range u.Ports {
		if p.Port > 0 {
			port = p.Port
		}
	}
	if port != DefaultSSHDPort {
		t.Errorf("sshd port = %d, want %d", port, DefaultSSHDPort)
	}
	// Disable, then ensure again: the operator's choice stands.
	u.Enabled = false
	if err := store.Save(u); err != nil {
		t.Fatal(err)
	}
	ensureSSHDUnit(store)
	if u, _ := store.Unit("sshd"); u.Enabled {
		t.Error("ensure re-enabled an operator-disabled unit")
	}
	// Missing store never panics.
	ensureSSHDUnit(nil)
}

// The capability registers under outpost.sshd.
func TestOutpostCapabilitiesIncludesSSHD(t *testing.T) {
	caps := outpostCapabilities(capabilityDeps{SSHD: &sshdCapability{}})
	if _, ok := caps[capSSHD]; !ok {
		t.Errorf("capability %q is not registered", capSSHD)
	}
}
