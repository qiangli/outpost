package upgrade

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func pairFixture(t *testing.T, dir, prefix string) map[string]string {
	t.Helper()
	paths := map[string]string{}
	for _, name := range ProductMembers() {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(prefix+name), 0755); err != nil {
			t.Fatal(err)
		}
		paths[name] = path
	}
	return paths
}
func applyFixture(t *testing.T, live, staged map[string]string, hook func() error) error {
	t.Helper()
	companions := map[string]string{}
	for name, path := range staged {
		if name != productName("outpost") {
			companions[name] = path
		}
	}
	return applyPair(live[productName("outpost")], staged[productName("outpost")], companions, hook)
}
func assertPairBytes(t *testing.T, paths map[string]string, prefix string) {
	t.Helper()
	for name, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != prefix+name {
			t.Errorf("%s=%q, %v; expected %s", name, data, err, prefix+name)
		}
	}
}

func TestPairTransactionRollbackAllMembers(t *testing.T) {
	live := pairFixture(t, t.TempDir(), "old-")
	staged := pairFixture(t, t.TempDir(), "new-")
	if err := applyFixture(t, live, staged, nil); err != nil {
		t.Fatal(err)
	}
	assertPairBytes(t, live, "new-")
	rec, err := readPairRecord(live[productName("outpost")])
	if err != nil || rec == nil || rec.Phase != "complete" {
		t.Fatalf("record=%+v %v", rec, err)
	}
	if err := restorePair(*rec); err != nil {
		t.Fatal(err)
	}
	assertPairBytes(t, live, "old-")
	if rec, err := readPairRecord(live[productName("outpost")]); err != nil || rec != nil {
		t.Fatalf("record not consumed: %+v %v", rec, err)
	}
}
func TestPairLastSwapFailureRestoresEarlierMembers(t *testing.T) {
	live := pairFixture(t, t.TempDir(), "old-")
	staged := pairFixture(t, t.TempDir(), "new-")
	err := applyFixture(t, live, staged, func() error { return os.Remove(staged[productName("outpost")]) })
	if err == nil {
		t.Fatal("expected final swap failure")
	}
	assertPairBytes(t, live, "old-")
}
func TestPairRetainFailurePreservesPreviousGeneration(t *testing.T) {
	live := pairFixture(t, t.TempDir(), "first-")
	staged := pairFixture(t, t.TempDir(), "second-")
	if err := applyFixture(t, live, staged, nil); err != nil {
		t.Fatal(err)
	}
	binary := live[productName("outpost")]
	before, err := os.ReadFile(pairRecordPath(binary))
	if err != nil {
		t.Fatal(err)
	}
	next := pairFixture(t, t.TempDir(), "third-")
	// A directory at a late destination fails retention after earlier members
	// were retained. The previously published rollback generation must survive.
	target := live[productName("sh")]
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := applyFixture(t, live, next, nil); err == nil {
		t.Fatal("expected retain failure")
	}
	after, err := os.ReadFile(pairRecordPath(binary))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("previous rollback record changed: %v", err)
	}
	rec, err := readPairRecord(binary)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range rec.Members {
		if !m.Absent {
			sum, err := pairFileHash(m.PrevPath)
			if err != nil || sum != m.SHA256 {
				t.Fatalf("old generation damaged: %s %v", m.PrevPath, err)
			}
		}
	}
}
func TestPairInterruptedRecoveryAndCorruptPrevious(t *testing.T) {
	live := pairFixture(t, t.TempDir(), "old-")
	staged := pairFixture(t, t.TempDir(), "new-")
	if err := applyFixture(t, live, staged, nil); err != nil {
		t.Fatal(err)
	}
	binary := live[productName("outpost")]
	rec, err := readPairRecord(binary)
	if err != nil {
		t.Fatal(err)
	}
	rec.Phase = "prepared"
	if err := writePairRecord(*rec); err != nil {
		t.Fatal(err)
	}
	// A missing retained file must block BEFORE any member changes.
	last := rec.Members[len(rec.Members)-1]
	saved, err := os.ReadFile(last.PrevPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(last.PrevPath); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverInterruptedPair(binary); err == nil {
		t.Fatal("expected missing retained member error")
	}
	assertPairBytes(t, live, "new-")
	if err := os.WriteFile(last.PrevPath, saved, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoverInterruptedPair(binary); err != nil {
		t.Fatal(err)
	}
	assertPairBytes(t, live, "old-")
}
func TestPairBeforeSwapFailureLeavesOriginal(t *testing.T) {
	live := pairFixture(t, t.TempDir(), "old-")
	staged := pairFixture(t, t.TempDir(), "new-")
	if err := applyFixture(t, live, staged, func() error { return errors.New("marker write failed") }); err == nil {
		t.Fatal("expected marker error")
	}
	assertPairBytes(t, live, "old-")
}
func TestParsePairedShellBanner(t *testing.T) {
	for _, s := range []string{"GNU bash, version 5.3.0(1)-bashy-v1.2.3-dev (arm64)\n", "bashy, version 5.3.0(1)-bashy-v1.2.3-dev"} {
		if got := ParseBashyBanner(s); got != "v1.2.3-dev" {
			t.Fatal(got)
		}
	}
}
