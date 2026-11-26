package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoundedLogRetentionAndOversize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.log")
	for _, p := range []string{path, path + ".1"} {
		if err := os.WriteFile(p, []byte(strings.Repeat("x", 20)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := openBoundedLog(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("abcdefghijklmnopqrst")); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{path: "qrst", path + ".1": "ijklmnop"} {
		got, err := os.ReadFile(p)
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q, %v; want %q", p, got, err, want)
		}
	}
	b, err = openBoundedLog(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("uvwx")); err != nil {
		t.Fatal(err)
	}
	b.Close()
	got, _ := os.ReadFile(path)
	if string(got) != "qrstuvwx" {
		t.Fatalf("restart: %q", got)
	}
}

func TestBoundedLogFailedRolloverDrainsAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.log")
	b, err := openBoundedLog(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	b.Write([]byte("abcd"))
	b.rename = func(string, string) error { return errors.New("rename refused") }
	n, err := b.Write([]byte("lost"))
	if n != 4 || err != nil {
		t.Fatalf("write = %d, %v", n, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "abcd" {
		t.Fatalf("failed rollover changed active: %q", got)
	}
	b.rename = os.Rename
	b.Write([]byte("next"))
	got, _ = os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	if string(got) != "next" || string(old) != "abcd" {
		t.Fatalf("recovery active=%q previous=%q", got, old)
	}
}

func TestBoundedLogReceivesRedactedOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "child.log")
	b, err := openBoundedLog(path, 128)
	if err != nil {
		t.Fatal(err)
	}
	out, err := newChildLogOutput(b)
	if err != nil {
		t.Fatal(err)
	}
	token := "9f42e8a1b0c7d6358a4f1c962e0b3d75f8a9c6102e4d7b953a6f8c012d35b7e4"
	for i := 0; i < 5; i++ {
		if _, err := out.write.Write([]byte("token=" + token + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	out.close()
	b.Close()
	var retained string
	for _, p := range []string{path, path + ".1"} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > 128 || strings.Contains(string(data), token) {
			t.Fatalf("unsafe retained log %s: %q", p, data)
		}
		retained += string(data)
	}
	if !strings.Contains(retained, "[redacted:") {
		t.Fatalf("missing redaction marker: %q", retained)
	}
}
