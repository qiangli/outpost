package main

import (
	"context"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/otel"
	"github.com/qiangli/outpost/internal/agent/shard"
	"github.com/qiangli/outpost/internal/agent/svcunit"
)

// cfgOf builds the declaration projection a capability receives.
func cfgOf(unit string, settings map[string]string, ports ...fleet.ServicePort) service.Config {
	return service.Config{Unit: unit, Settings: settings, Ports: ports}
}

// Every in-process surface the retained-coverage inventory names must bind to
// a capability id, or an enabled unit declaring it reports "capability is not
// registered" on a daemon that does in fact host the subsystem.
//
// The ids are a cross-repo contract with the unit seeds (yoke S17), so this
// is the assertion that keeps outpost answerable for its half of it.
func TestOutpostCapabilitiesCoverEveryRetainedSurface(t *testing.T) {
	apps := agent.NewAppRegistry()
	caps := outpostCapabilities(capabilityDeps{
		Files:        &filesCapability{Apps: apps},
		Podman:       &podmanCapability{Apps: apps},
		Sandbox:      &sandboxCapability{Apps: apps},
		Ollama:       &ollamaCapability{Apps: apps},
		OllamaPool:   &ollamaPoolCapability{},
		Warm:         &warmCapability{},
		Shard:        &shardCapability{},
		LANInference: &lanInferenceCapability{},
		Otel:         &otelCapability{Apps: apps},
		Ycode:        &ycodeCapability{Apps: apps},
	})
	for _, id := range []string{
		capFiles, capPodman, capSandbox, capOllama, capOllamaPool,
		capWarm, capShard, capLANInference, capOtel, capYcode,
		capActrunner, capActrunnerAlias,
	} {
		if _, ok := caps[id]; !ok {
			t.Errorf("capability %q is not registered", id)
		}
	}
	// And the registry accepts the whole wiring — a duplicate id would make
	// the daemon boot without its internal provider.
	if _, err := svcunit.Hosted(caps); err != nil {
		t.Fatalf("svcunit.Hosted rejected the wiring: %v", err)
	}
}

// A capability the daemon could not build is simply absent, so its unit
// reports the truth instead of the daemon pretending to host it.
func TestOutpostCapabilitiesSkipsUnbuiltSubsystems(t *testing.T) {
	caps := outpostCapabilities(capabilityDeps{})
	for _, id := range []string{capFiles, capPodman, capOllama, capShard} {
		if _, ok := caps[id]; ok {
			t.Errorf("capability %q registered with no implementation behind it", id)
		}
	}
	if _, ok := caps[capActrunner]; !ok {
		t.Error("the CI capability needs no daemon-resolved object and must always register")
	}
}

// capabilityEnabled is the replacement for conf.ShardOn()'s mesh/peer-plane
// cascade: the daemon brings the libp2p mesh and the peer plane up when an
// ENABLED unit declares the shard capability.
//
// It keys off the capability id, never the unit name — the name belongs to
// the operator — and it ignores process units and disabled or retired ones.
func TestCapabilityEnabledKeysOffTheCapabilityID(t *testing.T) {
	internalUnit := func(name, capID string, enabled bool) fleet.Service {
		return fleet.Service{
			Name: name, Kind: fleet.KindService,
			Provider: fleet.ServiceProviderInternal, Capability: capID,
			Owner: fleet.ServiceOwnerOutpost, Enabled: enabled,
		}
	}
	for _, tc := range []struct {
		name  string
		units []fleet.Service
		want  bool
	}{
		{"no units", nil, false},
		{"enabled, operator-chosen unit name", []fleet.Service{
			internalUnit("sharding-plane", capShard, true)}, true},
		{"declared but disabled", []fleet.Service{
			internalUnit("shard", capShard, false)}, false},
		{"a different capability", []fleet.Service{
			internalUnit("warm", capWarm, true)}, false},
		{"a process unit that merely has the name", []fleet.Service{{
			Name: "shard", Kind: fleet.KindService, Exec: []string{"shard"}, Enabled: true}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &svcunit.Store{Catalog: catalogWithUnits(t, tc.units...)}
			if got := capabilityEnabled(st, capShard); got != tc.want {
				t.Errorf("capabilityEnabled = %v, want %v", got, tc.want)
			}
		})
	}
	if capabilityEnabled(nil, capShard) {
		t.Error("a nil store must not report a capability as wanted")
	}
}

// catalogWithUnits builds a local-store-only fleet catalog holding units.
func catalogWithUnits(t *testing.T, units ...fleet.Service) *fleet.Catalog {
	t.Helper()
	cat := fleet.New(
		fleet.WithRoot(t.TempDir()),
		fleet.WithBaselineFS(emptyBaseline{}),
		fleet.WithoutCloudOverlay(),
	)
	for _, u := range units {
		if u.Kind == "" {
			u.Kind = fleet.KindService
		}
		if err := cat.SaveService(u); err != nil {
			t.Fatalf("seed unit %q: %v", u.Name, err)
		}
	}
	return cat
}

// The File Browser capability binds its own loopback listener and publishes
// the `files` app route with the facets no declaration can express — the
// index path and the per-app login gate — then withdraws both on deactivate.
func TestFilesCapabilityPublishesAndWithdraws(t *testing.T) {
	apps := agent.NewAppRegistry()
	scope := t.TempDir()
	cap := &filesCapability{Apps: apps}
	cfg := cfgOf("files", map[string]string{"scope": scope, "allow_write": "true"})
	if err := cap.Activate(context.Background(), cfg); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { _ = cap.Deactivate(context.Background()) })

	target := apps.LookupTarget(agent.BuiltinFiles)
	if target == nil {
		t.Fatal("the files app route was not published")
	}
	if !strings.HasPrefix(target.Host, "127.0.0.1:") {
		t.Errorf("files target = %s, want a loopback listener", target)
	}
	var entry agent.AppEntry
	for _, e := range apps.Entries() {
		if e.Name == agent.BuiltinFiles {
			entry = e
		}
	}
	if !entry.RequireLogin {
		t.Error("the files route must require login")
	}
	if entry.IndexPath != "/" {
		t.Errorf("files index_path = %q, want /", entry.IndexPath)
	}
	if err := cap.Health(context.Background()); err != nil {
		t.Errorf("Health on an active mount: %v", err)
	}
	// The handler actually answers on the bound port.
	resp, err := http.Get("http://" + target.Host + "/")
	if err != nil {
		t.Fatalf("GET the mount: %v", err)
	}
	_ = resp.Body.Close()

	if err := cap.Deactivate(context.Background()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if apps.LookupTarget(agent.BuiltinFiles) != nil {
		t.Error("the files app route survived deactivation")
	}
	if err := cap.Health(context.Background()); err == nil {
		t.Error("Health must report a deactivated mount as not serving")
	}
}

// A declared port is honoured, so the File Browser's listener is checked for
// conflicts by fleet like any other declared listener.
func TestFilesCapabilityHonoursADeclaredPort(t *testing.T) {
	apps := agent.NewAppRegistry()
	cap := &filesCapability{Apps: apps}
	// Bind an ephemeral port first to learn a free one, then release it.
	probe := httptest.NewServer(http.NotFoundHandler())
	addr := probe.Listener.Addr().String()
	probe.Close()
	_, portStr, _ := strings.Cut(addr, ":")
	cfg := service.Config{
		Unit:     "files",
		Settings: map[string]string{"scope": t.TempDir()},
		Ports:    []fleet.ServicePort{{Port: atoiOrZero(portStr)}},
	}
	if err := cap.Activate(context.Background(), cfg); err != nil {
		t.Skipf("the probed port was taken again: %v", err)
	}
	t.Cleanup(func() { _ = cap.Deactivate(context.Background()) })
	target := apps.LookupTarget(agent.BuiltinFiles)
	if target == nil || target.Host != "127.0.0.1:"+portStr {
		t.Errorf("files target = %v, want the declared port %s", target, portStr)
	}
}

// The sandbox policy comes from the declaration, and the ESCAPE knobs are not
// declarable at all: no setting re-enables privileged mode, host namespaces,
// host binds outside the scratch prefix, added capabilities or device
// passthrough.
func TestSandboxPolicyFromDeclaration(t *testing.T) {
	cfg := cfgOf("sandbox", map[string]string{
		"max_memory_mb":  "2048",
		"cpus":           "1.5",
		"pids_limit":     "256",
		"max_containers": "4",
		"allowed_images": "alpine:3, ghcr.io/acme/*",
		"scratch_dir":    "/tmp/sb",
		"privileged":     "true", // an operator trying it on: must be ignored
	})
	p := sandboxPolicy(cfg)
	if p.MaxMemoryBytes != 2048*1024*1024 {
		t.Errorf("MaxMemoryBytes = %d", p.MaxMemoryBytes)
	}
	if p.NanoCPUs != int64(1.5*1e9) {
		t.Errorf("NanoCPUs = %d", p.NanoCPUs)
	}
	if p.PidsLimit != 256 || p.MaxContainers != 4 {
		t.Errorf("pids=%d containers=%d", p.PidsLimit, p.MaxContainers)
	}
	if len(p.AllowedImages) != 2 || p.AllowedImages[0] != "alpine:3" || p.AllowedImages[1] != "ghcr.io/acme/*" {
		t.Errorf("AllowedImages = %v", p.AllowedImages)
	}
	if p.ScratchHostPrefix != "/tmp/sb" {
		t.Errorf("ScratchHostPrefix = %q", p.ScratchHostPrefix)
	}
}

// The prewarm set falls back to the CONCRETE entries of the allowlist — a
// wildcard names no image that can be pulled. (Replaces the retired
// sandboxPrewarmImages(fc) behaviour, which read two agent.json keys.)
func TestSandboxPrewarmImagesFromDeclaration(t *testing.T) {
	explicit := sandboxPrewarmImages(cfgOf("sandbox", map[string]string{
		"prewarm_images": "alpine:3 busybox:1",
		"allowed_images": "ignored:1",
	}))
	if len(explicit) != 2 || explicit[0] != "alpine:3" || explicit[1] != "busybox:1" {
		t.Errorf("explicit prewarm list = %v", explicit)
	}
	fallback := sandboxPrewarmImages(cfgOf("sandbox", map[string]string{
		"allowed_images": "alpine:3,ghcr.io/acme/*,busybox:1",
	}))
	if len(fallback) != 2 || fallback[0] != "alpine:3" || fallback[1] != "busybox:1" {
		t.Errorf("fallback prewarm list = %v, want the non-wildcard allowlist entries", fallback)
	}
	if got := sandboxPrewarmImages(cfgOf("sandbox", nil)); len(got) != 0 {
		t.Errorf("no declaration should prewarm nothing, got %v", got)
	}
}

// A podman-backed mount REFUSES activation with the reason when no socket is
// detected, instead of logging one line and leaving an enabled unit looking
// healthy while it proxies nothing.
func TestPodmanTargetRefusalNamesWhatWasTried(t *testing.T) {
	_, err := resolvePodmanTarget("podman", func() agent.BuiltinTarget {
		return agent.BuiltinTarget{Socket: "/run/podman/podman.sock"}
	}, "")
	if err == nil {
		t.Fatal("an undetected podman must refuse activation")
	}
	if !strings.Contains(err.Error(), "/run/podman/podman.sock") {
		t.Errorf("the refusal does not name what was tried: %v", err)
	}
	if !strings.Contains(err.Error(), "settings.socket") {
		t.Errorf("the refusal does not name the operator's override: %v", err)
	}
	// An operator-declared socket wins and carries the transport the PATH
	// implies — never a hardcoded "unix", or a Windows named pipe would be
	// registered as an unreachable unix socket.
	bt, err := resolvePodmanTarget("podman", nil, "/tmp/custom.sock")
	if err != nil {
		t.Fatalf("an explicit socket must be accepted: %v", err)
	}
	if bt.Socket != "/tmp/custom.sock" || bt.Scheme == "" {
		t.Errorf("explicit socket target = %+v", bt)
	}
}

// The Ollama proxy publishes the mount the pool router discovers hosts by,
// and withdraws it (plus its proxy wrap) when the unit stops.
func TestOllamaCapabilityPublishesTheLLMMount(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer daemon.Close()

	apps := agent.NewAppRegistry()
	cap := &ollamaCapability{Apps: apps}
	cfg := cfgOf("ollama", map[string]string{"url": daemon.URL})
	if err := cap.Activate(context.Background(), cfg); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if cap.Target() != daemon.URL {
		t.Errorf("Target() = %q, want %q", cap.Target(), daemon.URL)
	}
	if cap.Service() == nil {
		t.Fatal("the counter/capacity service was not created")
	}
	var entry agent.AppEntry
	for _, e := range apps.Entries() {
		if e.Name == agent.BuiltinOllama {
			entry = e
		}
	}
	if entry.Capabilities == nil || entry.Capabilities.Type != "llm" {
		t.Errorf("the ollama mount is missing the llm capability descriptor: %+v", entry.Capabilities)
	}
	if _, ok := cap.Status(); !ok {
		t.Error("Status() should report a live view while the unit is active")
	}
	if err := cap.Health(context.Background()); err != nil {
		t.Errorf("Health against a live daemon: %v", err)
	}

	// A daemon that went away takes the route with it rather than leaving
	// cloudbox routing inference at a host that cannot serve it.
	daemon.Close()
	if err := cap.Health(context.Background()); err == nil {
		t.Error("Health must fail once the daemon stops answering")
	}
	if err := cap.Deactivate(context.Background()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if apps.LookupTarget(agent.BuiltinOllama) != nil {
		t.Error("the ollama route survived deactivation")
	}
	if _, ok := cap.Status(); ok {
		t.Error("Status() should report nothing once the unit is down")
	}
}

func TestOllamaCapabilityRefusesWithoutADaemon(t *testing.T) {
	cap := &ollamaCapability{
		Apps:   agent.NewAppRegistry(),
		Detect: func() agent.BuiltinTarget { return agent.BuiltinTarget{} },
	}
	err := cap.Activate(context.Background(), cfgOf("ollama", nil))
	if err == nil {
		t.Fatal("an undetected Ollama must refuse activation")
	}
	if !strings.Contains(err.Error(), "settings.url") {
		t.Errorf("the refusal does not name the operator's override: %v", err)
	}
}

// The pool is a strict extension of the proxy: enabling it while the proxy
// unit is down refuses WITH that reason, which is what the declaration's
// `requires:` expresses and what the status row then reports.
func TestOllamaPoolRefusesWhenTheProxyIsDown(t *testing.T) {
	cap := &ollamaPoolCapability{
		Ollama:   &ollamaCapability{Apps: agent.NewAppRegistry()},
		Cloudbox: func() string { return "https://cloud.example.com" },
	}
	err := cap.Activate(context.Background(), cfgOf("ollama-pool", nil))
	if err == nil || !strings.Contains(err.Error(), "ollama proxy unit is not active") {
		t.Fatalf("pool activation without the proxy = %v, want a refusal naming it", err)
	}
	// Unpaired: cloudbox is where the registry is pushed.
	nopair := &ollamaPoolCapability{Ollama: &ollamaCapability{Apps: agent.NewAppRegistry()}}
	if err := nopair.Activate(context.Background(), cfgOf("ollama-pool", nil)); err == nil {
		t.Error("the pool must refuse on a host with no cloudbox URL")
	}
}

// The pool must notice when the proxy it rides is CYCLED. The two are
// separate units, and a cycled proxy builds a fresh in-flight counter — a
// pool still pushing through the old one would report capacity for a daemon
// nothing is proxying. Reporting unhealthy is what makes the reconciler
// rebind it, which is `requires:` at runtime.
func TestOllamaPoolHealthFollowsTheProxyIdentity(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer daemon.Close()
	proxy := &ollamaCapability{Apps: agent.NewAppRegistry()}
	if err := proxy.Activate(context.Background(), cfgOf("ollama", map[string]string{"url": daemon.URL})); err != nil {
		t.Fatalf("proxy Activate: %v", err)
	}
	pool := &ollamaPoolCapability{
		Ollama:      proxy,
		AgentName:   func() string { return "h" },
		Cloudbox:    func() string { return "https://cloud.example.com" },
		AccessToken: func() string { return "tok" },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := pool.Activate(ctx, cfgOf("ollama-pool", nil)); err != nil {
		t.Fatalf("pool Activate: %v", err)
	}
	t.Cleanup(func() { _ = pool.Deactivate(context.Background()) })
	if err := pool.Health(context.Background()); err != nil {
		t.Fatalf("Health on a freshly bound pool: %v", err)
	}

	// The proxy cycles — a new counter, same unit.
	if err := proxy.Activate(context.Background(), cfgOf("ollama", map[string]string{"url": daemon.URL})); err != nil {
		t.Fatalf("proxy re-Activate: %v", err)
	}
	if err := pool.Health(context.Background()); err == nil {
		t.Error("Health must report a stale binding after the proxy cycled")
	}

	// And the proxy going down entirely is reported too.
	if err := proxy.Deactivate(context.Background()); err != nil {
		t.Fatalf("proxy Deactivate: %v", err)
	}
	if err := pool.Health(context.Background()); err == nil {
		t.Error("Health must report the proxy being down")
	}
}

// Re-activating a mount that carries an intercept must not stack a second
// handler on the same prefix. AddIntercept APPENDS, so a Configure pass over
// a live registration would mount /_pool/capacity twice.
func TestReactivationDoesNotStackIntercepts(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer daemon.Close()
	apps := agent.NewAppRegistry()
	cap := &ollamaCapability{Apps: apps}
	cfg := cfgOf("ollama", map[string]string{"url": daemon.URL})
	for i := 0; i < 3; i++ {
		if err := cap.Activate(context.Background(), cfg); err != nil {
			t.Fatalf("Activate %d: %v", i, err)
		}
	}
	t.Cleanup(func() { _ = cap.Deactivate(context.Background()) })
	if n := apps.InterceptCount(agent.BuiltinOllama); n != 1 {
		t.Errorf("%d intercepts on the ollama mount after 3 activations, want 1", n)
	}
}

// The same-LAN listener's port lives in the DECLARATION, which is the whole
// improvement over lan_inference_port: fleet checks it against every other
// enabled unit's listeners. The default stays off every model port on the
// host — 11434 (the Ollama it proxies), 11435 (bashy's own engine) and 24556
// (the llm door).
func TestLANInferencePortComesFromTheDeclaration(t *testing.T) {
	for _, reserved := range []int{11434, 11435, 24556} {
		if DefaultLANInferencePort == reserved {
			t.Fatalf("the LAN inference default %d collides with a model port", reserved)
		}
	}
	if got := configPort(cfgOf("lan-inference", nil), "", DefaultLANInferencePort); got != DefaultLANInferencePort {
		t.Errorf("no declared port = %d, want the default %d", got, DefaultLANInferencePort)
	}
	declared := service.Config{Ports: []fleet.ServicePort{{Port: 12000, Bind: "lan"}}}
	if got := configPort(declared, "", DefaultLANInferencePort); got != 12000 {
		t.Errorf("declared port = %d, want 12000", got)
	}
	named := service.Config{Ports: []fleet.ServicePort{{Name: "other", Port: 1}, {Name: "api", Port: 12001}}}
	if got := configPort(named, "api", DefaultLANInferencePort); got != 12001 {
		t.Errorf("named port = %d, want 12001", got)
	}
}

func TestLANInferenceCapabilityServesAndWithdraws(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	cap := &lanInferenceCapability{Ollama: func() string { return upstream.URL }}
	// Port 0 is not declarable through fleet, so use an ephemeral bind by
	// declaring nothing and letting the capability take the default — on a
	// busy CI box that may be held, so a bind failure is a skip, not a fail.
	cfg := service.Config{Unit: "lan-inference", Ports: []fleet.ServicePort{{Port: 0, Bind: "lan"}}}
	if err := cap.Activate(context.Background(), cfg); err != nil {
		t.Skipf("could not bind the default LAN inference port: %v", err)
	}
	t.Cleanup(func() { _ = cap.Deactivate(context.Background()) })
	if err := cap.Health(context.Background()); err != nil {
		t.Errorf("Health while serving: %v", err)
	}
	// The endpoint advertised to cloudbox comes from the BOUND port, so a
	// unit that is not serving advertises nothing.
	if err := cap.Deactivate(context.Background()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if ep := cap.Endpoint(); ep != "" {
		t.Errorf("Endpoint() = %q after deactivation, want empty", ep)
	}
}

// The warm plane refuses with the reason rather than starting a supervisor
// that has nothing to serve or a budget that cannot be honoured.
func TestWarmCapabilityRefusals(t *testing.T) {
	unpaired := &warmCapability{Paired: func() bool { return false }}
	if err := unpaired.Activate(context.Background(), cfgOf("warm", nil)); err == nil ||
		!strings.Contains(err.Error(), "not paired") {
		t.Errorf("unpaired activation = %v, want a pairing refusal", err)
	}
	plane := &sysloadPlane{ctx: context.Background()}
	badFrac := &warmCapability{
		Paired: func() bool { return true },
		Load:   plane.Profiler,
	}
	if err := badFrac.Activate(context.Background(), cfgOf("warm", map[string]string{"budget_frac": "1.5"})); err == nil ||
		!strings.Contains(err.Error(), "budget_frac") {
		t.Errorf("out-of-range budget = %v, want a refusal naming the setting", err)
	}
	if badFrac.Executor() != nil {
		t.Error("a refused activation must not leave a live executor behind")
	}
}

// The declared budget fraction is applied to the SHARED load sensor, so a
// `bashy service set warm --set settings.budget_frac=…` takes effect without
// cycling the sampler and losing its learned baseline.
func TestWarmCapabilityAppliesTheDeclaredBudget(t *testing.T) {
	plane := &sysloadPlane{ctx: context.Background()}
	cap := &warmCapability{
		Paired:  func() bool { return true },
		Load:    plane.Profiler,
		Desired: func() []string { return []string{"llama3.2:1b"} },
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := cap.Activate(ctx, cfgOf("warm", map[string]string{"budget_frac": "0.5"})); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	t.Cleanup(func() { _ = cap.Deactivate(context.Background()) })
	if got := plane.Profiler().WarmBudgetFrac(); got != 0.5 {
		t.Errorf("the profiler's budget fraction = %v, want 0.5", got)
	}
	if cap.Executor() == nil {
		t.Error("no executor after a successful activation")
	}
	if _, ok := cap.Status(); !ok {
		t.Error("Status() should supply the pool advertisement while active")
	}
	if err := cap.Deactivate(context.Background()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if _, ok := cap.Status(); ok {
		t.Error("Status() should report nothing once the unit is down")
	}
}

// The shard plane refuses with the missing dependency named, where the old
// zero-config ShardOn() gate returned a silent nil and every shard verb
// answered "not enabled on this host" without saying why.
func TestShardCapabilityRefusalNamesTheMissingDependency(t *testing.T) {
	// No mesh on this host: newShardManager's own refusal reaches the unit.
	nomesh := &shardCapability{
		Build: func(shardSettings) (*shard.Manager, error) {
			return newShardManager(&conf.FileConfig{AgentName: "h", AccessToken: "t"}, shardSettings{}, nil, nil, nil)
		},
	}
	err := nomesh.Activate(context.Background(), cfgOf("shard", nil))
	if err == nil || !strings.Contains(err.Error(), "mesh") {
		t.Errorf("activation without a mesh = %v, want a refusal naming it", err)
	}
	if nomesh.Manager() != nil {
		t.Error("a refused activation must not leave a manager behind")
	}
	// ShardControl must be a TYPED NIL-SAFE value: a nil *Manager becoming a
	// non-nil warm.ShardControl would have the warm supervisor form shards
	// through a manager that is not there.
	if ctl := nomesh.ShardControl(); ctl != nil {
		t.Errorf("ShardControl() = %#v with no manager, want a nil interface", ctl)
	}
	// A build that returns (nil, nil) is still a refusal, not a silent
	// "activated with nothing".
	silent := &shardCapability{Build: func(shardSettings) (*shard.Manager, error) { return nil, nil }}
	if err := silent.Activate(context.Background(), cfgOf("shard", nil)); err == nil {
		t.Error("a nil manager with no error must still refuse")
	}
	// And no wiring at all refuses rather than panicking.
	if err := (&shardCapability{}).Activate(context.Background(), cfgOf("shard", nil)); err == nil {
		t.Error("an unwired shard capability must refuse")
	}
	if err := nomesh.Health(context.Background()); err == nil {
		t.Error("Health on an inactive shard plane must report it")
	}
}

// The shard declaration's settings project onto the ring's inputs.
func TestReadShardSettings(t *testing.T) {
	ss := readShardSettings(cfgOf("shard", map[string]string{
		"role":       "leader",
		"peers":      "host-a, host-b",
		"node_bytes": "17179869184",
		"server_bin": "/opt/prima/llama-server",
		"worker_bin": "/opt/prima/llama-cli",
	}))
	if ss.Role != "leader" {
		t.Errorf("Role = %q", ss.Role)
	}
	if len(ss.Peers) != 2 || ss.Peers[0] != "host-a" || ss.Peers[1] != "host-b" {
		t.Errorf("Peers = %v", ss.Peers)
	}
	if ss.NodeBytes != 17179869184 {
		t.Errorf("NodeBytes = %d", ss.NodeBytes)
	}
	if ss.ServerBin == "" || ss.WorkerBin == "" {
		t.Errorf("bins = %+v", ss)
	}
	// An empty declaration asks the node to measure itself and find its own
	// binaries, which is what newShardManager's zero-config path does.
	zero := readShardSettings(cfgOf("shard", nil))
	if zero.NodeBytes != 0 || zero.ServerBin != "" || len(zero.Peers) != 0 {
		t.Errorf("empty declaration = %+v, want zero values", zero)
	}
}

// The observability surfaces are published while the unit is active and
// withdrawn when the ycode proxy they front goes away — the old boot path
// registered them once and never withdrew.
func TestOtelCapabilityPublishesAndWithdrawsSurfaces(t *testing.T) {
	apps := agent.NewAppRegistry()
	available := true
	cap := &otelCapability{
		Apps: apps,
		Detect: func() otel.Target {
			return otel.Target{Available: available, ProxyURL: "http://127.0.0.1:31415", Token: "tok",
				ManifestPath: "/tmp/manifest.json"}
		},
	}
	if err := cap.Activate(context.Background(), cfgOf("otel", map[string]string{"pool": "false"})); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	for _, surface := range otel.Surfaces() {
		if apps.LookupTarget(surface) == nil {
			t.Errorf("surface %q was not published", surface)
		}
	}
	if cap.PoolShared() {
		t.Error("settings.pool=false must not report the stack as shared")
	}
	// Default (absent setting) is on, matching the retired otel_pool_enabled.
	if err := cap.Activate(context.Background(), cfgOf("otel", nil)); err != nil {
		t.Fatalf("re-Activate: %v", err)
	}
	if !cap.PoolShared() {
		t.Error("an absent settings.pool must default to shared")
	}

	available = false
	if err := cap.Health(context.Background()); err == nil {
		t.Error("Health must fail once the local proxy is unreachable")
	}
	if err := cap.Deactivate(context.Background()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	for _, surface := range otel.Surfaces() {
		if apps.LookupTarget(surface) != nil {
			t.Errorf("surface %q survived deactivation", surface)
		}
	}
}

func TestOtelCapabilityRefusesWithoutAProxy(t *testing.T) {
	cap := &otelCapability{
		Apps:   agent.NewAppRegistry(),
		Detect: func() otel.Target { return otel.Target{ManifestPath: "/tmp/none.json"} },
	}
	err := cap.Activate(context.Background(), cfgOf("otel", nil))
	if err == nil || !strings.Contains(err.Error(), "/tmp/none.json") {
		t.Errorf("refusal = %v, want one naming the manifest it tried", err)
	}
}

// `settings.share` replaces the ycode_share_surfaces boolean overlay: one
// declared list, with the catalog's own defaults when it is empty and `all`
// as the shorthand. (Replaces the retired otel.YcodeSurfaceEnabled.)
func TestYcodeShareSet(t *testing.T) {
	defaults := ycodeShareSet(cfgOf("ycode", nil))
	for _, s := range otel.YcodeSurfaces() {
		if defaults[s.Name] != s.DefaultOn {
			t.Errorf("empty settings.share: %q = %v, want the catalog default %v",
				s.Name, defaults[s.Name], s.DefaultOn)
		}
	}
	explicit := ycodeShareSet(cfgOf("ycode", map[string]string{"share": "ycode-canvas"}))
	if !explicit["ycode-canvas"] {
		t.Error("an explicit list must select the named surface")
	}
	if explicit["ycode"] {
		t.Error("an explicit list must REPLACE the defaults, not add to them")
	}
	all := ycodeShareSet(cfgOf("ycode", map[string]string{"share": "all"}))
	for _, s := range otel.YcodeSurfaces() {
		if !all[s.Name] {
			t.Errorf("share=all dropped %q", s.Name)
		}
	}
}

func TestYcodeCapabilityPublishesSelectedSurfaces(t *testing.T) {
	apps := agent.NewAppRegistry()
	cap := &ycodeCapability{
		Apps: apps,
		Detect: func() otel.Target {
			return otel.Target{Available: true, ProxyURL: "http://127.0.0.1:31415", Token: "tok"}
		},
	}
	cfg := cfgOf("ycode", map[string]string{"share": "ycode-canvas", "require_login": "true"})
	if err := cap.Activate(context.Background(), cfg); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if apps.LookupTarget("ycode-canvas") == nil {
		t.Error("the selected surface was not published")
	}
	if apps.LookupTarget("ycode") != nil {
		t.Error("an unselected surface was published")
	}
	for _, e := range apps.Entries() {
		if e.Name == "ycode-canvas" && !e.RequireLogin {
			t.Error("settings.require_login=true was not applied")
		}
	}
	if err := cap.Deactivate(context.Background()); err != nil {
		t.Fatalf("Deactivate: %v", err)
	}
	if apps.LookupTarget("ycode-canvas") != nil {
		t.Error("the surface survived deactivation")
	}
}

func TestYcodeCapabilityRefusesAnUnknownSurface(t *testing.T) {
	cap := &ycodeCapability{
		Apps: agent.NewAppRegistry(),
		Detect: func() otel.Target {
			return otel.Target{Available: true, ProxyURL: "http://127.0.0.1:31415"}
		},
	}
	err := cap.Activate(context.Background(), cfgOf("ycode", map[string]string{"share": "ycode-nope"}))
	if err == nil || !strings.Contains(err.Error(), "no known surface") {
		t.Errorf("refusal = %v, want one listing the known surfaces", err)
	}
}

// The declaration readers accept the spellings an operator actually writes.
func TestSettingReaders(t *testing.T) {
	cfg := cfgOf("u", map[string]string{
		"a": " yes ", "b": "OFF", "c": "12", "d": "0.25", "e": "x, y\nz",
		"bad_int": "nope", "bad_float": "nope",
	})
	if !settingBool(cfg, "a") {
		t.Error(`settingBool(" yes ") should be true`)
	}
	if settingBoolDefault(cfg, "b", true) {
		t.Error(`settingBoolDefault("OFF", true) should be false`)
	}
	if !settingBoolDefault(cfg, "missing", true) {
		t.Error("an absent setting must take the default")
	}
	if settingInt(cfg, "c", 0) != 12 || settingInt(cfg, "bad_int", 7) != 7 {
		t.Error("settingInt")
	}
	if settingUint(cfg, "c", 0) != 12 {
		t.Error("settingUint")
	}
	if settingFloat(cfg, "d", 0) != 0.25 || settingFloat(cfg, "bad_float", 1) != 1 {
		t.Error("settingFloat")
	}
	list := settingList(cfg, "e")
	if len(list) != 3 || list[0] != "x" || list[2] != "z" {
		t.Errorf("settingList = %v", list)
	}
	if settingList(cfg, "missing") != nil {
		t.Error("an absent list setting must be nil")
	}
}

// emptyBaseline stands in for fleet's embedded seed ring so a test catalog
// holds only the units it was given.
type emptyBaseline struct{}

func (emptyBaseline) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

func atoiOrZero(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
