package network

import (
	"context"
	"sort"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/popododo0720/vws-lib/obs"

	"github.com/popododo0720/vws-network/internal/domain"
)

func TestEnsureMetricsCountResultWithFixedLabels(t *testing.T) {
	ctx := context.Background()
	reader := sdkmetric.NewManualReader()
	shutdown, err := obs.Setup(ctx, obs.ForService("network"), obs.WithOverride(func(c *obs.Config) {
		c.MetricReaders = append(c.MetricReaders, reader)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	fake := newFake()
	m := measured{fake}
	vpc := domain.VPC{ID: "vpc-1", CIDR: mustP("10.0.0.0/16")}
	if err := m.ensureVPC(ctx, vpc); err != nil {
		t.Fatal(err)
	}
	fake.failNext(1, &domain.Error{Kind: domain.Conflict, Msg: "foreign"})
	_ = m.ensureVPC(ctx, vpc) //nolint:errcheck // the metric is the assertion
	fake.failNext(1, context.DeadlineExceeded)
	_ = m.ensureVPC(ctx, vpc) //nolint:errcheck // the metric is the assertion

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != obs.MetricReconcileTotal {
				continue
			}
			sum, ok := mt.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s data %T", mt.Name, mt.Data)
			}
			for _, dp := range sum.DataPoints {
				var kv []string
				for _, a := range dp.Attributes.ToSlice() {
					kv = append(kv, string(a.Key)+"="+a.Value.AsString())
				}
				got = append(got, strings.Join(kv, ","))
			}
		}
	}
	sort.Strings(got)
	want := []string{
		"controller=network.vpc,result=conflict",
		"controller=network.vpc,result=error",
		"controller=network.vpc,result=ok",
	}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("vws.reconcile.total points %v, want %v", got, want)
	}
}
