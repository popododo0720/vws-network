package network

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/popododo0720/vws-network/internal/domain"
)

type panickingDataplane struct{ *fakeDataplane }

func (panickingDataplane) ensureVPC(context.Context, domain.VPC) error {
	panic("secret kubeconfig path /root/.kube/config")
}

func TestDataplanePanicFailsOperationWithoutLeak(t *testing.T) {
	ctx := context.Background()
	svc, err := newService(ctx, newMemRepo(), panickingDataplane{newFake()}, mustP("198.51.100.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	svc.backoff = func(int) time.Duration { return 0 }
	svc.maxAttempts = 3
	var logged []string
	svc.logf = func(format string, args ...any) { logged = append(logged, format) }
	op, err := svc.CreateVPC(ctx, call("proj", "vpc"), domain.CreateVPC{CIDR: mustP("10.7.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := svc.step(ctx); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	op, err = svc.GetOperation(ctx, "proj", op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.State != domain.OpFailed || op.Err == nil || op.Err.Msg != "internal error" {
		t.Fatalf("op %+v err %+v", op, op.Err)
	}
	if strings.Contains(op.Err.Msg, "secret") {
		t.Fatalf("panic value leaked into %q", op.Err.Msg)
	}
	if len(logged) != 3 || !strings.Contains(logged[0], "panic in") {
		t.Fatalf("log %v", logged)
	}
}

func TestStepRecoveringSurvivesPanic(t *testing.T) {
	ctx := context.Background()
	svc, _ := openTest(t, newMemRepo())
	svc.newID = func(string) string { panic("boom") }
	ok, err := svc.stepRecovering(ctx)
	if ok || !errors.Is(err, errPanic) {
		t.Fatalf("ok %v err %v, want false and errPanic", ok, err)
	}
}
