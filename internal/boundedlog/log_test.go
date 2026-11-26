package boundedlog

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentLogCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("legacy"), 1000), 0600); err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	for range 8 {
		writers.Go(func() {
			for range 12 {
				if _, err := appendBounded(path, []byte("entry\n"), 64); err != nil {
					t.Error(err)
					return
				}
				s, e := os.Stat(path)
				if e != nil || s.Size() > 64 {
					t.Error("cap exceeded", s, e)
					return
				}
			}
		})
	}
	writers.Wait()
	b, e := os.ReadFile(path)
	if e != nil || len(b) == 0 || len(b) > 64 || !bytes.Equal(b, bytes.Repeat([]byte("entry\n"), len(b)/6)) {
		t.Fatalf("invalid retained log: %q %v", b, e)
	}
}
