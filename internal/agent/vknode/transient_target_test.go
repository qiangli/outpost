package vknode

import (
	"context"
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
	publishPod(apps, pod, func(int) string { return "172.28.16.2" })
	name := TransientAppName(pod.Namespace, pod.Name, string(pod.UID))
	if got := apps[name]; got != "http://172.28.16.2:46391" {
		t.Fatalf("transient target = %q, want machine address and mapped host port", got)
	}
}

func TestProviderUpdatePodPublishesMachineAddress(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "user-1", Name: "bashy-run-1", UID: types.UID("abcdef123456")},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Ports: []corev1.ContainerPort{{ContainerPort: 46390, HostPort: 46391}},
		}}},
	}
	p := NewProviderWithBackend(&fakeBackend{})
	p.transientHost = func(context.Context, int) string { return "172.28.16.2" }
	apps := recordingTransientApps{}
	p.SetTransientApps(apps)
	if err := p.UpdatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	name := TransientAppName(pod.Namespace, pod.Name, string(pod.UID))
	if got := apps[name]; got != "http://172.28.16.2:46391" {
		t.Fatalf("transient target = %q, want reachable machine address", got)
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
