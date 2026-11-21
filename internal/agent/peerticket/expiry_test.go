package peerticket

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/qiangli/outpost/internal/agent/recovery"
)

func TestExchangeRecordsExplicitClientElevationExpiry(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, tc := range []struct {
		name, body string
		status     int
		expired    bool
	}{
		{"generic", `{"error":"elevation required"}`, 401, false},
		{"wrong status", `{"error":"invalid elevation cookie: token has invalid claims: token is expired"}`, 403, false},
		{"expired", `{"error":"invalid elevation cookie: token has invalid claims: token is expired"}`, 401, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			_, err := Exchange(t.Context(), server.URL, "test", "test", "test", "ssh")
			if err == nil || errors.Is(err, ErrElevationExpired) != tc.expired {
				t.Fatalf("expiry classification: %v", err)
			}
		})
	}
	events, err := recovery.Read()
	if err != nil || len(events) != 1 || events[0].Component != "ssh_client" ||
		events[0].Reason != "client_elevation_expired" {
		t.Fatalf("client elevation diagnostics = %+v, error = %v", events, err)
	}
}
