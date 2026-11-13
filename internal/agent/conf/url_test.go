package conf

import "testing"

func TestAppTargetFromURL(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		scheme     string
		host       string
		port       int
		socket     string
		wantErr    bool
		errContain string
	}{
		{name: "http with port", in: "http://localhost:8080", scheme: "http", host: "localhost", port: 8080},
		{name: "https without port", in: "https://example.com/", scheme: "https", host: "example.com", port: 443},
		{name: "http without port", in: "http://example.com/", scheme: "http", host: "example.com", port: 80},
		{name: "ipv4 host", in: "http://127.0.0.1:1234", scheme: "http", host: "127.0.0.1", port: 1234},
		{name: "ipv6 host", in: "http://[::1]:1234", scheme: "http", host: "::1", port: 1234},
		{name: "unix triple-slash", in: "unix:///run/podman/podman.sock", scheme: "unix", socket: "/run/podman/podman.sock"},
		{name: "unix single-slash", in: "unix:/var/run/docker.sock", scheme: "unix", socket: "/var/run/docker.sock"},
		{name: "uppercase scheme normalized", in: "HTTP://x:9", scheme: "http", host: "x", port: 9},

		{name: "empty", in: "", wantErr: true, errContain: "required"},
		{name: "bare path", in: "/foo", wantErr: true, errContain: "scheme"},
		{name: "ftp rejected", in: "ftp://x:21", wantErr: true, errContain: "scheme"},
		{name: "no host", in: "http://:8080", wantErr: true, errContain: "host"},
		{name: "bad port", in: "http://x:99999", wantErr: true, errContain: "port"},
		{name: "unix no path", in: "unix://", wantErr: true, errContain: "socket"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, h, p, sk, err := AppTargetFromURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (scheme=%q host=%q port=%d socket=%q)", s, h, p, sk)
				}
				if tc.errContain != "" && !contains(err.Error(), tc.errContain) {
					t.Fatalf("error %q missing %q", err.Error(), tc.errContain)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s != tc.scheme || h != tc.host || p != tc.port || sk != tc.socket {
				t.Fatalf("got (scheme=%q host=%q port=%d socket=%q); want (scheme=%q host=%q port=%d socket=%q)",
					s, h, p, sk, tc.scheme, tc.host, tc.port, tc.socket)
			}
		})
	}
}

func TestAppBasePathFromURL(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		wantErr  bool
	}{
		{in: "http://127.0.0.1:22749/vlog/", want: "/vlog"},
		{in: "https://h/a/b", want: "/a/b"},
		{in: "http://h:1", want: ""},
		{in: "http://h:1/", want: ""},
		{in: "unix:///run/podman/podman.sock", want: ""},
		{in: "tcp://h:5432", want: ""},
		{in: "git+https://example.com/", want: ""},
		{in: "egress://", want: ""},

		{in: "tcp://h:5432/db", wantErr: true},
		{in: "git+https://example.com/owner", wantErr: true},
		{in: "http://h:1/x?y=1", wantErr: true},
		{in: "http://h:1/x#frag", wantErr: true},
		{in: "http://h:1/a/../b", wantErr: true},
		{in: "http://h:1/a//b", wantErr: true},
	} {
		got, err := AppBasePathFromURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: want error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got (%q, %v), want %q", tc.in, got, err, tc.want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
