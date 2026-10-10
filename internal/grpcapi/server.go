// Package grpcapi serves NetworkService. It parses wire values and maps
// errors. CIDR and lifecycle rules stay in the domain.
package grpcapi

import (
	"context"
	"errors"
	"net/netip"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	networkv1 "github.com/popododo0720/vws-network/gen/network/v1"
	"github.com/popododo0720/vws-network/internal/domain"
	"github.com/popododo0720/vws-network/internal/network"
)

// Register attaches svc to s.
func Register(s *grpc.Server, svc *network.Service) {
	networkv1.RegisterNetworkServiceServer(s, &handler{svc: svc})
}

// handler implements every RPC. Embedding the unimplemented server keeps the
// type forward-compatible, and TestHandlersOverrideUnimplemented fails when a
// method is still the stub.
type handler struct {
	networkv1.UnimplementedNetworkServiceServer
	svc *network.Service
}

func (h *handler) CreateVpc(ctx context.Context, req *networkv1.CreateVpcRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	cidr, err := parsePrefix("cidr_block", req.GetCidrBlock())
	if err != nil {
		return nil, err
	}
	op, err := h.svc.CreateVPC(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.CreateVPC{CIDR: cidr})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) DeleteVpc(ctx context.Context, req *networkv1.DeleteVpcRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	op, err := h.svc.DeleteVPC(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.DeleteVPC{VPC: domain.VPCID(req.GetVpcId())})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) GetVpc(ctx context.Context, req *networkv1.GetVpcRequest) (*networkv1.Vpc, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	v, err := h.svc.GetVPC(ctx, domain.ProjectID(req.GetProjectId()), domain.VPCID(req.GetVpcId()))
	if err != nil {
		return nil, toStatus(err)
	}
	return toVPC(domain.ProjectID(req.GetProjectId()), v), nil
}

func (h *handler) ListVpcs(ctx context.Context, req *networkv1.ListVpcsRequest) (*networkv1.ListVpcsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	vpcs, err := h.svc.ListVPCs(ctx, domain.ProjectID(req.GetProjectId()))
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.ListVpcsResponse{}
	for _, v := range vpcs {
		out.Vpcs = append(out.Vpcs, toVPC(domain.ProjectID(req.GetProjectId()), v))
	}
	return out, nil
}

func (h *handler) CreateSubnet(ctx context.Context, req *networkv1.CreateSubnetRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	cidr, err := parsePrefix("cidr_block", req.GetCidrBlock())
	if err != nil {
		return nil, err
	}
	op, err := h.svc.CreateSubnet(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.CreateSubnet{
		VPC: domain.VPCID(req.GetVpcId()), CIDR: cidr, Zone: req.GetAvailabilityZone(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) DeleteSubnet(ctx context.Context, req *networkv1.DeleteSubnetRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	op, err := h.svc.DeleteSubnet(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.DeleteSubnet{Subnet: domain.SubnetID(req.GetSubnetId())})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) GetSubnet(ctx context.Context, req *networkv1.GetSubnetRequest) (*networkv1.Subnet, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	sub, err := h.svc.GetSubnet(ctx, domain.ProjectID(req.GetProjectId()), domain.SubnetID(req.GetSubnetId()))
	if err != nil {
		return nil, toStatus(err)
	}
	return toSubnet(domain.ProjectID(req.GetProjectId()), sub), nil
}

func (h *handler) ListSubnets(ctx context.Context, req *networkv1.ListSubnetsRequest) (*networkv1.ListSubnetsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	subs, err := h.svc.ListSubnets(ctx, domain.ProjectID(req.GetProjectId()), domain.VPCID(req.GetVpcId()))
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.ListSubnetsResponse{}
	for _, sub := range subs {
		out.Subnets = append(out.Subnets, toSubnet(domain.ProjectID(req.GetProjectId()), sub))
	}
	return out, nil
}

func (h *handler) CreateSecurityGroup(ctx context.Context, req *networkv1.CreateSecurityGroupRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	op, err := h.svc.CreateSecurityGroup(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.CreateSecurityGroup{
		VPC: domain.VPCID(req.GetVpcId()), Name: req.GetName(),
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) DeleteSecurityGroup(ctx context.Context, req *networkv1.DeleteSecurityGroupRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	op, err := h.svc.DeleteSecurityGroup(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.DeleteSecurityGroup{Group: domain.SecurityGroupID(req.GetSecurityGroupId())})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) AuthorizeSecurityGroupRules(ctx context.Context, req *networkv1.AuthorizeSecurityGroupRulesRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	rules, err := parseRules(req.GetRules())
	if err != nil {
		return nil, err
	}
	op, err := h.svc.AuthorizeRules(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.AuthorizeRules{
		Group: domain.SecurityGroupID(req.GetSecurityGroupId()), Rules: rules,
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) RevokeSecurityGroupRules(ctx context.Context, req *networkv1.RevokeSecurityGroupRulesRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	rules, err := parseRules(req.GetRules())
	if err != nil {
		return nil, err
	}
	op, err := h.svc.RevokeRules(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.RevokeRules{
		Group: domain.SecurityGroupID(req.GetSecurityGroupId()), Rules: rules,
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) GetSecurityGroup(ctx context.Context, req *networkv1.GetSecurityGroupRequest) (*networkv1.SecurityGroup, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	g, err := h.svc.GetSecurityGroup(ctx, domain.ProjectID(req.GetProjectId()), domain.SecurityGroupID(req.GetSecurityGroupId()))
	if err != nil {
		return nil, toStatus(err)
	}
	return toGroup(domain.ProjectID(req.GetProjectId()), g), nil
}

func (h *handler) ListSecurityGroups(ctx context.Context, req *networkv1.ListSecurityGroupsRequest) (*networkv1.ListSecurityGroupsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	groups, err := h.svc.ListSecurityGroups(ctx, domain.ProjectID(req.GetProjectId()), domain.VPCID(req.GetVpcId()))
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.ListSecurityGroupsResponse{}
	for _, g := range groups {
		out.SecurityGroups = append(out.SecurityGroups, toGroup(domain.ProjectID(req.GetProjectId()), g))
	}
	return out, nil
}

func (h *handler) AllocateElasticIp(ctx context.Context, req *networkv1.AllocateElasticIpRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	op, err := h.svc.AllocateElasticIP(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.AllocateElasticIP{VPC: domain.VPCID(req.GetVpcId())})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) ReleaseElasticIp(ctx context.Context, req *networkv1.ReleaseElasticIpRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	op, err := h.svc.ReleaseElasticIP(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.ReleaseElasticIP{ElasticIP: domain.ElasticIPID(req.GetElasticIpId())})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) AssociateElasticIp(ctx context.Context, req *networkv1.AssociateElasticIpRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	addr, err := parseAddr("private_address", req.GetPrivateAddress())
	if err != nil {
		return nil, err
	}
	op, err := h.svc.AssociateElasticIP(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.AssociateElasticIP{
		ElasticIP: domain.ElasticIPID(req.GetElasticIpId()), Private: addr,
	})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) DisassociateElasticIp(ctx context.Context, req *networkv1.DisassociateElasticIpRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	op, err := h.svc.DisassociateElasticIP(ctx, callOf(req.GetProjectId(), req.GetIdempotencyKey()), domain.DisassociateElasticIP{ElasticIP: domain.ElasticIPID(req.GetElasticIpId())})
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func (h *handler) GetElasticIp(ctx context.Context, req *networkv1.GetElasticIpRequest) (*networkv1.ElasticIp, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	e, err := h.svc.GetElasticIP(ctx, domain.ProjectID(req.GetProjectId()), domain.ElasticIPID(req.GetElasticIpId()))
	if err != nil {
		return nil, toStatus(err)
	}
	return toElasticIP(domain.ProjectID(req.GetProjectId()), e), nil
}

func (h *handler) ListElasticIps(ctx context.Context, req *networkv1.ListElasticIpsRequest) (*networkv1.ListElasticIpsResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	eips, err := h.svc.ListElasticIPs(ctx, domain.ProjectID(req.GetProjectId()), domain.VPCID(req.GetVpcId()))
	if err != nil {
		return nil, toStatus(err)
	}
	out := &networkv1.ListElasticIpsResponse{}
	for _, e := range eips {
		out.ElasticIps = append(out.ElasticIps, toElasticIP(domain.ProjectID(req.GetProjectId()), e))
	}
	return out, nil
}

func (h *handler) GetOperation(ctx context.Context, req *networkv1.GetOperationRequest) (*networkv1.Operation, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	op, err := h.svc.GetOperation(ctx, domain.ProjectID(req.GetProjectId()), req.GetOperationId())
	if err != nil {
		return nil, toStatus(err)
	}
	return toOperation(op), nil
}

func callOf(project, key string) domain.Call {
	return domain.Call{Project: domain.ProjectID(project), Key: domain.IdempotencyKey(key)}
}

func parsePrefix(field, raw string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(raw)
	if err != nil || !p.IsValid() || !p.Addr().Is4() {
		return netip.Prefix{}, status.Errorf(codes.InvalidArgument, "%s is not an ipv4 prefix", field)
	}
	return p, nil
}

func parseAddr(field, raw string) (netip.Addr, error) {
	a, err := netip.ParseAddr(raw)
	if err != nil || !a.Is4() {
		return netip.Addr{}, status.Errorf(codes.InvalidArgument, "%s is not an ipv4 address", field)
	}
	return a, nil
}

func parseRules(rs []*networkv1.Rule) ([]domain.Rule, error) {
	out := make([]domain.Rule, 0, len(rs))
	for _, r := range rs {
		if r == nil {
			return nil, status.Error(codes.InvalidArgument, "rule is required")
		}
		dir, err := parseDirection(r.GetDirection())
		if err != nil {
			return nil, err
		}
		proto, err := parseProtocol(r.GetProtocol())
		if err != nil {
			return nil, err
		}
		rule := domain.Rule{Direction: dir, Protocol: proto, FromPort: r.GetFromPort(), ToPort: r.GetToPort()}
		switch peer := r.Peer.(type) {
		case *networkv1.Rule_CidrBlock:
			cidr, err := parsePrefix("cidr_block", peer.CidrBlock)
			if err != nil {
				return nil, err
			}
			rule.Peer.CIDR = cidr
		case *networkv1.Rule_SecurityGroupId:
			if peer.SecurityGroupId == "" {
				return nil, status.Error(codes.InvalidArgument, "security group peer is required")
			}
			rule.Peer.Group = domain.SecurityGroupID(peer.SecurityGroupId)
		default:
			return nil, status.Error(codes.InvalidArgument, "rule peer is required")
		}
		out = append(out, rule)
	}
	return out, nil
}

func parseDirection(d networkv1.Direction) (domain.Direction, error) {
	switch d {
	case networkv1.Direction_DIRECTION_INGRESS:
		return domain.Ingress, nil
	case networkv1.Direction_DIRECTION_EGRESS:
		return domain.Egress, nil
	default:
		return 0, status.Error(codes.InvalidArgument, "rule direction is required")
	}
}

func parseProtocol(p networkv1.Protocol) (domain.Protocol, error) {
	switch p {
	case networkv1.Protocol_PROTOCOL_TCP:
		return domain.TCP, nil
	case networkv1.Protocol_PROTOCOL_UDP:
		return domain.UDP, nil
	case networkv1.Protocol_PROTOCOL_ICMP:
		return domain.ICMP, nil
	case networkv1.Protocol_PROTOCOL_ALL:
		return domain.All, nil
	default:
		return 0, status.Error(codes.InvalidArgument, "rule protocol is required")
	}
}

func toStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok && status.Code(err) != codes.Unknown {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		return status.Error(codes.Internal, "internal error")
	}
	return status.Error(codeOf(de.Kind), de.Msg)
}

func codeOf(k domain.ErrKind) codes.Code {
	switch k {
	case domain.Invalid:
		return codes.InvalidArgument
	case domain.NotFound:
		return codes.NotFound
	case domain.Conflict:
		return codes.AlreadyExists
	case domain.Precondition:
		return codes.FailedPrecondition
	case domain.Exhausted:
		return codes.ResourceExhausted
	case domain.Dataplane:
		return codes.Unavailable
	case domain.Busy:
		return codes.Aborted
	default:
		return codes.Internal
	}
}

func toOperation(op domain.Operation) *networkv1.Operation {
	out := &networkv1.Operation{
		Id:             op.ID,
		ProjectId:      string(op.Project),
		Type:           string(op.Type),
		ResourceId:     op.Resource,
		State:          toOpState(op.State),
		IdempotencyKey: string(op.Key),
	}
	if op.Err != nil {
		out.ErrorMessage = op.Err.Msg
	}
	return out
}

func toOpState(s domain.OpState) networkv1.Operation_State {
	switch s {
	case domain.OpPending:
		return networkv1.Operation_PENDING
	case domain.OpRunning:
		return networkv1.Operation_RUNNING
	case domain.OpSucceeded:
		return networkv1.Operation_SUCCEEDED
	case domain.OpFailed:
		return networkv1.Operation_FAILED
	default:
		return networkv1.Operation_STATE_UNSPECIFIED
	}
}

func toStatusValue(s domain.Status) networkv1.ResourceStatus {
	switch s {
	case domain.StatusPending:
		return networkv1.ResourceStatus_RESOURCE_STATUS_PENDING
	case domain.StatusAvailable:
		return networkv1.ResourceStatus_RESOURCE_STATUS_AVAILABLE
	case domain.StatusDeleting:
		return networkv1.ResourceStatus_RESOURCE_STATUS_DELETING
	case domain.StatusFailed:
		return networkv1.ResourceStatus_RESOURCE_STATUS_FAILED
	default:
		return networkv1.ResourceStatus_RESOURCE_STATUS_UNSPECIFIED
	}
}

func toVPC(p domain.ProjectID, v domain.VPC) *networkv1.Vpc {
	return &networkv1.Vpc{
		Id:                     string(v.ID),
		ProjectId:              string(p),
		CidrBlock:              v.CIDR.String(),
		DefaultSecurityGroupId: string(v.DefaultGroup),
		Status:                 toStatusValue(v.Status),
	}
}

func toSubnet(p domain.ProjectID, s domain.Subnet) *networkv1.Subnet {
	return &networkv1.Subnet{
		Id:               string(s.ID),
		ProjectId:        string(p),
		VpcId:            string(s.VPC),
		CidrBlock:        s.CIDR.String(),
		AvailabilityZone: s.Zone,
		Status:           toStatusValue(s.Status),
	}
}

func toGroup(p domain.ProjectID, g domain.SecurityGroup) *networkv1.SecurityGroup {
	out := &networkv1.SecurityGroup{
		Id:        string(g.ID),
		ProjectId: string(p),
		VpcId:     string(g.VPC),
		Name:      g.Name,
		IsDefault: g.Default,
		Status:    toStatusValue(g.Status),
	}
	for _, rule := range g.Rules {
		out.Rules = append(out.Rules, toRule(rule))
	}
	return out
}

func toElasticIP(p domain.ProjectID, e domain.ElasticIP) *networkv1.ElasticIp {
	out := &networkv1.ElasticIp{
		Id:        string(e.ID),
		ProjectId: string(p),
		VpcId:     string(e.VPC),
		Address:   e.Address.String(),
		SubnetId:  string(e.Subnet),
		Status:    toStatusValue(e.Status),
	}
	if e.Private.IsValid() {
		out.PrivateAddress = e.Private.String()
	}
	return out
}

func toRule(r domain.Rule) *networkv1.Rule {
	out := &networkv1.Rule{
		Direction: toDirection(r.Direction),
		Protocol:  toProtocol(r.Protocol),
		FromPort:  r.FromPort,
		ToPort:    r.ToPort,
	}
	if r.Peer.Group != "" {
		out.Peer = &networkv1.Rule_SecurityGroupId{SecurityGroupId: string(r.Peer.Group)}
		return out
	}
	out.Peer = &networkv1.Rule_CidrBlock{CidrBlock: r.Peer.CIDR.String()}
	return out
}

func toDirection(d domain.Direction) networkv1.Direction {
	switch d {
	case domain.Ingress:
		return networkv1.Direction_DIRECTION_INGRESS
	case domain.Egress:
		return networkv1.Direction_DIRECTION_EGRESS
	default:
		return networkv1.Direction_DIRECTION_UNSPECIFIED
	}
}

func toProtocol(p domain.Protocol) networkv1.Protocol {
	switch p {
	case domain.TCP:
		return networkv1.Protocol_PROTOCOL_TCP
	case domain.UDP:
		return networkv1.Protocol_PROTOCOL_UDP
	case domain.ICMP:
		return networkv1.Protocol_PROTOCOL_ICMP
	case domain.All:
		return networkv1.Protocol_PROTOCOL_ALL
	default:
		return networkv1.Protocol_PROTOCOL_UNSPECIFIED
	}
}
