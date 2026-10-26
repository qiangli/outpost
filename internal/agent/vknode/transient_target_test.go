package vknode

import (
	"context"
	"net"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type recordingTransientApps map[string]string

func (r recordingTransientApps) Register(name, target string) error {
	r[name] = target
	return nil
}
func (r recordingTransientApps) Unregister(name string) { delete(r, name) }

func TestPublishPodUsesReachableHost(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "user-1", Name: "bashy-run-1", UID: types.UID("abcdef123456")},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Ports: []corev1.ContainerPort{{ContainerPort: 46390, HostPort: 46391}},
		}}},
	}
	apps := recordingTransientApps{}
	publishPod(apps, pod, func(int) string { return "172.28.16.2" }, nil)
	name := TransientAppName(pod.Namespace, pod.Name, string(pod.UID))
	if got := apps[name]; got != "http://172.28.16.2:46391" {
		t.Fatalf("transient target = %q, want machine address and mapped host port", got)
	}
}

type recordingDialApps struct {
	recordingTransientApps
	dial func(context.Context, string, string) (net.Conn, error)
}

func (r *recordingDialApps) RegisterWithDialer(name, target string, dial func(context.Context, string, string) (net.Conn, error)) error {
	r.recordingTransientApps[name] = target
	r.dial = dial
	return nil
}

func TestProviderUpdatePodSelectsHostAtDial(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "user-1", Name: "bashy-run-1", UID: types.UID("abcdef123456")},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Ports: []corev1.ContainerPort{{ContainerPort: 46390, HostPort: 46391}},
		}}},
	}
	p := NewProviderWithBackend(&fakeBackend{})
	lookups := 0
	p.transientHost = func(context.Context, int) string { lookups++; return "127.0.0.1" }
	apps := &recordingDialApps{recordingTransientApps: recordingTransientApps{}}
	p.SetTransientApps(apps)
	if err := p.UpdatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	name := TransientAppName(pod.Namespace, pod.Name, string(pod.UID))
	if got := apps.recordingTransientApps[name]; got != "http://127.0.0.1:46391" {
		t.Fatalf("transient target = %q, want published host port", got)
	}
	if lookups != 0 || apps.dial == nil {
		t.Fatalf("machine lookups at registration = %d; dialer installed = %v", lookups, apps.dial != nil)
	}
	conn, _ := apps.dial(context.Background(), "tcp", "127.0.0.1:46391")
	if conn != nil {
		_ = conn.Close()
	}
	if lookups != 1 {
		t.Fatalf("machine lookups after dial = %d, want 1", lookups)
	}
}

func TestMachineNameFromPodmanSocket(t *testing.T) {
	for _, tc := range []struct{ socket, want string }{
		{`\\.\pipe\podman-bashy`, "bashy"},
		{`//./pipe/podman-podman-machine-default`, "podman-machine-default"},
		{`C:\Users\Li\AppData\Local\Temp\podman\bashy-api.sock`, "bashy"},
		{`C:\Users\Li\AppData\Local\Temp\podman\podman-machine-default-api.sock`, "podman-machine-default"},
		{`\\.\pipe\docker_engine`, ""},
		{`\\.\pipe\podman--bad`, ""},
	} {
		if got := machineNameFromPodmanSocket(tc.socket); got != tc.want {
			t.Errorf("machineNameFromPodmanSocket(%q) = %q, want %q", tc.socket, got, tc.want)
		}
	}
}

func TestFirstMachineIPv4(t *testing.T) {
	if got := firstMachineIPv4("::1 127.0.0.1 169.254.0.3 172.28.16.2 10.0.0.1"); got != "172.28.16.2" {
		t.Fatalf("firstMachineIPv4 = %q", got)
	}
	if got := firstMachineIPv4("2: eth0 inet 172.17.254.194/20 brd 172.17.255.255 scope global eth0"); got != "172.17.254.194" {
		t.Fatalf("ip addr address = %q", got)
	}
}

func TestChoosePublishedHost(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reachable  map[string]bool
		wantHost   string
		wantSource string
		wantLookup bool
	}{
		{"published host port wins", map[string]bool{"127.0.0.1": true, "172.28.16.2": true}, "127.0.0.1", "host-loopback", false},
		{"machine port fallback", map[string]bool{"172.28.16.2": true}, "172.28.16.2", "podman-machine", true},
		{"neither port ready", nil, "127.0.0.1", "host-loopback-unverified", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookedUp := false
			host, source := choosePublishedHost(context.Background(), 45563,
				func(context.Context) string { lookedUp = true; return "172.28.16.2" },
				func(_ context.Context, candidate string, port int) bool {
					if port != 45563 {
						t.Fatalf("probed port %d, want 45563", port)
					}
					return tc.reachable[candidate]
				})
			if host != tc.wantHost || source != tc.wantSource || lookedUp != tc.wantLookup {
				t.Fatalf("choosePublishedHost = (%q, %q), machine lookup=%v; want (%q, %q), lookup=%v", host, source, lookedUp, tc.wantHost, tc.wantSource, tc.wantLookup)
			}
		})
	}
}
