package portal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostSharesWire(t *testing.T) {
	var posts, deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer own-token" {
			t.Errorf("bearer = %q", r.Header.Get("Authorization"))
		}
		if r.URL.EscapedPath() != "/api/v1/hosts/my%20host/shares" && r.URL.EscapedPath() != "/api/v1/hosts/my%20host/shares/id%2F1" {
			t.Errorf("path = %q", r.URL.EscapedPath())
		}
		switch r.Method {
		case http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"shares": []any{map[string]any{"id": "id/1", "host": "my host", "app": "notes", "email": "member@example.test", "created_at": "2026-01-02T03:04:05Z"}}})
		case http.MethodPost:
			posts++
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["app"] != "notes" || body["email"] != "member@example.test" {
				t.Errorf("body = %#v", body)
			}
			if posts == 1 {
				w.WriteHeader(http.StatusCreated)
			} else {
				w.WriteHeader(http.StatusOK)
			}
			json.NewEncoder(w).Encode(map[string]any{"share": map[string]any{"id": "id/1", "host": "my host", "app": "notes", "email": "member@example.test", "created_at": "2026-01-02T03:04:05Z"}})
		case http.MethodDelete:
			deletes++
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	shares, err := ListHostShares(ctx, server.URL, "own-token", "my host")
	if err != nil || len(shares) != 1 || shares[0].ID != "id/1" {
		t.Fatalf("list = %+v, %v", shares, err)
	}
	share, status, err := CreateHostShare(ctx, server.URL, "own-token", "my host", "notes", "member@example.test")
	if err != nil || status != 201 || share.ID != "id/1" {
		t.Fatalf("create = %+v, %d, %v", share, status, err)
	}
	_, status, err = CreateHostShare(ctx, server.URL, "own-token", "my host", "notes", "member@example.test")
	if err != nil || status != 200 {
		t.Fatalf("idempotent create status=%d err=%v", status, err)
	}
	if err := DeleteHostShare(ctx, server.URL, "own-token", "my host", "id/1"); err != nil {
		t.Fatal(err)
	}
	if posts != 2 || deletes != 1 {
		t.Fatalf("posts=%d deletes=%d", posts, deletes)
	}
}

func TestHostSharesContractError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"code":"not_mutual_member","error":"not a mutual member"}`))
	}))
	defer server.Close()
	_, status, err := CreateHostShare(context.Background(), server.URL, "token", "h", "app", "outsider@example.test")
	if status != 422 {
		t.Fatalf("status=%d", status)
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.Code != "not_mutual_member" {
		t.Fatalf("error = %v", err)
	}
}
