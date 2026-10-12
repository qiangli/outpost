package telemetry

import (
	"slices"
	"testing"
)

func TestChildEnvPinsEverySignalToLocalCollector(t *testing.T) {
	got := ChildEnv("act_runner")
	for _, want := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + LocalCollectorEndpoint,
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=" + LocalCollectorEndpoint + "/v1/traces",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT=" + LocalCollectorEndpoint + "/v1/metrics",
		"OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=" + LocalCollectorEndpoint + "/v1/logs",
		"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
		"OTEL_SERVICE_NAME=act_runner",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q in %v", want, got)
		}
	}
}
