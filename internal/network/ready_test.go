package network

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type unreadyDataplane struct{ *fakeDataplane }

func (unreadyDataplane) ready(context.Context) error { return errors.New("subnet ext not found") }

func TestReady(t *testing.T) {
	ctx := context.Background()
	pool := mustP("198.51.100.0/24")
	sqlite := func(t *testing.T) repository {
		t.Helper()
		r, err := openSQLite(ctx, filepath.Join(t.TempDir(), "r.db"))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	cases := []struct {
		name  string
		setup func(t *testing.T) *Service
		want  string
	}{
		{"healthy with workers", func(t *testing.T) *Service {
			s, err := start(ctx, sqlite(t), newFake(), pool, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			return s
		}, ""},
		{"closed store", func(t *testing.T) *Service {
			r := sqlite(t)
			s, err := newService(ctx, r, newFake(), pool)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.close(); err != nil {
				t.Fatal(err)
			}
			return s
		}, "store:"},
		{"stale heartbeat", func(t *testing.T) *Service {
			s, err := newService(ctx, newMemRepo(), newFake(), pool)
			if err != nil {
				t.Fatal(err)
			}
			s.stop = func() {}
			s.heartbeat.Store(time.Now().Add(-2 * staleHeartbeat).UnixNano())
			return s
		}, "workers:"},
		{"dataplane read fails", func(t *testing.T) *Service {
			s, err := newService(ctx, newMemRepo(), unreadyDataplane{newFake()}, pool)
			if err != nil {
				t.Fatal(err)
			}
			return s
		}, "dataplane: subnet ext not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.setup(t).Ready(ctx)
			if c.want == "" {
				if err != nil {
					t.Fatalf("Ready = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), c.want) {
				t.Fatalf("Ready = %v, want prefix %q", err, c.want)
			}
		})
	}
}
