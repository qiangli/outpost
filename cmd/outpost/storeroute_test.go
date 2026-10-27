package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

func TestStoreRouteWithoutMesh(t *testing.T) {
	const host = "self.satellite.localhost:31890"
	const query = "X-Amz-Signature=abc&key=a%2Fb"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host || r.URL.RawQuery != query {
			t.Errorf("request changed: host=%q query=%q", r.Host, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte("object"))
	}))
	defer backend.Close()
	local := func() ([]admincore.MeshServiceView, error) {
		return []admincore.MeshServiceView{{Name: "s3", Addr: strings.TrimPrefix(backend.URL, "http://")}}, nil
	}
	h := storeRouteHandler("self", local, nil, nil)
	for _, tc := range []struct {
		host   string
		status int
		body   string
	}{
		{host, http.StatusOK, "object"},
		{"remote.satellite.localhost:31890", http.StatusBadGateway, "mesh not available"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/bucket/object?"+query, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("got %d %q, want %d containing %q", w.Code, w.Body.String(), tc.status, tc.body)
			}
		})
	}
}
