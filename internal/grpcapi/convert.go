package grpcapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	commonv1 "github.com/popododo0720/vws-proto/gen/go/vws/common/v1"
	networkv1 "github.com/popododo0720/vws-proto/gen/go/vws/network/v1"

	"github.com/popododo0720/vws-network/internal/domain"
)

// ResourceId is [a-z]([a-z0-9-]{0,61}[a-z0-9])?.
var resourceIDPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)

func resourceID(raw string) error {
	if !resourceIDPattern.MatchString(raw) {
		return invalidID("resource id is invalid")
	}
	return nil
}

func parseRequestID(raw string) error {
	if raw == "" {
		return nil
	}
	if !uuid4(raw) {
		return invalidSpec("request_id is not a uuid")
	}
	return nil
}

func uuid4(raw string) bool {
	if len(raw) != 36 {
		return false
	}
	for i := 0; i < len(raw); i++ {
		switch i {
		case 8, 13, 18, 23:
			if raw[i] != '-' {
				return false
			}
		case 14:
			if raw[i] != '4' {
				return false
			}
		case 19:
			switch raw[i] {
			case '8', '9', 'a', 'b', 'A', 'B':
			default:
				return false
			}
		default:
			if !hexDigit(raw[i]) {
				return false
			}
		}
	}
	return true
}

func hexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

type name struct {
	Project string
	Region  string
	VPC     string
	ID      string
}

func parseRegionParent(parent string) (project, region string, err error) {
	parts := strings.Split(parent, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "regions" {
		return "", "", invalidID("parent is invalid")
	}
	if err := resourceID(parts[1]); err != nil {
		return "", "", err
	}
	if err := resourceID(parts[3]); err != nil {
		return "", "", err
	}
	return parts[1], parts[3], nil
}

func parseVPCParent(parent string) (name, error) {
	parts := strings.Split(parent, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "regions" || parts[4] != "vpcs" {
		return name{}, invalidID("parent is invalid")
	}
	for _, id := range []string{parts[1], parts[3], parts[5]} {
		if err := resourceID(id); err != nil {
			return name{}, err
		}
	}
	return name{Project: parts[1], Region: parts[3], VPC: parts[5]}, nil
}

func parseVPCName(raw string) (name, error) {
	n, id, err := splitName(raw, "vpcs", 6)
	if err != nil {
		return name{}, err
	}
	n.ID = id
	return n, nil
}

func parseChildName(raw, collection string) (name, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 8 || parts[0] != "projects" || parts[2] != "regions" || parts[4] != "vpcs" || parts[6] != collection {
		return name{}, invalidID("name is invalid")
	}
	for _, id := range []string{parts[1], parts[3], parts[5], parts[7]} {
		if err := resourceID(id); err != nil {
			return name{}, err
		}
	}
	return name{Project: parts[1], Region: parts[3], VPC: parts[5], ID: parts[7]}, nil
}

func parseAddressName(raw string) (name, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "regions" || parts[4] != "addresses" {
		return name{}, invalidID("name is invalid")
	}
	for _, id := range []string{parts[1], parts[3], parts[5]} {
		if err := resourceID(id); err != nil {
			return name{}, err
		}
	}
	return name{Project: parts[1], Region: parts[3], ID: parts[5]}, nil
}

func splitName(raw, collection string, n int) (name, string, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != n || parts[0] != "projects" || parts[2] != "regions" || parts[4] != collection {
		return name{}, "", invalidID("name is invalid")
	}
	for _, id := range []string{parts[1], parts[3], parts[5]} {
		if err := resourceID(id); err != nil {
			return name{}, "", err
		}
	}
	return name{Project: parts[1], Region: parts[3]}, parts[5], nil
}

func parseOpName(raw string) (domain.ProjectID, string, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "operations" || parts[1] == "" || parts[3] == "" {
		return "", "", invalidID("operation name is invalid")
	}
	if err := resourceID(parts[1]); err != nil {
		return "", "", err
	}
	return domain.ProjectID(parts[1]), parts[3], nil
}

func vpcName(project, region, id string) string {
	return "projects/" + project + "/regions/" + region + "/vpcs/" + id
}

func subnetName(project, region, vpc, id string) string {
	return vpcName(project, region, vpc) + "/subnets/" + id
}

func groupName(project, region, vpc, id string) string {
	return vpcName(project, region, vpc) + "/securityGroups/" + id
}

func addressName(project, region, id string) string {
	return "projects/" + project + "/regions/" + region + "/addresses/" + id
}

func callOf(project, requestID, resourceID string) domain.Call {
	return callDry(project, requestID, resourceID, false)
}

func callDry(project, requestID, resourceID string, dry bool) domain.Call {
	key := requestID
	if key == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			key = "req-" + resourceID + "-" + hex.EncodeToString(b[:])
		} else {
			key = "req-" + hex.EncodeToString(b[:])
		}
	}
	return domain.Call{Project: domain.ProjectID(project), Key: domain.IdempotencyKey(key), DryRun: dry}
}

func rejectText(labels map[string]string, description string) error {
	if description != "" || len(labels) > 0 {
		return invalidSpec("labels and description are not stored")
	}
	return nil
}

func rejectEtag(etag string) error {
	if etag != "" {
		return withInfo(codes.Aborted, "etag does not match", "ETAG_MISMATCH")
	}
	return nil
}

func maskPaths(mask *fieldmaskpb.FieldMask) []string {
	if mask == nil || len(mask.GetPaths()) == 0 {
		return []string{"spec", "labels"}
	}
	return mask.GetPaths()
}

func maskTouches(mask *fieldmaskpb.FieldMask, paths ...string) bool {
	for _, path := range maskPaths(mask) {
		for _, want := range paths {
			if path == want || strings.HasPrefix(path, want+".") {
				return true
			}
		}
	}
	return false
}

func allowPath(path string, ok ...string) error {
	for _, allowed := range ok {
		if path == allowed {
			return nil
		}
	}
	return invalidSpec("update_mask path is not stored")
}

func parsePrefix(field, raw string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(raw)
	if err != nil || !p.IsValid() || !p.Addr().Is4() {
		return netip.Prefix{}, invalidSpec(field + " is not an ipv4 prefix")
	}
	return p, nil
}

func parseRules(rs []*networkv1.SecurityGroupRule, project, region, vpc string) ([]domain.Rule, error) {
	seen := make(map[string]bool, len(rs))
	out := make([]domain.Rule, 0, len(rs))
	for _, r := range rs {
		if r == nil {
			return nil, invalidSpec("rule is required")
		}
		if err := resourceID(r.GetId()); err != nil {
			return nil, err
		}
		if seen[r.GetId()] {
			return nil, invalidSpec("duplicate rule id")
		}
		seen[r.GetId()] = true
		if r.GetDescription() != "" {
			return nil, invalidSpec("rule description is not stored")
		}
		dir, err := parseDirection(r.GetDirection())
		if err != nil {
			return nil, err
		}
		proto, from, to, err := parseProtocol(r.GetProtocol(), r.GetFromPort(), r.GetToPort())
		if err != nil {
			return nil, err
		}
		rule := domain.Rule{Direction: dir, Protocol: proto, FromPort: from, ToPort: to}
		switch peer := r.Target.(type) {
		case *networkv1.SecurityGroupRule_Cidr:
			cidr, err := parsePrefix("cidr", peer.Cidr)
			if err != nil {
				return nil, err
			}
			rule.Peer.CIDR = cidr
		case *networkv1.SecurityGroupRule_SecurityGroup:
			n, err := parseChildName(peer.SecurityGroup, "securityGroups")
			if err != nil {
				return nil, err
			}
			if n.Project != project || n.Region != region || n.VPC != vpc {
				return nil, invalidSpec("security group peer is in another vpc")
			}
			rule.Peer.Group = domain.SecurityGroupID(n.ID)
		default:
			return nil, invalidSpec("rule peer is required")
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
		return 0, invalidSpec("rule direction is required")
	}
}

func parseProtocol(p networkv1.Protocol, from, to int32) (domain.Protocol, int32, int32, error) {
	switch p {
	case networkv1.Protocol_PROTOCOL_TCP:
		return domain.TCP, from, to, nil
	case networkv1.Protocol_PROTOCOL_UDP:
		return domain.UDP, from, to, nil
	case networkv1.Protocol_PROTOCOL_ICMP:
		if from != 0 || to != 0 {
			return 0, 0, 0, invalidSpec("icmp and all rules use port 0")
		}
		return domain.ICMP, -1, -1, nil
	case networkv1.Protocol_PROTOCOL_ALL:
		if from != 0 || to != 0 {
			return 0, 0, 0, invalidSpec("icmp and all rules use port 0")
		}
		return domain.All, -1, -1, nil
	default:
		return 0, 0, 0, invalidSpec("rule protocol is required")
	}
}

func cutPage[T any](items []T, pageSize int32, pageToken string) ([]T, string, error) {
	if pageSize < 0 {
		return nil, "", invalidSpec("page_size is invalid")
	}
	size := int(pageSize)
	if size == 0 {
		size = 50
	}
	if size > 1000 {
		size = 1000
	}
	offset := 0
	if pageToken != "" {
		n, err := strconv.Atoi(pageToken)
		if err != nil || n < 0 {
			return nil, "", invalidSpec("page_token is invalid")
		}
		offset = n
	}
	if offset >= len(items) {
		return nil, "", nil
	}
	end := offset + size
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	} else {
		end = len(items)
	}
	return items[offset:end], next, nil
}

func invalidSpec(msg string) error {
	return withInfo(codes.InvalidArgument, msg, "INVALID_SPEC")
}

func invalidID(msg string) error {
	return withInfo(codes.InvalidArgument, msg, "INVALID_RESOURCE_ID")
}

func withInfo(code codes.Code, msg, reason string) error {
	st, err := status.New(code, msg).WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: "vws.dev"})
	if err != nil {
		return status.Error(code, msg)
	}
	return st.Err()
}

func requiredRequest() error {
	return invalidSpec("request is required")
}

func toStatus(err error, requestID string) error {
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
	code, reason := statusInfo(de, requestID)
	if reason == "" {
		return status.Error(code, de.Msg)
	}
	return withInfo(code, de.Msg, reason)
}

func statusInfo(de *domain.Error, requestID string) (codes.Code, string) {
	switch de.Kind {
	case domain.Invalid:
		if strings.Contains(de.Msg, "resource id") {
			return codes.InvalidArgument, "INVALID_RESOURCE_ID"
		}
		return codes.InvalidArgument, "INVALID_SPEC"
	case domain.NotFound:
		return codes.NotFound, "RESOURCE_NOT_FOUND"
	case domain.Conflict:
		if de.Msg == "idempotency key reused with a different request" && requestID != "" {
			return codes.Aborted, "IDEMPOTENCY_CONFLICT"
		}
		return codes.AlreadyExists, "ALREADY_EXISTS"
	case domain.Precondition:
		if strings.Contains(de.Msg, "deleting or failed") {
			return codes.FailedPrecondition, "DEPENDENCY_NOT_READY"
		}
		return codes.FailedPrecondition, "DEPENDENCY_MISSING"
	case domain.Exhausted:
		return codes.ResourceExhausted, "QUOTA_EXCEEDED"
	case domain.Dataplane:
		return codes.Unavailable, "RECONCILIATION_FAILED"
	case domain.Busy:
		return codes.Aborted, ""
	default:
		return codes.Internal, "RECONCILIATION_FAILED"
	}
}

func toVPC(project string, v domain.VPC) *networkv1.Vpc {
	return &networkv1.Vpc{
		Name:   vpcName(project, v.Region, string(v.ID)),
		Spec:   &networkv1.VpcSpec{Ipv4Cidr: v.CIDR.String()},
		Status: &networkv1.VpcStatus{Observed: observed(v.Status)},
	}
}

func toSubnet(project string, s domain.Subnet) *networkv1.Subnet {
	return &networkv1.Subnet{
		Name:   subnetName(project, s.Region, string(s.VPC), string(s.ID)),
		Spec:   &networkv1.SubnetSpec{Zone: s.Zone, Ipv4Cidr: s.CIDR.String()},
		Status: &networkv1.SubnetStatus{Observed: observed(s.Status)},
	}
}

func toGroup(project string, g domain.SecurityGroup) *networkv1.SecurityGroup {
	out := &networkv1.SecurityGroup{
		Name:   groupName(project, g.Region, string(g.VPC), string(g.ID)),
		Spec:   &networkv1.SecurityGroupSpec{},
		Status: &networkv1.SecurityGroupStatus{Observed: observed(g.Status)},
	}
	for _, rule := range g.Rules {
		out.Spec.Rules = append(out.Spec.Rules, toRule(project, g.Region, string(g.VPC), rule))
	}
	return out
}

func toAddress(project, region string, e domain.ElasticIP) *networkv1.Address {
	use := e.Region
	if use == "" {
		use = region
	}
	return &networkv1.Address{
		Name: addressName(project, use, string(e.ID)),
		Spec: &networkv1.AddressSpec{IpVersion: networkv1.IpVersion_IP_VERSION_IPV4},
		Status: &networkv1.AddressStatus{
			Observed: observed(e.Status),
			Ipv4:     e.Address.String(),
		},
	}
}

func toRule(project, region, vpc string, r domain.Rule) *networkv1.SecurityGroupRule {
	out := &networkv1.SecurityGroupRule{
		Id:        ruleID(r),
		Direction: toDirection(r.Direction),
		Protocol:  toProtocol(r.Protocol),
		FromPort:  r.FromPort,
		ToPort:    r.ToPort,
	}
	if r.Protocol == domain.ICMP || r.Protocol == domain.All {
		out.FromPort = 0
		out.ToPort = 0
	}
	if r.Peer.Group != "" {
		out.Target = &networkv1.SecurityGroupRule_SecurityGroup{
			SecurityGroup: groupName(project, region, vpc, string(r.Peer.Group)),
		}
		return out
	}
	out.Target = &networkv1.SecurityGroupRule_Cidr{Cidr: r.Peer.CIDR.String()}
	return out
}

func ruleID(r domain.Rule) string {
	sum := sha256.Sum256([]byte(r.Identity()))
	return "r" + hex.EncodeToString(sum[:8])
}

func observed(s domain.Status) *commonv1.ObservedState {
	return &commonv1.ObservedState{State: reconState(s)}
}

func reconState(s domain.Status) commonv1.ReconciliationState {
	switch s {
	case domain.StatusPending:
		return commonv1.ReconciliationState_RECONCILIATION_STATE_PENDING
	case domain.StatusAvailable:
		return commonv1.ReconciliationState_RECONCILIATION_STATE_READY
	case domain.StatusDeleting:
		return commonv1.ReconciliationState_RECONCILIATION_STATE_DELETING
	case domain.StatusFailed:
		return commonv1.ReconciliationState_RECONCILIATION_STATE_ERROR
	default:
		return commonv1.ReconciliationState_RECONCILIATION_STATE_UNSPECIFIED
	}
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
