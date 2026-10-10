// Package grpcapi serves the VWS network RPCs. It parses resource names and
// specs, and maps errors. CIDR and lifecycle rules stay in the domain.
package grpcapi

import (
	"context"
	"net/netip"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	commonv1 "github.com/popododo0720/vws-proto/gen/go/vws/common/v1"
	networkv1 "github.com/popododo0720/vws-proto/gen/go/vws/network/v1"

	"github.com/popododo0720/vws-network/internal/domain"
	"github.com/popododo0720/vws-network/internal/network"
)

// Register attaches svc to s.
func Register(s *grpc.Server, svc *network.Service) {
	h := &handler{svc: svc}
	networkv1.RegisterVpcServiceServer(s, h)
	networkv1.RegisterSubnetServiceServer(s, h)
	networkv1.RegisterSecurityGroupServiceServer(s, h)
	networkv1.RegisterAddressServiceServer(s, h)
	longrunningpb.RegisterOperationsServer(s, h)
}

// handler implements every network RPC. Embedding the unimplemented servers
// keeps the type forward-compatible, and TestHandlersOverrideUnimplemented
// fails when a method is still the stub.
type handler struct {
	networkv1.UnimplementedVpcServiceServer
	networkv1.UnimplementedSubnetServiceServer
	networkv1.UnimplementedSecurityGroupServiceServer
	networkv1.UnimplementedAddressServiceServer
	longrunningpb.UnimplementedOperationsServer
	svc *network.Service
}

func (h *handler) CreateVpc(ctx context.Context, req *networkv1.CreateVpcRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	project, region, err := parseRegionParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := resourceID(req.GetVpcId()); err != nil {
		return nil, err
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	cidr, err := vpcSpec(req.GetVpc())
	if err != nil {
		return nil, err
	}
	op, err := h.svc.CreateVPC(ctx, callDry(project, req.GetRequestId(), req.GetVpcId(), req.GetValidateOnly()), domain.CreateVPC{
		CIDR: cidr, ID: domain.VPCID(req.GetVpcId()), Region: region,
	})
	if err != nil {
		return nil, toStatus(err, req.GetRequestId())
	}
	return h.toOp(project, req.GetRequestId(), vpcName(project, region, req.GetVpcId()), commonv1.OperationVerb_OPERATION_VERB_CREATE, op)
}

func (h *handler) UpdateVpc(ctx context.Context, req *networkv1.UpdateVpcRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	if err := checkMask(req.GetUpdateMask(), "spec", "spec.ipv4_cidr", "spec.description", "labels"); err != nil {
		return nil, err
	}
	n, err := parseVPCName(req.GetVpc().GetName())
	if err != nil {
		return nil, err
	}
	touch := maskTouches(req.GetUpdateMask(), "spec", "spec.ipv4_cidr")
	var cidr netip.Prefix
	if touch {
		cidr, err = vpcSpec(req.GetVpc())
		if err != nil {
			return nil, err
		}
	} else if err := rejectResource(req.GetVpc().GetLabels(), req.GetVpc().GetSpec().GetDescription(), req.GetVpc().GetEtag()); err != nil {
		return nil, err
	}
	op, err := h.svc.UpdateVPC(ctx, callDry(n.Project, req.GetRequestId(), n.ID, req.GetValidateOnly()), domain.CreateVPC{
		CIDR: cidr, ID: domain.VPCID(n.ID), Region: n.Region, AllowMissing: req.GetAllowMissing(), Keep: !touch,
	})
	if err != nil {
		return nil, toStatus(err, req.GetRequestId())
	}
	return h.toOp(n.Project, req.GetRequestId(), vpcName(n.Project, n.Region, n.ID), commonv1.OperationVerb_OPERATION_VERB_UPDATE, op)
}

func (h *handler) DeleteVpc(ctx context.Context, req *networkv1.DeleteVpcRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	n, err := parseVPCName(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	if err := rejectEtag(req.GetEtag()); err != nil {
		return nil, err
	}
	op, err := h.svc.DeleteVPC(ctx, callOf(n.Project, req.GetRequestId(), n.ID), domain.DeleteVPC{
		VPC: domain.VPCID(n.ID), RejectMissing: !req.GetAllowMissing(), Region: n.Region,
	})
	if err != nil {
		return nil, toStatus(err, req.GetRequestId())
	}
	return h.toOp(n.Project, req.GetRequestId(), vpcName(n.Project, n.Region, n.ID), commonv1.OperationVerb_OPERATION_VERB_DELETE, op)
}

func (h *handler) GetVpc(ctx context.Context, req *networkv1.GetVpcRequest) (*networkv1.Vpc, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	n, err := parseVPCName(req.GetName())
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetVPC(ctx, domain.ProjectID(n.Project), domain.VPCID(n.ID))
	if err != nil {
		return nil, toStatus(err, "")
	}
	if v.Region != "" && v.Region != n.Region {
		return nil, withInfo(codes.NotFound, "vpc not found", "RESOURCE_NOT_FOUND")
	}
	return toVPC(n.Project, v), nil
}

func (h *handler) ListVpcs(ctx context.Context, req *networkv1.ListVpcsRequest) (*networkv1.ListVpcsResponse, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	project, region, err := parseRegionParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	vpcs, err := h.svc.ListVPCs(ctx, domain.ProjectID(project))
	if err != nil {
		return nil, toStatus(err, "")
	}
	var msgs []*networkv1.Vpc
	for _, v := range vpcs {
		if v.Region != "" && v.Region != region {
			continue
		}
		msgs = append(msgs, toVPC(project, v))
	}
	page, next, err := cutPage(msgs, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &networkv1.ListVpcsResponse{Vpcs: page, NextPageToken: next}, nil
}

func (h *handler) ReportVpcStatus(ctx context.Context, req *networkv1.ReportVpcStatusRequest) (*networkv1.ReportVpcStatusResponse, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	if req.GetStatus() == nil {
		return nil, invalidSpec("status is required")
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	// The dataplane worker is the only writer of observed state.
	v, err := h.GetVpc(ctx, &networkv1.GetVpcRequest{Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	return &networkv1.ReportVpcStatusResponse{Vpc: v}, nil
}

func (h *handler) CreateSubnet(ctx context.Context, req *networkv1.CreateSubnetRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	parent, err := parseVPCParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := resourceID(req.GetSubnetId()); err != nil {
		return nil, err
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	cidr, zone, err := subnetSpec(req.GetSubnet())
	if err != nil {
		return nil, err
	}
	op, err := h.svc.CreateSubnet(ctx, callDry(parent.Project, req.GetRequestId(), req.GetSubnetId(), req.GetValidateOnly()), domain.CreateSubnet{
		VPC: domain.VPCID(parent.VPC), CIDR: cidr, Zone: zone, ID: domain.SubnetID(req.GetSubnetId()), Region: parent.Region,
	})
	if err != nil {
		return nil, toStatus(err, req.GetRequestId())
	}
	return h.toOp(parent.Project, req.GetRequestId(), subnetName(parent.Project, parent.Region, parent.VPC, req.GetSubnetId()), commonv1.OperationVerb_OPERATION_VERB_CREATE, op)
}

func (h *handler) UpdateSubnet(ctx context.Context, req *networkv1.UpdateSubnetRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	if err := checkMask(req.GetUpdateMask(), "spec", "spec.zone", "spec.ipv4_cidr", "spec.description", "labels"); err != nil {
		return nil, err
	}
	n, err := parseChildName(req.GetSubnet().GetName(), "subnets")
	if err != nil {
		return nil, err
	}
	touch := maskTouches(req.GetUpdateMask(), "spec", "spec.zone", "spec.ipv4_cidr")
	var cidr netip.Prefix
	var zone string
	if touch {
		cidr, zone, err = subnetSpec(req.GetSubnet())
		if err != nil {
			return nil, err
		}
	} else if err := rejectResource(req.GetSubnet().GetLabels(), req.GetSubnet().GetSpec().GetDescription(), req.GetSubnet().GetEtag()); err != nil {
		return nil, err
	}
	op, err := h.svc.UpdateSubnet(ctx, callDry(n.Project, req.GetRequestId(), n.ID, req.GetValidateOnly()), domain.CreateSubnet{
		VPC: domain.VPCID(n.VPC), CIDR: cidr, Zone: zone, ID: domain.SubnetID(n.ID), Region: n.Region, AllowMissing: req.GetAllowMissing(), Keep: !touch,
	})
	if err != nil {
		return nil, toStatus(err, req.GetRequestId())
	}
	return h.toOp(n.Project, req.GetRequestId(), subnetName(n.Project, n.Region, n.VPC, n.ID), commonv1.OperationVerb_OPERATION_VERB_UPDATE, op)
}

func (h *handler) DeleteSubnet(ctx context.Context, req *networkv1.DeleteSubnetRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	n, err := parseChildName(req.GetName(), "subnets")
	if err != nil {
		return nil, err
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	if err := rejectEtag(req.GetEtag()); err != nil {
		return nil, err
	}
	op, err := h.svc.DeleteSubnet(ctx, callOf(n.Project, req.GetRequestId(), n.ID), domain.DeleteSubnet{
		Subnet: domain.SubnetID(n.ID), RejectMissing: !req.GetAllowMissing(), VPC: domain.VPCID(n.VPC), Region: n.Region,
	})
	if err != nil {
		return nil, toStatus(err, req.GetRequestId())
	}
	return h.toOp(n.Project, req.GetRequestId(), subnetName(n.Project, n.Region, n.VPC, n.ID), commonv1.OperationVerb_OPERATION_VERB_DELETE, op)
}

func (h *handler) GetSubnet(ctx context.Context, req *networkv1.GetSubnetRequest) (*networkv1.Subnet, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	n, err := parseChildName(req.GetName(), "subnets")
	if err != nil {
		return nil, err
	}
	sub, err := h.svc.GetSubnet(ctx, domain.ProjectID(n.Project), domain.SubnetID(n.ID))
	if err != nil {
		return nil, toStatus(err, "")
	}
	if sub.VPC != domain.VPCID(n.VPC) || (sub.Region != "" && sub.Region != n.Region) {
		return nil, withInfo(codes.NotFound, "subnet not found", "RESOURCE_NOT_FOUND")
	}
	return toSubnet(n.Project, sub), nil
}

func (h *handler) ListSubnets(ctx context.Context, req *networkv1.ListSubnetsRequest) (*networkv1.ListSubnetsResponse, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	parent, err := parseVPCParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	subs, err := h.svc.ListSubnets(ctx, domain.ProjectID(parent.Project), domain.VPCID(parent.VPC))
	if err != nil {
		return nil, toStatus(err, "")
	}
	var msgs []*networkv1.Subnet
	for _, sub := range subs {
		if sub.Region != "" && sub.Region != parent.Region {
			continue
		}
		msgs = append(msgs, toSubnet(parent.Project, sub))
	}
	page, next, err := cutPage(msgs, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &networkv1.ListSubnetsResponse{Subnets: page, NextPageToken: next}, nil
}

func (h *handler) ReportSubnetStatus(ctx context.Context, req *networkv1.ReportSubnetStatusRequest) (*networkv1.ReportSubnetStatusResponse, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	if req.GetStatus() == nil {
		return nil, invalidSpec("status is required")
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	sub, err := h.GetSubnet(ctx, &networkv1.GetSubnetRequest{Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	return &networkv1.ReportSubnetStatusResponse{Subnet: sub}, nil
}

func (h *handler) CreateSecurityGroup(ctx context.Context, req *networkv1.CreateSecurityGroupRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	parent, err := parseVPCParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := resourceID(req.GetSecurityGroupId()); err != nil {
		return nil, err
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	rules, err := groupSpec(req.GetSecurityGroup(), parent.Project, parent.Region, parent.VPC)
	if err != nil {
		return nil, err
	}
	op, err := h.svc.CreateSecurityGroup(ctx, callDry(parent.Project, req.GetRequestId(), req.GetSecurityGroupId(), req.GetValidateOnly()), domain.CreateSecurityGroup{
		VPC: domain.VPCID(parent.VPC), Name: req.GetSecurityGroupId(), ID: domain.SecurityGroupID(req.GetSecurityGroupId()),
		Region: parent.Region, Rules: rules, ReplaceRules: true,
	})
	if err != nil {
		return nil, toStatus(err, req.GetRequestId())
	}
	return h.toOp(parent.Project, req.GetRequestId(), groupName(parent.Project, parent.Region, parent.VPC, req.GetSecurityGroupId()), commonv1.OperationVerb_OPERATION_VERB_CREATE, op)
}

func (h *handler) UpdateSecurityGroup(ctx context.Context, req *networkv1.UpdateSecurityGroupRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	if err := checkMask(req.GetUpdateMask(), "spec", "spec.rules", "spec.description", "labels"); err != nil {
		return nil, err
	}
	n, err := parseChildName(req.GetSecurityGroup().GetName(), "securityGroups")
	if err != nil {
		return nil, err
	}
	touch := maskTouches(req.GetUpdateMask(), "spec", "spec.rules")
	var rules []domain.Rule
	if touch {
		rules, err = groupSpec(req.GetSecurityGroup(), n.Project, n.Region, n.VPC)
		if err != nil {
			return nil, err
		}
	} else if err := rejectResource(req.GetSecurityGroup().GetLabels(), req.GetSecurityGroup().GetSpec().GetDescription(), req.GetSecurityGroup().GetEtag()); err != nil {
		return nil, err
	}
	op, err := h.svc.UpdateSecurityGroup(ctx, callDry(n.Project, req.GetRequestId(), n.ID, req.GetValidateOnly()), domain.CreateSecurityGroup{
		VPC: domain.VPCID(n.VPC), Name: n.ID, ID: domain.SecurityGroupID(n.ID), Region: n.Region,
		Rules: rules, ReplaceRules: touch, AllowMissing: req.GetAllowMissing(), KeepRules: !touch,
	})
	if err != nil {
		return nil, toStatus(err, req.GetRequestId())
	}
	return h.toOp(n.Project, req.GetRequestId(), groupName(n.Project, n.Region, n.VPC, n.ID), commonv1.OperationVerb_OPERATION_VERB_UPDATE, op)
}

func (h *handler) DeleteSecurityGroup(ctx context.Context, req *networkv1.DeleteSecurityGroupRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	n, err := parseChildName(req.GetName(), "securityGroups")
	if err != nil {
		return nil, err
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	if err := rejectEtag(req.GetEtag()); err != nil {
		return nil, err
	}
	op, err := h.svc.DeleteSecurityGroup(ctx, callOf(n.Project, req.GetRequestId(), n.ID), domain.DeleteSecurityGroup{
		Group: domain.SecurityGroupID(n.ID), RejectMissing: !req.GetAllowMissing(), VPC: domain.VPCID(n.VPC), Region: n.Region,
	})
	if err != nil {
		return nil, toStatus(err, req.GetRequestId())
	}
	return h.toOp(n.Project, req.GetRequestId(), groupName(n.Project, n.Region, n.VPC, n.ID), commonv1.OperationVerb_OPERATION_VERB_DELETE, op)
}

func (h *handler) GetSecurityGroup(ctx context.Context, req *networkv1.GetSecurityGroupRequest) (*networkv1.SecurityGroup, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	n, err := parseChildName(req.GetName(), "securityGroups")
	if err != nil {
		return nil, err
	}
	g, err := h.svc.GetSecurityGroup(ctx, domain.ProjectID(n.Project), domain.SecurityGroupID(n.ID))
	if err != nil {
		return nil, toStatus(err, "")
	}
	if g.Default || g.VPC != domain.VPCID(n.VPC) || (g.Region != "" && g.Region != n.Region) {
		return nil, withInfo(codes.NotFound, "security group not found", "RESOURCE_NOT_FOUND")
	}
	return toGroup(n.Project, g), nil
}

func (h *handler) ListSecurityGroups(ctx context.Context, req *networkv1.ListSecurityGroupsRequest) (*networkv1.ListSecurityGroupsResponse, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	parent, err := parseVPCParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	groups, err := h.svc.ListSecurityGroups(ctx, domain.ProjectID(parent.Project), domain.VPCID(parent.VPC))
	if err != nil {
		return nil, toStatus(err, "")
	}
	var msgs []*networkv1.SecurityGroup
	for _, g := range groups {
		if g.Default || (g.Region != "" && g.Region != parent.Region) {
			continue
		}
		msgs = append(msgs, toGroup(parent.Project, g))
	}
	page, next, err := cutPage(msgs, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &networkv1.ListSecurityGroupsResponse{SecurityGroups: page, NextPageToken: next}, nil
}

func (h *handler) ReportSecurityGroupStatus(ctx context.Context, req *networkv1.ReportSecurityGroupStatusRequest) (*networkv1.ReportSecurityGroupStatusResponse, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	if req.GetStatus() == nil {
		return nil, invalidSpec("status is required")
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	g, err := h.GetSecurityGroup(ctx, &networkv1.GetSecurityGroupRequest{Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	return &networkv1.ReportSecurityGroupStatusResponse{SecurityGroup: g}, nil
}

func (h *handler) CreateAddress(ctx context.Context, req *networkv1.CreateAddressRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	if _, _, err := parseRegionParent(req.GetParent()); err != nil {
		return nil, err
	}
	if err := resourceID(req.GetAddressId()); err != nil {
		return nil, err
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	if err := addressSpec(req.GetAddress()); err != nil {
		return nil, err
	}
	if req.GetAddress().GetSpec().GetAssociation() != nil {
		return nil, invalidSpec("association names a compute instance")
	}
	// This service allocates a pool address inside a VPC. The request has no VPC.
	return nil, invalidSpec("an address is allocated in a vpc")
}

func (h *handler) UpdateAddress(ctx context.Context, req *networkv1.UpdateAddressRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	if err := checkMask(req.GetUpdateMask(), "spec", "spec.association", "spec.ip_version", "labels"); err != nil {
		return nil, err
	}
	n, err := parseAddressName(req.GetAddress().GetName())
	if err != nil {
		return nil, err
	}
	touch := maskTouches(req.GetUpdateMask(), "spec", "spec.association")
	if touch {
		if err := addressSpec(req.GetAddress()); err != nil {
			return nil, err
		}
		if req.GetAddress().GetSpec().GetAssociation() != nil {
			return nil, invalidSpec("association names a compute instance")
		}
	} else if err := rejectResource(req.GetAddress().GetLabels(), "", req.GetAddress().GetEtag()); err != nil {
		return nil, err
	}
	op, err := h.svc.DisassociateElasticIP(ctx, callDry(n.Project, req.GetRequestId(), n.ID, req.GetValidateOnly()), domain.DisassociateElasticIP{
		ElasticIP: domain.ElasticIPID(n.ID), RejectMissing: !req.GetAllowMissing(), AllowMissing: req.GetAllowMissing(), Region: n.Region, Keep: !touch,
	})
	if err != nil {
		return nil, toStatus(err, req.GetRequestId())
	}
	return h.toOp(n.Project, req.GetRequestId(), addressName(n.Project, n.Region, n.ID), commonv1.OperationVerb_OPERATION_VERB_UPDATE, op)
}

func (h *handler) DeleteAddress(ctx context.Context, req *networkv1.DeleteAddressRequest) (*longrunningpb.Operation, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	n, err := parseAddressName(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	if err := rejectEtag(req.GetEtag()); err != nil {
		return nil, err
	}
	op, err := h.svc.ReleaseElasticIP(ctx, callOf(n.Project, req.GetRequestId(), n.ID), domain.ReleaseElasticIP{
		ElasticIP: domain.ElasticIPID(n.ID), RejectMissing: !req.GetAllowMissing(), Region: n.Region,
	})
	if err != nil {
		return nil, toStatus(err, req.GetRequestId())
	}
	return h.toOp(n.Project, req.GetRequestId(), addressName(n.Project, n.Region, n.ID), commonv1.OperationVerb_OPERATION_VERB_DELETE, op)
}

func (h *handler) GetAddress(ctx context.Context, req *networkv1.GetAddressRequest) (*networkv1.Address, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	n, err := parseAddressName(req.GetName())
	if err != nil {
		return nil, err
	}
	e, err := h.svc.GetElasticIP(ctx, domain.ProjectID(n.Project), domain.ElasticIPID(n.ID))
	if err != nil {
		return nil, toStatus(err, "")
	}
	if e.Region != "" && e.Region != n.Region {
		return nil, withInfo(codes.NotFound, "address not found", "RESOURCE_NOT_FOUND")
	}
	return toAddress(n.Project, n.Region, e), nil
}

func (h *handler) ListAddresses(ctx context.Context, req *networkv1.ListAddressesRequest) (*networkv1.ListAddressesResponse, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	project, region, err := parseRegionParent(req.GetParent())
	if err != nil {
		return nil, err
	}
	eips, err := h.svc.ListElasticIPs(ctx, domain.ProjectID(project), "")
	if err != nil {
		return nil, toStatus(err, "")
	}
	var msgs []*networkv1.Address
	for _, e := range eips {
		if e.Region != "" && e.Region != region {
			continue
		}
		msgs = append(msgs, toAddress(project, region, e))
	}
	page, next, err := cutPage(msgs, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &networkv1.ListAddressesResponse{Addresses: page, NextPageToken: next}, nil
}

func (h *handler) ReportAddressStatus(ctx context.Context, req *networkv1.ReportAddressStatusRequest) (*networkv1.ReportAddressStatusResponse, error) {
	if req == nil {
		return nil, requiredRequest()
	}
	if req.GetStatus() == nil {
		return nil, invalidSpec("status is required")
	}
	if err := parseRequestID(req.GetRequestId()); err != nil {
		return nil, err
	}
	addr, err := h.GetAddress(ctx, &networkv1.GetAddressRequest{Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	return &networkv1.ReportAddressStatusResponse{Address: addr}, nil
}

func vpcSpec(v *networkv1.Vpc) (netip.Prefix, error) {
	if v == nil || v.GetSpec() == nil {
		return netip.Prefix{}, invalidSpec("vpc spec is required")
	}
	if err := rejectText(v.GetLabels(), v.GetSpec().GetDescription()); err != nil {
		return netip.Prefix{}, err
	}
	if err := rejectEtag(v.GetEtag()); err != nil {
		return netip.Prefix{}, err
	}
	return parsePrefix("ipv4_cidr", v.GetSpec().GetIpv4Cidr())
}

func subnetSpec(s *networkv1.Subnet) (netip.Prefix, string, error) {
	if s == nil || s.GetSpec() == nil {
		return netip.Prefix{}, "", invalidSpec("subnet spec is required")
	}
	if err := rejectText(s.GetLabels(), s.GetSpec().GetDescription()); err != nil {
		return netip.Prefix{}, "", err
	}
	if err := rejectEtag(s.GetEtag()); err != nil {
		return netip.Prefix{}, "", err
	}
	if err := resourceID(s.GetSpec().GetZone()); err != nil {
		return netip.Prefix{}, "", err
	}
	cidr, err := parsePrefix("ipv4_cidr", s.GetSpec().GetIpv4Cidr())
	if err != nil {
		return netip.Prefix{}, "", err
	}
	return cidr, s.GetSpec().GetZone(), nil
}

func groupSpec(g *networkv1.SecurityGroup, project, region, vpc string) ([]domain.Rule, error) {
	if g == nil || g.GetSpec() == nil {
		return nil, invalidSpec("security group spec is required")
	}
	if err := rejectText(g.GetLabels(), g.GetSpec().GetDescription()); err != nil {
		return nil, err
	}
	if err := rejectEtag(g.GetEtag()); err != nil {
		return nil, err
	}
	return parseRules(g.GetSpec().GetRules(), project, region, vpc)
}

func addressSpec(a *networkv1.Address) error {
	if a == nil || a.GetSpec() == nil {
		return invalidSpec("address spec is required")
	}
	if err := rejectText(a.GetLabels(), ""); err != nil {
		return err
	}
	if err := rejectEtag(a.GetEtag()); err != nil {
		return err
	}
	if a.GetSpec().GetIpVersion() != networkv1.IpVersion_IP_VERSION_IPV4 {
		return invalidSpec("ip_version must be ipv4")
	}
	return nil
}

func rejectResource(labels map[string]string, description, etag string) error {
	if err := rejectText(labels, description); err != nil {
		return err
	}
	return rejectEtag(etag)
}

func checkMask(mask *fieldmaskpb.FieldMask, allowed ...string) error {
	for _, path := range maskPaths(mask) {
		if err := allowPath(path, allowed...); err != nil {
			return err
		}
	}
	return nil
}
