package telemetry

import (
	"context"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// ExportGauges uses the same OTLP/HTTP transport as local trace export, without
// installing a global meter provider or retaining stale Kubernetes samples.
func ExportGauges(ctx context.Context, endpoint, token string, data *metricdata.ResourceMetrics) error {
	opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(strings.TrimRight(endpoint, "/") + "/v1/metrics"), otlpmetrichttp.WithTimeout(5 * time.Second), otlpmetrichttp.WithRetry(otlpmetrichttp.RetryConfig{Enabled: false})}
	if token != "" {
		opts = append(opts, otlpmetrichttp.WithHeaders(map[string]string{"Authorization": "Bearer " + token}))
	}
	exp, err := otlpmetrichttp.New(ctx, opts...)
	if err != nil {
		return err
	}
	defer exp.Shutdown(ctx)
	return exp.Export(ctx, data)
}
