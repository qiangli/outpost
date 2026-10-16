package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalExporterSelection(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	ctx := context.Background()
	if LocalExporter(ctx, srv.URL) == nil {
		t.Fatal("collector answering: want an exporter")
	}
	srv.Close()
	if LocalExporter(ctx, srv.URL) != nil {
		t.Fatal("collector gone: want collector-free (nil)")
	}
}

func TestEnsureLocalCollector(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if EnsureLocalCollector(context.Background(), "http://127.0.0.1:1") {
		t.Fatal("dead endpoint must not enable export")
	}
	if !EnsureLocalCollector(context.Background(), srv.URL) {
		t.Fatal("live endpoint must enable export")
	}
	if !EnsureLocalCollector(context.Background(), srv.URL) {
		t.Fatal("second call stays enabled")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "x:4317")
	if EnsureLocalCollector(context.Background(), srv.URL) {
		t.Fatal("explicit env endpoint wins")
	}
}
