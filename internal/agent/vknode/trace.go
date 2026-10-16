package vknode

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
)

const tracerName = "outpost/vknode"

// SetNodeName stamps the virtual node's name onto every pod lifecycle span.
func (p *Provider) SetNodeName(n string) { p.nodeName = n }

// SetTracerProvider overrides the global tracer provider (tests).
func (p *Provider) SetTracerProvider(tp trace.TracerProvider) { p.tp = tp }

func (p *Provider) tracer() trace.Tracer {
	if p.tp != nil {
		return p.tp.Tracer(tracerName)
	}
	return otel.Tracer(tracerName)
}

func (p *Provider) podAttrs(pod *corev1.Pod) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("k8s.node.name", p.nodeName),
		attribute.String("k8s.namespace.name", pod.Namespace),
		attribute.String("k8s.pod.name", pod.Name),
		attribute.String("k8s.pod.uid", string(pod.UID)),
	}
}

// startPodSpan opens a lifecycle span; the returned func ends it, recording err.
func (p *Provider) startPodSpan(ctx context.Context, name string, pod *corev1.Pod) (context.Context, func(error)) {
	ctx, span := p.tracer().Start(ctx, name, trace.WithAttributes(p.podAttrs(pod)...))
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

// recordPodFinish emits one span the first time a pod is seen terminal.
func (p *Provider) recordPodFinish(ctx context.Context, pod *corev1.Pod, st *corev1.PodStatus) {
	if st == nil || (st.Phase != corev1.PodSucceeded && st.Phase != corev1.PodFailed) {
		return
	}
	key := podKey(pod.Namespace, pod.Name) + "/" + string(pod.UID)
	if _, dup := p.finished.LoadOrStore(key, struct{}{}); dup {
		return
	}
	_, span := p.tracer().Start(ctx, "vknode.pod.finish", trace.WithAttributes(
		append(p.podAttrs(pod), attribute.String("k8s.pod.phase", string(st.Phase)))...))
	if st.Phase == corev1.PodFailed {
		span.SetStatus(codes.Error, st.Reason)
	}
	span.End()
}
