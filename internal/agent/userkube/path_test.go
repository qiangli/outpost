package userkube

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		goos string
		want string
	}{
		{
			name: "windows msys temp path",
			in:   "/c/Users/liqia/AppData/Local/Temp/tmp.X",
			goos: "windows",
			want: `C:\Users\liqia\AppData\Local\Temp\tmp.X`,
		},
		{
			name: "windows msys lowercase drive",
			in:   "/d/workspace/kubeconfig.yaml",
			goos: "windows",
			want: `D:\workspace\kubeconfig.yaml`,
		},
		{
			name: "windows msys uppercase drive",
			in:   "/C/workspace/kubeconfig.yaml",
			goos: "windows",
			want: `C:\workspace\kubeconfig.yaml`,
		},
		{
			name: "windows msys root drive slash",
			in:   "/c/",
			goos: "windows",
			want: `C:\`,
		},
		{
			name: "windows msys bare drive",
			in:   "/c",
			goos: "windows",
			want: `C:\`,
		},
		{
			name: "windows msys mixed slash",
			in:   `/c/Users\liqia/test.yaml`,
			goos: "windows",
			want: `C:\Users\liqia\test.yaml`,
		},
		{
			name: "windows wsl path",
			in:   "/mnt/c/Users/liqia/test.yaml",
			goos: "windows",
			want: `C:\Users\liqia\test.yaml`,
		},
		{
			name: "windows wsl bare drive",
			in:   "/mnt/c",
			goos: "windows",
			want: `C:\`,
		},
		{
			name: "windows native path with backslash",
			in:   `C:\Users\liqia\test.yaml`,
			goos: "windows",
			want: `C:\Users\liqia\test.yaml`,
		},
		{
			name: "windows native path with forward slash",
			in:   `C:/Users/liqia/test.yaml`,
			goos: "windows",
			want: `C:/Users/liqia/test.yaml`,
		},
		{
			name: "windows unc path",
			in:   `\\server\share\file.yaml`,
			goos: "windows",
			want: `\\server\share\file.yaml`,
		},
		{
			name: "windows relative path",
			in:   `tmp/file.yaml`,
			goos: "windows",
			want: `tmp/file.yaml`,
		},
		{
			name: "windows non-drive unix path",
			in:   `/etc/hosts`,
			goos: "windows",
			want: `/etc/hosts`,
		},
		{
			name: "windows multi-char first segment",
			in:   `/cd/foo`,
			goos: "windows",
			want: `/cd/foo`,
		},
		{
			name: "windows single slash",
			in:   `/`,
			goos: "windows",
			want: `/`,
		},
		{
			name: "windows empty",
			in:   "",
			goos: "windows",
			want: "",
		},
		{
			name: "darwin msys path untouched",
			in:   "/c/Users/liqia/AppData/Local/Temp/tmp.X",
			goos: "darwin",
			want: "/c/Users/liqia/AppData/Local/Temp/tmp.X",
		},
		{
			name: "linux msys path untouched",
			in:   "/c/Users/liqia/AppData/Local/Temp/tmp.X",
			goos: "linux",
			want: "/c/Users/liqia/AppData/Local/Temp/tmp.X",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizePathOS(tc.in, tc.goos)
			if got != tc.want {
				t.Errorf("normalizePathOS(%q, %q) = %q, want %q", tc.in, tc.goos, got, tc.want)
			}
		})
	}
}

func TestWriteStandaloneMSYSPathOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific MSYS path resolution test")
	}
	dir := t.TempDir()
	vol := filepath.VolumeName(dir)
	if len(vol) < 2 || vol[1] != ':' {
		t.Skipf("unexpected volume name %q", vol)
	}
	drive := strings.ToLower(string(vol[0]))
	rel := filepath.ToSlash(dir[len(vol):])
	realFile := filepath.Join(dir, "userkube.yaml")

	// Pre-create 0-byte file (matching `mktemp` behaviour).
	if err := os.WriteFile(realFile, []byte{}, 0o600); err != nil {
		t.Fatalf("create 0-byte file: %v", err)
	}

	msysFile := "/" + drive + rel + "/userkube.yaml"
	if err := WriteStandalone([]byte(validUserKubeconfig), msysFile); err != nil {
		t.Fatalf("WriteStandalone(%s): %v", msysFile, err)
	}

	info, err := os.Stat(realFile)
	if err != nil {
		t.Fatalf("real file not found at %s: %v", realFile, err)
	}
	if info.Size() == 0 {
		t.Fatalf("real file at %s is 0 bytes", realFile)
	}

	// Verify no stray C:\c tree was created.
	cDriveStray := vol + `\c`
	if _, err := os.Stat(cDriveStray); err == nil {
		t.Errorf("stray directory %s was created", cDriveStray)
	}
}
