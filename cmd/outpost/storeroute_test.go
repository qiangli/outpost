package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

func TestStoreRouteWithoutMesh(t *testing.T) {
	const host = "127.0.0.1:31890"
	const query = "X-Amz-Signature=abc&key=a%2Fb"
	var gotHost, gotURI string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotURI = r.Host, r.RequestURI
		_, _ = w.Write([]byte("object"))
	}))
	defer backend.Close()
	local := func() ([]admincore.MeshServiceView, error) {
		return []admincore.MeshServiceView{{Name: "s3", Addr: strings.TrimPrefix(backend.URL, "http://")}}, nil
	}
	h := storeRouteHandler("Self", local, nil, nil)
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{"/s3-self/key?" + query, http.StatusOK, "object"},
		{"/s3-self/key%2Fpart?" + query, http.StatusOK, "object"},
		{"/s3-self", http.StatusOK, "object"},
		{"/s3-other/key", http.StatusBadGateway, "mesh not available"},
		{"/bad/key", http.StatusNotFound, "bucket must be s3-<peer-id>"},
		{"/s3-/key", http.StatusNotFound, "bucket must be s3-<peer-id>"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+host+tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("got %d %q, want %d containing %q", w.Code, w.Body.String(), tc.status, tc.body)
			}
			if tc.status == http.StatusOK && (gotHost != host || gotURI != tc.path) {
				t.Fatalf("request changed: host=%q uri=%q", gotHost, gotURI)
			}
		})
	}
}
