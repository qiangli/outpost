package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// TestPreserveTraceContext_RoundTrip is the core contract assertion
// for the cross-process trace fabric: an inbound `traceparent` header
// from cloudbox gets parsed by the Tracing() gin middleware, then
// PreserveTraceContext re-emits a `traceparent` with the SAME trace_id
// onto the outbound app-proxy request. If this breaks, the cloudbox-
// to-outpost-to-app chain orphans every span past outpost.
func TestPreserveTraceContext_RoundTrip(t *testing.T) {
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	const inboundTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	const expectedTraceID = "0af7651916cd43dd8448eb211c80319c"

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Tracing("test-svc"))

	var capturedOut *http.Request
	r.GET("/proxy", func(c *gin.Context) {
		// Simulate the outpost apps.go Rewrite callback: synthesize an
		// outbound request whose Context() carries the gin span, then
		// call PreserveTraceContext as Rewrite would.
		out, _ := http.NewRequestWithContext(c.Request.Context(),
			"POST", "http://app/v1/chat", nil)
		PreserveTraceContext(out, c.Request)
		capturedOut = out
		c.String(http.StatusOK, "ok")
	})

	srv := httptest.NewServer(r)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(),
		"GET", srv.URL+"/proxy", nil)
	req.Header.Set("traceparent", inboundTraceparent)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	resp.Body.Close()

	if capturedOut == nil {
		t.Fatal("handler did not run")
	}
	got := capturedOut.Header.Get("traceparent")
	if got == "" {
		t.Fatal("outbound app-proxy request has no traceparent — downstream app would be orphaned")
	}
	parts := strings.Split(got, "-")
	if len(parts) != 4 {
		t.Fatalf("malformed traceparent %q: want 4 parts, got %d", got, len(parts))
	}
	if parts[1] != expectedTraceID {
		t.Errorf("outbound trace_id = %s, want %s (cloudbox-stamped trace_id) — fabric broken at outpost hop",
			parts[1], expectedTraceID)
	}
}

// TestProvider_NoEndpoint_StillInstallsPropagator: the home-host
// default (no centralized collector) must still preserve the
// traceparent across outpost's proxy hop, so cooperative apps
// downstream see the parent trace from ycode→cloudbox even when
// outpost itself isn't exporting spans.
func TestProvider_NoEndpoint_StillInstallsPropagator(t *testing.T) {
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() { otel.SetTextMapPropagator(prevProp) })
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	_, err := build(context.Background())
	if err != nil {
		t.Fatalf("build with no endpoint: %v", err)
	}
	prop := otel.GetTextMapPropagator()
	carrier := propagation.HeaderCarrier{}
	carrier.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	ctx := prop.Extract(context.Background(), carrier)
	out := propagation.HeaderCarrier{}
	prop.Inject(ctx, out)
	if got := out.Get("traceparent"); got == "" {
		t.Fatal("no-exporter mode failed to install a working propagator — outpost-to-app preservation would break")
	}
}
