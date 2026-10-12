package network

import (
	"context"
	"errors"
	"net/netip"
	"time"

	"github.com/popododo0720/vws-lib/obs"

	"github.com/popododo0720/vws-network/internal/domain"
)

// measured counts and times every ensure call as vws.reconcile.* with the
// object kind as controller and ok, conflict or error as result.
type measured struct{ dataplane }

func observe(ctx context.Context, controller string, start time.Time, err error) error {
	obs.M().Reconcile(ctx, controller, resultOf(err), time.Since(start))
	return err
}

func resultOf(err error) string {
	var de *domain.Error
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &de) && de.Kind == domain.Conflict:
		return "conflict"
	default:
		return "error"
	}
}

func (m measured) ensureVPC(ctx context.Context, v domain.VPC) error {
	return observe(ctx, "network.vpc", time.Now(), m.dataplane.ensureVPC(ctx, v))
}

func (m measured) ensureSubnet(ctx context.Context, s domain.Subnet) error {
	return observe(ctx, "network.subnet", time.Now(), m.dataplane.ensureSubnet(ctx, s))
}

func (m measured) ensureGroup(ctx context.Context, g domain.SecurityGroup) error {
	return observe(ctx, "network.security_group", time.Now(), m.dataplane.ensureGroup(ctx, g))
}

func (m measured) ensureRule(ctx context.Context, g domain.SecurityGroupID, r domain.Rule) error {
	return observe(ctx, "network.rule", time.Now(), m.dataplane.ensureRule(ctx, g, r))
}

func (m measured) ensureElasticIP(ctx context.Context, e domain.ElasticIP) error {
	return observe(ctx, "network.elastic_ip", time.Now(), m.dataplane.ensureElasticIP(ctx, e))
}

func (m measured) ensureNAT(ctx context.Context, e domain.ElasticIP, private netip.Addr) error {
	return observe(ctx, "network.nat", time.Now(), m.dataplane.ensureNAT(ctx, e, private))
}
