package storeroute

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostRouted(t *testing.T) {
	var gotHost, gotQuery string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotQuery = r.Host, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "body=%s", b)
	}))
	defer backend.Close()
	be := strings.TrimPrefix(backend.URL, "http://")
	front := httptest.NewServer(Handler(func(_ context.Context, p string) (string, error) {
		if p == "peera" {
			return be, nil
		}
		return "", ErrUnknownPeer
	}))
	defer front.Close()

	do := func(host string) *http.Response {
		req, _ := http.NewRequest("PUT", front.URL+"/b/k?X-Amz-Signature=abc&X-Amz-Expires=60", strings.NewReader("data"))
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := do("peera.sat1.localhost:31890")
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "body=data" || gotHost != "peera.sat1.localhost:31890" || gotQuery != "X-Amz-Signature=abc&X-Amz-Expires=60" {
		t.Fatalf("host=%q query=%q body=%q", gotHost, gotQuery, b)
	}
	if r := do("nope.sat1.localhost:31890"); r.StatusCode != 404 {
		t.Fatalf("unknown peer: %d", r.StatusCode)
	}
	if r := do("example.com"); r.StatusCode != 404 {
		t.Fatalf("bad host: %d", r.StatusCode)
	}
}
