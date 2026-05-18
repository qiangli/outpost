package agent

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestClipboardRoundTrip writes a known string to the OS clipboard via
// POST /clipboard and reads it back via GET /clipboard. Only meaningful
// on macOS (the only platform we support clipboard for today).
func TestClipboardRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("clipboard bridge only implemented on darwin")
	}
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.GET("/clipboard", clipboardHandler())
	engine.POST("/clipboard", clipboardHandler())

	srv := httptest.NewServer(engine)
	defer srv.Close()

	want := "matrix-clipboard-roundtrip-" + t.Name()
	wReq, _ := http.NewRequest("POST", srv.URL+"/clipboard", strings.NewReader(want))
	wResp, err := http.DefaultClient.Do(wReq)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	wResp.Body.Close()
	if wResp.StatusCode != http.StatusNoContent {
		t.Fatalf("write status = %d, want 204", wResp.StatusCode)
	}

	rResp, err := http.DefaultClient.Get(srv.URL + "/clipboard")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer rResp.Body.Close()
	if rResp.StatusCode != http.StatusOK {
		t.Fatalf("read status = %d, want 200", rResp.StatusCode)
	}
	got, _ := io.ReadAll(rResp.Body)
	if string(got) != want {
		t.Errorf("clipboard roundtrip mismatch: got %q, want %q", got, want)
	}
}

// TestClipboardTooLarge asserts the size cap is enforced.
func TestClipboardTooLarge(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("clipboard bridge only implemented on darwin")
	}
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/clipboard", clipboardHandler())
	srv := httptest.NewServer(engine)
	defer srv.Close()

	huge := strings.Repeat("a", clipboardMaxBytes+1)
	resp, err := http.Post(srv.URL+"/clipboard", "text/plain", strings.NewReader(huge))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversize status = %d, want 413", resp.StatusCode)
	}
}
