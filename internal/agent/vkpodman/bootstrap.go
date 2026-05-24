package vkpodman

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FetchEndpointPath is the cloudbox endpoint that mints per-host
// kubeconfigs. Kept here as a constant so the bootstrap call site and
// any future url-builder agree without duplicating the literal.
const FetchEndpointPath = "/api/cluster/kubeconfig"

// FetchRequest is the JSON body the outpost POSTs to cloudbox at
// FetchEndpointPath. NodeName tells cloudbox which of the owner's
// paired hosts the kubeconfig is for; cloudbox enforces that the
// (owner, node_name) pair exists in its host table before minting a
// ServiceAccount token.
type FetchRequest struct {
	NodeName string `json:"node_name"`
}

// FetchResponse mirrors cloudbox's hub/internal/handlers/cluster.go
// agentKubeconfigResp shape. CAData is base64 so the JSON stays clean
// even when the CA bundle contains PEM newlines.
type FetchResponse struct {
	APIURL   string `json:"api_url"`
	Token    string `json:"token"`
	CAData   string `json:"ca_data,omitempty"`
	NodeName string `json:"node_name"`
}

// FetchKubeconfig POSTs to cloudbox's per-host kubeconfig endpoint
// using the existing outpost access_token, then decodes the response
// into a ParsedKubeconfig the caller can persist into FileConfig.Cluster.
// cloudboxBase is the HTTPS URL of cloudbox (e.g. "https://ai.dhnt.io"),
// matching what cmd/outpost/main.go::cloudboxHTTPBase already derives
// from the matrix-tunnel pairing fields.
//
// Returns an error wrapping the HTTP status when cloudbox responds
// non-2xx — callers can use this to distinguish 503 (cluster mode off)
// from 401/403 (bad token) from 5xx (cloudbox issue). The body, when
// present, carries cloudbox's `{"error": "..."}` shape which we
// surface in the error message.
func FetchKubeconfig(ctx context.Context, cloudboxBase, accessToken, nodeName string) (*ParsedKubeconfig, error) {
	if strings.TrimSpace(cloudboxBase) == "" {
		return nil, errors.New("vkpodman: empty cloudboxBase")
	}
	if strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("vkpodman: empty accessToken")
	}
	if strings.TrimSpace(nodeName) == "" {
		return nil, errors.New("vkpodman: empty nodeName")
	}
	url := strings.TrimRight(cloudboxBase, "/") + FetchEndpointPath
	body, err := json.Marshal(FetchRequest{NodeName: nodeName})
	if err != nil {
		return nil, fmt.Errorf("vkpodman: marshal fetch request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vkpodman: dial cloudbox: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return nil, &FetchError{Status: resp.StatusCode, Message: msg}
	}
	var out FetchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("vkpodman: decode fetch response: %w", err)
	}
	var ca []byte
	if out.CAData != "" {
		decoded, derr := base64.StdEncoding.DecodeString(out.CAData)
		if derr != nil {
			return nil, fmt.Errorf("vkpodman: decode ca_data base64: %w", derr)
		}
		ca = decoded
	}
	if strings.TrimSpace(out.APIURL) == "" {
		return nil, errors.New("vkpodman: cloudbox response missing api_url")
	}
	if strings.TrimSpace(out.Token) == "" {
		return nil, errors.New("vkpodman: cloudbox response missing token")
	}
	return &ParsedKubeconfig{
		APIURL: out.APIURL,
		Token:  out.Token,
		CA:     ca,
	}, nil
}

// FetchError wraps a non-2xx cloudbox response. Status exposes the HTTP
// code so the caller can branch on 503 (cluster mode disabled
// upstream — non-fatal, retry later) vs. 401/403 (token doesn't have
// the right scope — fatal until pairing is refreshed).
type FetchError struct {
	Status  int
	Message string
}

func (e *FetchError) Error() string {
	if e == nil {
		return "<nil FetchError>"
	}
	return fmt.Sprintf("vkpodman: cloudbox fetch: HTTP %d: %s", e.Status, e.Message)
}

// IsClusterDisabled reports whether err is a 503 from cloudbox,
// meaning the upstream cloudbox instance hasn't enabled cluster mode
// (CLUSTER_ENABLED=false or kubeconfig load failed at boot). Callers
// treat this as "not ready yet — try again later" rather than a
// permanent error.
func IsClusterDisabled(err error) bool {
	var fe *FetchError
	if errors.As(err, &fe) {
		return fe.Status == http.StatusServiceUnavailable
	}
	return false
}

// TokenExpiry parses the exp claim out of a ServiceAccount JWT without
// verifying the signature — we only need the timestamp, and the token
// came back to us through TLS from cloudbox which already checked it.
// Returns zero time when the token isn't a JWT at all (e.g. legacy
// opaque tokens) so the refresher's "rotate at T-12h" math reads it
// as "expires far in the future" and keeps using it.
func TokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some encoders use padded base64; fall back to that.
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}
	}
	if claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}
