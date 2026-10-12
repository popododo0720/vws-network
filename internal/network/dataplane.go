package network

import (
	"context"
	"net/netip"

	"github.com/popododo0720/vws-network/internal/domain"
)

// dataplane is the OVN boundary. Every method is idempotent: ensuring a
// present object and deleting an absent object both succeed.
type dataplane interface {
	ensureVPC(ctx context.Context, v domain.VPC) error
	deleteVPC(ctx context.Context, v domain.VPC) error
	ensureSubnet(ctx context.Context, s domain.Subnet) error
	deleteSubnet(ctx context.Context, s domain.Subnet) error
	ensureGroup(ctx context.Context, g domain.SecurityGroup) error
	deleteGroup(ctx context.Context, g domain.SecurityGroup) error
	ensureRule(ctx context.Context, g domain.SecurityGroupID, r domain.Rule) error
	deleteRule(ctx context.Context, g domain.SecurityGroupID, r domain.Rule) error
	ensureElasticIP(ctx context.Context, e domain.ElasticIP) error
	deleteElasticIP(ctx context.Context, e domain.ElasticIP) error
	ensureNAT(ctx context.Context, e domain.ElasticIP, private netip.Addr) error
	deleteNAT(ctx context.Context, e domain.ElasticIP, private netip.Addr) error
	// ready reads, never writes, to prove the dataplane can be reached.
	ready(ctx context.Context) error
}
