package adminui

import (
	"strings"
	"testing"
)

func TestValidateOutbound(t *testing.T) {
	cases := []struct {
		name     string
		in       outboundUpsertReq
		wantErr  string // substring; "" means no error
		wantName string // expected post-normalization value
		wantSch  string // expected post-normalization value
		wantPort int    // expected post-normalization value
	}{
		{
			name:    "http needs name",
			in:      outboundUpsertReq{Path: "p", Host: "h", User: "u"},
			wantErr: "name is required for http",
		},
		{
			name:     "http strips local_port",
			in:       outboundUpsertReq{Path: "p", Name: "n", Host: "h", User: "u", Scheme: "http", LocalPort: 8000},
			wantSch:  "",
			wantName: "n",
			wantPort: 0,
		},
		{
			name:    "tcp needs local_port",
			in:      outboundUpsertReq{Path: "p", Name: "n", Host: "h", User: "u", Scheme: "tcp"},
			wantErr: "local_port 0 is out of range",
		},
		{
			name:     "tcp happy",
			in:       outboundUpsertReq{Path: "p", Name: "n", Host: "h", User: "u", Scheme: "tcp", LocalPort: 5432},
			wantSch:  "tcp",
			wantName: "n",
			wantPort: 5432,
		},
		{
			name:     "ssh ignores name and stores empty",
			in:       outboundUpsertReq{Path: "p", Name: "stale-value", Host: "h", User: "u", Scheme: "ssh", LocalPort: 2022},
			wantSch:  "ssh",
			wantName: "",
			wantPort: 2022,
		},
		{
			name:    "ssh needs local_port",
			in:      outboundUpsertReq{Path: "p", Host: "h", User: "u", Scheme: "ssh"},
			wantErr: "local_port 0 is out of range",
		},
		{
			name:    "unknown scheme",
			in:      outboundUpsertReq{Path: "p", Name: "n", Host: "h", User: "u", Scheme: "ftp"},
			wantErr: `scheme "ftp" must be one of http|tcp|ssh`,
		},
		{
			name:    "reserved path",
			in:      outboundUpsertReq{Path: "api", Name: "n", Host: "h", User: "u"},
			wantErr: "reserved by the admin UI",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.in
			err := validateOutbound(&req)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if req.Name != tc.wantName {
					t.Errorf("name = %q, want %q", req.Name, tc.wantName)
				}
				if req.Scheme != tc.wantSch {
					t.Errorf("scheme = %q, want %q", req.Scheme, tc.wantSch)
				}
				if req.LocalPort != tc.wantPort {
					t.Errorf("local_port = %d, want %d", req.LocalPort, tc.wantPort)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}
