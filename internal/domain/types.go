package domain

import "net/netip"

// ProjectID partitions every resource.
type ProjectID string

// IdempotencyKey is unique per project for the life of the store.
type IdempotencyKey string

// VPCID identifies a VPC. Identifiers are unique across resource kinds.
type VPCID string

// SubnetID identifies a subnet.
type SubnetID string

// SecurityGroupID identifies a security group.
type SecurityGroupID string

// ElasticIPID identifies an elastic IP.
type ElasticIPID string

// Call is the project and idempotency key on every mutation.
// DryRun checks the mutation and writes nothing.
type Call struct {
	Project ProjectID
	Key     IdempotencyKey
	DryRun  bool
}

// Validate rejects an empty project or key, and a key longer than 128 bytes.
func (c Call) Validate() error {
	if c.Project == "" || len(c.Project) > 128 {
		return invalid("project id is required")
	}
	if c.Key == "" || len(c.Key) > 128 {
		return invalid("idempotency key is required")
	}
	return nil
}

// Status is a resource lifecycle. Failed accepts only delete or release.
// Deleting and Failed still occupy their CIDR, name, and address.
type Status int

const (
	StatusPending Status = iota + 1
	StatusAvailable
	StatusDeleting
	StatusFailed
)

// VPC is one network. CIDR is immutable. DefaultGroup lives exactly as long as the VPC.
// Region is the API name segment. Empty means the caller did not supply one.
type VPC struct {
	ID           VPCID
	CIDR         netip.Prefix
	DefaultGroup SecurityGroupID
	Region       string
	Status       Status
}

// Subnet is one range inside a VPC. CIDR is immutable. Zone is stored and not a catalog lookup.
type Subnet struct {
	ID     SubnetID
	VPC    VPCID
	CIDR   netip.Prefix
	Zone   string
	Region string
	Status Status
}

// Direction is the side of a security group rule.
type Direction int

const (
	Ingress Direction = iota + 1
	Egress
)

// Protocol is the protocol of a security group rule.
type Protocol int

const (
	TCP Protocol = iota + 1
	UDP
	ICMP
	All
)

// Peer is the source or destination of a rule. Exactly one side is set.
// JSON names are the idempotency fingerprint, so a Go rename does not change it.
type Peer struct {
	CIDR  netip.Prefix    `json:"cidr,omitempty"`
	Group SecurityGroupID `json:"group,omitempty"`
}

// Rule is the authorize and revoke tuple. The value is comparable.
type Rule struct {
	Direction Direction `json:"direction"`
	Protocol  Protocol  `json:"protocol"`
	FromPort  int32     `json:"from_port"`
	ToPort    int32     `json:"to_port"`
	Peer      Peer      `json:"peer"`
}

// SecurityGroup belongs to one VPC. Name is unique in that VPC.
// revoking holds rules whose dataplane delete is not yet confirmed.
type SecurityGroup struct {
	ID       SecurityGroupID
	VPC      VPCID
	Name     string
	Default  bool
	Region   string
	Rules    []Rule
	revoking []Rule
	Status   Status
}

// ElasticIP is one address from the pool. Address is fixed at allocation.
// Subnet is set only while Private is associated. detaching holds addresses
// whose NAT delete is not yet confirmed.
type ElasticIP struct {
	ID        ElasticIPID
	VPC       VPCID
	Address   netip.Addr
	Private   netip.Addr
	Subnet    SubnetID
	Region    string
	detaching []netip.Addr
	Status    Status
}

// CreateVPC is the request body of a VPC create.
// ID and Region are set by the API. Empty ID means the service mints one.
type CreateVPC struct {
	CIDR         netip.Prefix `json:"cidr"`
	ID           VPCID        `json:"id,omitempty"`
	Region       string       `json:"region,omitempty"`
	AllowMissing bool         `json:"-"`
	// Keep leaves the stored spec alone. It is not part of the fingerprint;
	// the service fingerprints a separate body so an empty CIDR is not marshaled.
	Keep bool `json:"-"`
}

// DeleteVPC names the VPC to remove.
// Empty Region skips the region check. Region is part of the fingerprint.
type DeleteVPC struct {
	VPC           VPCID  `json:"vpc"`
	RejectMissing bool   `json:"reject_missing,omitempty"`
	Region        string `json:"region,omitempty"`
}

// CreateSubnet is the request body of a subnet create.
type CreateSubnet struct {
	VPC          VPCID        `json:"vpc"`
	CIDR         netip.Prefix `json:"cidr"`
	Zone         string       `json:"zone"`
	ID           SubnetID     `json:"id,omitempty"`
	Region       string       `json:"region,omitempty"`
	AllowMissing bool         `json:"-"`
	Keep         bool         `json:"-"`
}

// DeleteSubnet names the subnet to remove.
// Empty VPC or Region skips that check. Both are part of the fingerprint.
type DeleteSubnet struct {
	Subnet        SubnetID `json:"subnet"`
	RejectMissing bool     `json:"reject_missing,omitempty"`
	VPC           VPCID    `json:"vpc,omitempty"`
	Region        string   `json:"region,omitempty"`
}

// CreateSecurityGroup is the request body of a security group create.
// Rules, when set, replace the starting egress rule in the same commit.
type CreateSecurityGroup struct {
	VPC          VPCID           `json:"vpc"`
	Name         string          `json:"name"`
	ID           SecurityGroupID `json:"id,omitempty"`
	Region       string          `json:"region,omitempty"`
	Rules        []Rule          `json:"rules,omitempty"`
	AllowMissing bool            `json:"-"`
	// ReplaceRules clears the starting egress rule when Rules is empty.
	// It is not part of the idempotency fingerprint.
	ReplaceRules bool `json:"-"`
	// KeepRules leaves stored rules alone. It is part of the fingerprint.
	KeepRules bool `json:"keep_rules,omitempty"`
}

// DeleteSecurityGroup names the group to remove.
// Empty VPC or Region skips that check. Both are part of the fingerprint.
type DeleteSecurityGroup struct {
	Group         SecurityGroupID `json:"group"`
	RejectMissing bool            `json:"reject_missing,omitempty"`
	VPC           VPCID           `json:"vpc,omitempty"`
	Region        string          `json:"region,omitempty"`
}

// AuthorizeRules adds rules to a group. Rules are canonical before the call.
type AuthorizeRules struct {
	Group SecurityGroupID `json:"group"`
	Rules []Rule          `json:"rules"`
}

// RevokeRules removes rules from a group.
type RevokeRules struct {
	Group SecurityGroupID `json:"group"`
	Rules []Rule          `json:"rules"`
}

// AllocateElasticIP reserves one pool address in a VPC.
type AllocateElasticIP struct {
	VPC    VPCID       `json:"vpc"`
	ID     ElasticIPID `json:"id,omitempty"`
	Region string      `json:"region,omitempty"`
}

// ReleaseElasticIP returns an unassociated address to the pool.
// Empty Region skips the region check. Region is part of the fingerprint.
type ReleaseElasticIP struct {
	ElasticIP     ElasticIPID `json:"elastic_ip"`
	RejectMissing bool        `json:"reject_missing,omitempty"`
	Region        string      `json:"region,omitempty"`
}

// AssociateElasticIP binds an elastic IP to a private address.
// The subnet is the one that contains Private.
type AssociateElasticIP struct {
	ElasticIP ElasticIPID `json:"elastic_ip"`
	Private   netip.Addr  `json:"private"`
}

// DisassociateElasticIP clears an association.
// AllowMissing rejects creation of a missing address.
// Keep leaves an existing association alone. Empty Region skips the region check.
type DisassociateElasticIP struct {
	ElasticIP     ElasticIPID `json:"elastic_ip"`
	RejectMissing bool        `json:"reject_missing,omitempty"`
	AllowMissing  bool        `json:"allow_missing,omitempty"`
	Region        string      `json:"region,omitempty"`
	Keep          bool        `json:"keep,omitempty"`
}

// OperationType is the RPC name that accepted the operation.
type OperationType string

const (
	OpCreateVPC             OperationType = "CreateVpc"
	OpUpdateVPC             OperationType = "UpdateVpc"
	OpDeleteVPC             OperationType = "DeleteVpc"
	OpCreateSubnet          OperationType = "CreateSubnet"
	OpUpdateSubnet          OperationType = "UpdateSubnet"
	OpDeleteSubnet          OperationType = "DeleteSubnet"
	OpCreateSecurityGroup   OperationType = "CreateSecurityGroup"
	OpUpdateSecurityGroup   OperationType = "UpdateSecurityGroup"
	OpDeleteSecurityGroup   OperationType = "DeleteSecurityGroup"
	OpAuthorizeRules        OperationType = "AuthorizeSecurityGroupRules"
	OpRevokeRules           OperationType = "RevokeSecurityGroupRules"
	OpAllocateElasticIP     OperationType = "AllocateElasticIp"
	OpReleaseElasticIP      OperationType = "ReleaseElasticIp"
	OpAssociateElasticIP    OperationType = "AssociateElasticIp"
	OpDisassociateElasticIP OperationType = "DisassociateElasticIp"
)

// OperationTypes is every stored mutation. VPC, subnet, and security group
// names match their RPCs. Rule and elastic IP mutations stay inside the service.
func OperationTypes() []OperationType {
	return []OperationType{
		OpCreateVPC, OpUpdateVPC, OpDeleteVPC,
		OpCreateSubnet, OpUpdateSubnet, OpDeleteSubnet,
		OpCreateSecurityGroup, OpUpdateSecurityGroup, OpDeleteSecurityGroup,
		OpAuthorizeRules, OpRevokeRules,
		OpAllocateElasticIP, OpReleaseElasticIP,
		OpAssociateElasticIP, OpDisassociateElasticIP,
	}
}

// ResourceKind is the map an operation's resource id belongs to.
type ResourceKind int

const (
	KindVPC ResourceKind = iota + 1
	KindSubnet
	KindGroup
	KindAddress
)

// KindOf maps an operation to the resource it names.
// Client ids are not unique across kinds, so reconcile must not guess from the id.
func KindOf(typ OperationType) ResourceKind {
	switch typ {
	case OpCreateVPC, OpUpdateVPC, OpDeleteVPC:
		return KindVPC
	case OpCreateSubnet, OpUpdateSubnet, OpDeleteSubnet:
		return KindSubnet
	case OpCreateSecurityGroup, OpUpdateSecurityGroup, OpDeleteSecurityGroup, OpAuthorizeRules, OpRevokeRules:
		return KindGroup
	case OpAllocateElasticIP, OpReleaseElasticIP, OpAssociateElasticIP, OpDisassociateElasticIP:
		return KindAddress
	default:
		return 0
	}
}

// OpState is the caller-visible operation lifecycle. Succeeded and Failed are terminal.
type OpState int

const (
	OpPending OpState = iota + 1
	OpRunning
	OpSucceeded
	OpFailed
)

// Operation is one accepted mutation. Err is set only when State is OpFailed.
type Operation struct {
	ID       string
	Project  ProjectID
	Type     OperationType
	Resource string
	State    OpState
	Err      *Error
	Key      IdempotencyKey
}

// ActionKind is one dataplane call, in the order Plan returns.
type ActionKind int

const (
	ActEnsureVPC ActionKind = iota + 1
	ActDeleteVPC
	ActEnsureSubnet
	ActDeleteSubnet
	ActEnsureGroup
	ActDeleteGroup
	ActEnsureRule
	ActDeleteRule
	ActEnsureEIP
	ActDeleteEIP
	ActEnsureNAT
	ActDeleteNAT
)

// Action is one idempotent dataplane call. Group rules travel as their own actions.
type Action struct {
	Kind   ActionKind
	VPC    VPC
	Subnet Subnet
	Group  SecurityGroup
	Rule   Rule
	EIP    ElasticIP
	Addr   netip.Addr
}
