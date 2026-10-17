package vknode

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestProvider_PodIPsFromInspect(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings InspectNetworkSettings
		want     []corev1.PodIP
	}{
		{name: "missing"},
		{name: "legacy", settings: InspectNetworkSettings{InspectNetwork: InspectNetwork{IPAddress: "192.0.2.10"}}, want: []corev1.PodIP{{IP: "192.0.2.10"}}},
		{name: "attachments dual stack stable", settings: InspectNetworkSettings{Networks: map[string]InspectNetwork{
			"z-network": {IPAddress: "192.0.2.20"},
			"a-network": {IPAddress: "192.0.2.10", GlobalIPv6Address: "2001:db8::10"},
		}}, want: []corev1.PodIP{{IP: "192.0.2.10"}, {IP: "2001:db8::10"}}},
		{name: "legacy wins and deduplicates", settings: InspectNetworkSettings{InspectNetwork: InspectNetwork{IPAddress: "192.0.2.20"}, Networks: map[string]InspectNetwork{"bridge": {IPAddress: "192.0.2.10"}}}, want: []corev1.PodIP{{IP: "192.0.2.20"}}},
		{name: "reject invalid and local", settings: InspectNetworkSettings{InspectNetwork: InspectNetwork{IPAddress: "not-an-ip", GlobalIPv6Address: "fe80::1"}, Networks: map[string]InspectNetwork{"bridge": {IPAddress: "127.0.0.1", GlobalIPv6Address: "::"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, fake := newProviderWithFake(t)
			pod := newTestPod("network", "network-test-uid")
			ctx := context.Background()
			if err := p.CreatePod(ctx, pod); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			for _, c := range fake.containers {
				c.NetworkSettings = tc.settings
			}
			fake.mu.Unlock()
			st, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(st.PodIPs, tc.want) {
				t.Fatalf("PodIPs = %+v, want %+v", st.PodIPs, tc.want)
			}
			primary := ""
			if len(tc.want) > 0 {
				primary = tc.want[0].IP
			}
			if st.PodIP != primary {
				t.Fatalf("PodIP = %q, want %q", st.PodIP, primary)
			}
			if st.Phase != corev1.PodRunning || !st.ContainerStatuses[0].Ready || st.Conditions[0].Status != corev1.ConditionTrue {
				t.Fatalf("running status: %+v", st)
			}
		})
	}
}

func TestProvider_NetworkReadinessHealthz(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("probe path = %q", r.URL.Path)
				}
				w.WriteHeader(code)
			}))
			defer health.Close()
			_, port, err := net.SplitHostPort(health.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			hostPort, _ := strconv.Atoi(port)
			p, fake := newProviderWithFake(t)
			pod := newTestPod("health", "health-test-uid")
			pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 8080, HostPort: int32(hostPort)}}
			pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http")}}}
			ctx := context.Background()
			if err := p.CreatePod(ctx, pod); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			for _, c := range fake.containers {
				c.NetworkSettings.IPAddress = "192.0.2.10"
			}
			fake.mu.Unlock()
			st, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
			if err != nil {
				t.Fatal(err)
			}
			wantReady := code == http.StatusOK
			if st.ContainerStatuses[0].Ready != wantReady {
				t.Fatalf("container readiness = %v, want %v", st.ContainerStatuses[0].Ready, wantReady)
			}
			for _, cond := range st.Conditions {
				if (cond.Status == corev1.ConditionTrue) != wantReady {
					t.Fatalf("condition: %+v", cond)
				}
			}
			if st.PodIP != "192.0.2.10" {
				t.Fatalf("PodIP lost on readiness update: %q", st.PodIP)
			}
		})
	}
}
