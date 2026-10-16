package vknode

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestProvider_PodStartEmitsSpan(t *testing.T) {
	p, _ := newProviderWithFake(t)
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	p.SetTracerProvider(tp)
	p.SetNodeName("n1-vk-podman")
	p.SetRequireExplicitAccess(false)

	pod := newTestPod("hello", "22222222-aaaa-bbbb-cccc-dddddddddddd")
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 || spans[0].Name != "vknode.pod.start" {
		t.Fatalf("want one vknode.pod.start span, got %+v", spans)
	}
	got := map[attribute.Key]string{}
	for _, kv := range spans[0].Attributes {
		got[kv.Key] = kv.Value.AsString()
	}
	if got["k8s.node.name"] != "n1-vk-podman" || got["k8s.pod.name"] != "hello" || got["k8s.namespace.name"] != "user-1" {
		t.Errorf("attributes: %+v", got)
	}
}
