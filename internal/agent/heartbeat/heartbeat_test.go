package heartbeat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAcknowledgementRequiresAcceptedHeartbeat(t *testing.T) {
	for _, code := range []int{200, 401, 403, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/host/heartbeat" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("missing authenticated heartbeat")
				}
				w.WriteHeader(code)
			}))
			defer srv.Close()
			w := New(Config{CloudboxBase: srv.URL, AccessToken: "fixture-token", AgentName: "fixture", OnAcknowledged: func(context.Context) { calls++ }})
			err := w.postOne(context.Background())
			if code == 200 {
				if err != nil || calls != 1 {
					t.Fatal(err, calls)
				}
			} else if err == nil || calls != 0 {
				t.Fatal("unaccepted heartbeat acknowledged", err, calls)
			}
		})
	}
}
