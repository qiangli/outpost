package headlamp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeDefaults(t *testing.T) {
	o, err := Options{}.Normalize()
	if err != nil {
		t.Fatalf("zero-value Options must normalize: %v", err)
	}
	if o.Namespace != DefaultNamespace || o.Name != DefaultName || o.Image != DefaultImage || o.LocalPort != DefaultLocalPort {
		t.Fatalf("defaults not applied: %+v", o)
	}
	if o.DisableViewerRBAC {
		t.Fatal("viewer RBAC must default ON")
	}
}

func TestNormalizeRejects(t *testing.T) {
	cases := []Options{
		{Namespace: "Bad_Namespace"},
		{Name: "UPPER"},
		{Namespace: "-leading"},
		{Name: "trailing-"},
		{LocalPort: 80},    // privileged port
		{LocalPort: 70000}, // out of range
		{Namespace: strings.Repeat("a", 64)},
	}
	for _, c := range cases {
		if _, err := c.Normalize(); err == nil {
			t.Errorf("Normalize(%+v) must fail", c)
		}
	}
}

func TestValidateListenAddrLoopbackOnly(t *testing.T) {
	ok := []string{"127.0.0.1:18466", "127.9.9.9:1", "[::1]:80", "localhost:8080"}
	for _, addr := range ok {
		if err := ValidateListenAddr(addr); err != nil {
			t.Errorf("ValidateListenAddr(%q) must accept loopback: %v", addr, err)
		}
	}
	bad := []string{
		":18466",            // all interfaces
		"0.0.0.0:18466",     // all interfaces
		"[::]:18466",        // all interfaces
		"192.168.1.5:18466", // LAN
		"100.64.0.9:18466",  // tailnet
		"example.com:18466", // hostname other than localhost (DNS could point anywhere)
		"127.0.0.1",         // no port
		"garbage",           // unparseable
	}
	for _, addr := range bad {
		if err := ValidateListenAddr(addr); err == nil {
			t.Errorf("ValidateListenAddr(%q) must reject", addr)
		}
	}
}

func TestForwardArgsPinsLoopback(t *testing.T) {
	args, err := ForwardArgs("/kube/config", Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"kubectl", "--kubeconfig", "/kube/config",
		"-n", "headlamp", "port-forward",
		"--address", "127.0.0.1",
		"svc/headlamp", "18466:80",
	}
	if len(args) != len(want) {
		t.Fatalf("argv mismatch:\n got %v\nwant %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full: %v)", i, args[i], want[i], args)
		}
	}

	args, err = ForwardArgs("", Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range args {
		if a == "--kubeconfig" {
			t.Fatal("empty kubeconfig path must not emit --kubeconfig")
		}
	}
}

// renderedList decodes RenderJSON output for structural assertions.
type renderedList struct {
	APIVersion string           `json:"apiVersion"`
	Kind       string           `json:"kind"`
	Items      []map[string]any `json:"items"`
}

func render(t *testing.T, o Options) renderedList {
	t.Helper()
	raw, err := RenderJSON(o)
	if err != nil {
		t.Fatal(err)
	}
	var list renderedList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("RenderJSON output is not valid JSON: %v", err)
	}
	return list
}

func itemsOfKind(list renderedList, kind string) []map[string]any {
	var out []map[string]any
	for _, it := range list.Items {
		if it["kind"] == kind {
			out = append(out, it)
		}
	}
	return out
}

func TestRenderJSONShape(t *testing.T) {
	list := render(t, Options{})
	if list.Kind != "List" || list.APIVersion != "v1" {
		t.Fatalf("want v1 List envelope, got %s %s", list.APIVersion, list.Kind)
	}
	kinds := map[string]int{}
	for _, it := range list.Items {
		kind, _ := it["kind"].(string)
		if kind == "" {
			t.Fatalf("item missing kind: %v", it)
		}
		kinds[kind]++
	}
	want := map[string]int{
		"Namespace": 1, "ServiceAccount": 2, "ClusterRole": 1,
		"ClusterRoleBinding": 2, "Deployment": 1, "Service": 1,
	}
	for k, n := range want {
		if kinds[k] != n {
			t.Errorf("want %d %s, got %d", n, k, kinds[k])
		}
	}

	// Deterministic output for identical options.
	a, _ := RenderJSON(Options{})
	b, _ := RenderJSON(Options{})
	if string(a) != string(b) {
		t.Fatal("RenderJSON must be deterministic")
	}
}

func TestRenderJSONSecurityPosture(t *testing.T) {
	raw, err := RenderJSON(Options{})
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, forbidden := range []string{"NodePort", "LoadBalancer", "hostNetwork", "hostPort", "cluster-admin"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("manifest must not contain %q", forbidden)
		}
	}
	if !strings.Contains(s, "/healthz") {
		t.Fatal("deployment must probe /healthz for readiness (Headlamp v0.27.0 does not serve 200 on /)")
	}
	list := render(t, Options{})

	svc := itemsOfKind(list, "Service")[0]
	spec := svc["spec"].(map[string]any)
	if spec["type"] != "ClusterIP" {
		t.Fatalf("service type = %v, want ClusterIP", spec["type"])
	}

	// The two viewer bindings reference only view + nodeview, and only
	// the viewer SA — never the pod SA.
	for _, crb := range itemsOfKind(list, "ClusterRoleBinding") {
		roleRef := crb["roleRef"].(map[string]any)
		name := roleRef["name"].(string)
		if name != "view" && name != NodeViewClusterRole {
			t.Errorf("unexpected roleRef %q", name)
		}
		for _, subj := range crb["subjects"].([]any) {
			if subj.(map[string]any)["name"] != ViewerServiceAccount {
				t.Errorf("binding subject %v is not the viewer SA", subj)
			}
		}
	}

	// The nodeview role grants read-only nodes access and nothing else.
	role := itemsOfKind(list, "ClusterRole")[0]
	rules := role["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("nodeview must carry exactly one rule, got %d", len(rules))
	}
	rule := rules[0].(map[string]any)
	if res := rule["resources"].([]any); len(res) != 1 || res[0] != "nodes" {
		t.Fatalf("nodeview resources = %v, want [nodes]", res)
	}
	for _, v := range rule["verbs"].([]any) {
		switch v {
		case "get", "list", "watch":
		default:
			t.Fatalf("nodeview must be read-only, found verb %v", v)
		}
	}
}

func TestRenderJSONDisableViewerRBAC(t *testing.T) {
	list := render(t, Options{DisableViewerRBAC: true})
	if n := len(itemsOfKind(list, "ClusterRoleBinding")); n != 0 {
		t.Fatalf("DisableViewerRBAC must render zero bindings, got %d", n)
	}
	if n := len(itemsOfKind(list, "ClusterRole")); n != 0 {
		t.Fatalf("DisableViewerRBAC must render zero clusterroles, got %d", n)
	}
	if n := len(itemsOfKind(list, "ServiceAccount")); n != 1 {
		t.Fatalf("DisableViewerRBAC must render only the pod SA, got %d SAs", n)
	}
}
