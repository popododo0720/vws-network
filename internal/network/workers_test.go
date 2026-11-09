package network

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/popododo0720/vws-network/internal/domain"
)

var testPool = mustP("10.250.0.0/24")

func TestCheckWorkers(t *testing.T) {
	for _, ok := range []int{1, DefaultWorkers, MaxWorkers} {
		if err := CheckWorkers(ok); err != nil {
			t.Fatalf("%d: %v", ok, err)
		}
	}
	for _, bad := range []int{-1, 0, MaxWorkers + 1} {
		var de *domain.Error
		if err := CheckWorkers(bad); !errors.As(err, &de) || de.Kind != domain.Invalid {
			t.Fatalf("%d: %v, want Invalid", bad, err)
		}
	}
}

// TestOutOfRangeWorkersFailsAtStart: Open refuses the count before it opens
// the store, so no database file is created.
func TestOutOfRangeWorkersFailsAtStart(t *testing.T) {
	for _, bad := range []int{-1, MaxWorkers + 1} {
		db := filepath.Join(t.TempDir(), "a.db")
		var de *domain.Error
		_, err := Open(t.Context(), Config{DatabasePath: db, AddressPool: testPool, Workers: bad})
		if !errors.As(err, &de) || de.Kind != domain.Invalid {
			t.Fatalf("%d: err %v, want Invalid", bad, err)
		}
		if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%d: store opened: %v", bad, err)
		}
	}
	db := filepath.Join(t.TempDir(), "a.db")
	svc, err := Open(t.Context(), Config{DatabasePath: db, AddressPool: testPool, Workers: MaxWorkers})
	if err != nil {
		t.Fatalf("max workers: %v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
}
