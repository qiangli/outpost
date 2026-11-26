package upgrade

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/outpost/internal/boundedlog"
)

func TestUpgradeHistoryRetainsLatestAtLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(boundedlog.MaxBytes); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	l := NewLedger(path)
	if err := l.Append(LedgerEntry{Step: "verified"}); err != nil {
		t.Fatal(err)
	}
	entries, err := l.Tail(1)
	if err != nil || len(entries) != 1 || entries[0].Step != "verified" {
		t.Fatalf("latest entry %+v, %v", entries, err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() > boundedlog.MaxBytes {
		t.Fatalf("size cap failed: %v %v", st, err)
	}
}
