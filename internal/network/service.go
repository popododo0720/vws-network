package network

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/popododo0720/vws-lib/obs"

	"github.com/popododo0720/vws-network/internal/domain"
)

// Config selects the durable store, the elastic IP pool, and the dataplane.
// DatabasePath is one SQLite file. AddressPool is a canonical IPv4 prefix
// from /16 through /30. Workers 0 starts two workers. A zero KubeOVN keeps
// the in-process fake dataplane.
type Config struct {
	DatabasePath string
	AddressPool  netip.Prefix
	Workers      int
	KubeOVN      KubeOVNConfig
}

// Service is the network control surface. Storage and the dataplane are
// unexported. Mutations are idempotent under Call.Key and survive restarts.
type Service struct {
	repo        repository
	dp          dataplane
	pool        netip.Prefix
	now         func() time.Time
	newID       func(prefix string) string
	wake        chan struct{}
	stop        func()
	maxAttempts int
	backoff     func(attempts int) time.Duration
	logf        func(string, ...any)
	// direct are the direct networks, read-only and shared by every project.
	direct []domain.Subnet
	// heartbeat is the unix nanosecond time a worker loop last went round.
	heartbeat atomic.Int64
}

// Open opens SQLite, requeues operations left running by the previous process,
// and starts workers. The dataplane is Kube-OVN when cfg.KubeOVN sets a
// kubeconfig or any other Kube-OVN option, and the in-process fake otherwise.
func Open(ctx context.Context, cfg Config) (*Service, error) {
	if err := validatePool(cfg.AddressPool); err != nil {
		return nil, err
	}
	if cfg.DatabasePath == "" {
		return nil, &domain.Error{Kind: domain.Invalid, Msg: "database path is required"}
	}
	if err := validatePoolInUnderlay(cfg.KubeOVN.Underlay, cfg.AddressPool); err != nil {
		return nil, err
	}
	if err := validateDirect(cfg.KubeOVN.Direct, cfg.KubeOVN.Underlay, cfg.AddressPool); err != nil {
		return nil, err
	}
	dp, err := newDataplane(cfg.KubeOVN)
	if err != nil {
		return nil, err
	}
	if k, ok := dp.(*kubeOVN); ok {
		if err := k.startChecks(ctx, cfg.AddressPool); err != nil {
			return nil, err
		}
	}
	repo, err := openSQLite(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	workers := cfg.Workers
	if workers == 0 {
		workers = 2
	}
	svc, err := start(ctx, repo, measured{dp}, cfg.AddressPool, workers)
	if err != nil {
		if cerr := repo.close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
		return nil, err
	}
	svc.direct = directSubnets(cfg.KubeOVN.Direct)
	if k, ok := dp.(*kubeOVN); ok && len(k.gatewayNodes) > 0 {
		period := cfg.KubeOVN.GatewayRecheck
		if period <= 0 {
			period = defaultGatewayRecheck
		}
		svc.startGatewayLoop(k, period)
	} else if ok {
		if gatewayLabelUnkept(k) {
			slog.Default().LogAttrs(ctx, slog.LevelWarn, gatewayUnkeptWarning, slog.String("label", gatewayLabel))
		}
	}
	return svc, nil
}

// startGatewayLoop runs gatewayLoop until Close, which stops it before the
// workers and the store.
//
//nolint:contextcheck // the loop outlives the Open context and stops through Close
func (s *Service) startGatewayLoop(k *kubeOVN, period time.Duration) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.gatewayLoop(ctx, k, period)
	}()
	prev := s.stop
	s.stop = func() {
		cancel()
		<-done
		if prev != nil {
			prev()
		}
	}
}

// Close stops workers and closes the store. A later Open requeues any claim
// that was still running, which is the same path a crash takes.
func (s *Service) Close() error {
	if s.stop != nil {
		s.stop()
	}
	return s.repo.close()
}

// logError writes a worker failure through the process slog logger.
func logError(format string, args ...any) {
	slog.Default().LogAttrs(context.Background(), slog.LevelError, "network worker",
		slog.String(obs.FieldErr, fmt.Sprintf(format, args...)))
}

// staleHeartbeat is how long a worker loop may go without a round before the
// service is not ready. One round can include a dataplane call.
const staleHeartbeat = time.Minute

// Ready reports whether the service can do work: the store answers, the worker
// loops go round, and the dataplane answers a read. Liveness is not this.
// PendingOperations is the number of operations not yet succeeded or failed,
// for the vws.queue.depth metric. A store error is logged and reads as 0.
//
//nolint:contextcheck // the metric callback has no context; the read is bounded by its own timeout
func (s *Service) PendingOperations() int64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	n, err := s.repo.pending(ctx)
	if err != nil {
		if s.logf != nil {
			s.logf("network: pending operations: %v", err)
		}
		return 0
	}
	return n
}

func (s *Service) Ready(ctx context.Context) error {
	if err := s.repo.ping(ctx); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if s.stop != nil {
		if last := time.Unix(0, s.heartbeat.Load()); s.now().Sub(last) > staleHeartbeat {
			return fmt.Errorf("workers: no round since %s", last.Format(time.RFC3339))
		}
	}
	if err := s.dp.ready(ctx); err != nil {
		return fmt.Errorf("dataplane: %w", err)
	}
	return nil
}

//nolint:contextcheck // workers outlive the Open context and stop through Close
func start(ctx context.Context, repo repository, dp dataplane, pool netip.Prefix, workers int) (*Service, error) {
	if err := validatePool(pool); err != nil {
		return nil, err
	}
	if err := repo.requeue(ctx); err != nil {
		return nil, err
	}
	s := &Service{
		repo:        repo,
		dp:          dp,
		pool:        pool,
		now:         time.Now,
		newID:       randomID,
		wake:        make(chan struct{}, 1),
		maxAttempts: 10,
		backoff:     defaultBackoff,
		logf:        logError,
	}
	if workers > 0 {
		workerCtx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.loop(workerCtx)
			}()
		}
		s.heartbeat.Store(s.now().UnixNano())
		s.stop = func() {
			cancel()
			wg.Wait()
		}
	}
	return s, nil
}

// newService is the test constructor. It requeues and starts no workers.
func newService(ctx context.Context, repo repository, dp dataplane, pool netip.Prefix) (*Service, error) {
	return start(ctx, repo, dp, pool, 0)
}

func validatePool(p netip.Prefix) error {
	if !p.IsValid() || !p.Addr().Is4() || p != p.Masked() || p.Bits() < 16 || p.Bits() > 30 {
		return &domain.Error{Kind: domain.Invalid, Msg: "address pool must be a canonical ipv4 prefix from /16 through /30"}
	}
	return nil
}

func defaultBackoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 8 {
		attempts = 8
	}
	d := 100 * time.Millisecond << attempts
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}

func randomID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return prefix + hex.EncodeToString(b[:])
}

// CreateVPC accepts a VPC and its default security group.
func (s *Service) CreateVPC(ctx context.Context, c domain.Call, r domain.CreateVPC) (domain.Operation, error) {
	if err := domain.CheckVPCCIDR(r.CIDR); err != nil {
		return domain.Operation{}, err
	}
	if err := storedText(r.Text); err != nil {
		return domain.Operation{}, err
	}
	if r.ID == DirectVPC {
		return domain.Operation{}, directReadOnly()
	}
	id := string(r.ID)
	if id == "" {
		id = s.newID("vpc-")
	}
	sg := s.newID("sg-")
	return s.mutate(ctx, c, domain.OpCreateVPC, id, r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		if v, ok := st.VPCs[domain.VPCID(id)]; ok {
			if v.CIDR == r.CIDR && v.Region == r.Region && v.Description == r.Description && domain.SameLabels(v.Labels, r.Labels) && (v.Status == domain.StatusPending || v.Status == domain.StatusAvailable) {
				return false, nil
			}
			return false, &domain.Error{Kind: domain.Conflict, Msg: "vpc id already exists"}
		}
		return true, st.CreateVPC(domain.VPCID(id), domain.SecurityGroupID(sg), r)
	})
}

// UpdateVPC rejects a CIDR change. The same CIDR and text is a succeeded no-op.
// A text change is stored and the operation succeeds without dataplane work.
// allow_missing creates the VPC when the request includes a CIDR.
func (s *Service) UpdateVPC(ctx context.Context, c domain.Call, r domain.CreateVPC) (domain.Operation, error) {
	if r.ID == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "vpc id is required"}
	}
	if err := maskedText(r.Text); err != nil {
		return domain.Operation{}, err
	}
	if !r.Keep {
		if err := domain.CheckVPCCIDR(r.CIDR); err != nil {
			return domain.Operation{}, err
		}
	}
	sg := s.newID("sg-")
	body := any(r)
	if r.Keep {
		body = kept(string(r.ID), "", r.Region, r.Text)
	}
	return s.mutate(ctx, c, domain.OpUpdateVPC, string(r.ID), body, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		if v, ok := st.VPCs[r.ID]; ok {
			if otherName(v.Region, r.Region, "", "") {
				return false, &domain.Error{Kind: domain.NotFound, Msg: "vpc not found"}
			}
			if !r.Keep && v.CIDR != r.CIDR {
				return false, &domain.Error{Kind: domain.Invalid, Msg: "ipv4_cidr is immutable"}
			}
			return st.ApplyVPCText(r.ID, r)
		}
		if !r.AllowMissing {
			return false, &domain.Error{Kind: domain.NotFound, Msg: "vpc not found"}
		}
		if r.Keep {
			return false, &domain.Error{Kind: domain.Invalid, Msg: "vpc spec is required"}
		}
		return true, st.CreateVPC(r.ID, domain.SecurityGroupID(sg), r)
	})
}

// DeleteVPC removes a VPC that has no subnets, extra groups, or elastic IPs.
func (s *Service) DeleteVPC(ctx context.Context, c domain.Call, r domain.DeleteVPC) (domain.Operation, error) {
	if r.VPC == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "vpc id is required"}
	}
	return s.mutate(ctx, c, domain.OpDeleteVPC, string(r.VPC), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		v, ok := st.VPCs[r.VPC]
		if ok && otherName(v.Region, r.Region, "", "") {
			return absentAtName(r.RejectMissing, "vpc not found")
		}
		if !ok && r.RejectMissing {
			return false, &domain.Error{Kind: domain.NotFound, Msg: "vpc not found"}
		}
		return st.DeleteVPC(r)
	})
}

// CreateSubnet accepts a subnet inside a VPC.
func (s *Service) CreateSubnet(ctx context.Context, c domain.Call, r domain.CreateSubnet) (domain.Operation, error) {
	if r.VPC == "" || r.Zone == "" || !r.CIDR.IsValid() {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "subnet vpc, cidr, and zone are required"}
	}
	if isDirect(r.VPC, r.ID) {
		return domain.Operation{}, directReadOnly()
	}
	if err := storedText(r.Text); err != nil {
		return domain.Operation{}, err
	}
	id := string(r.ID)
	if id == "" {
		id = s.newID("subnet-")
	}
	return s.mutate(ctx, c, domain.OpCreateSubnet, id, r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		if sub, ok := st.Subnets[domain.SubnetID(id)]; ok {
			if sub.VPC == r.VPC && sub.CIDR == r.CIDR && sub.Zone == r.Zone && sub.Region == r.Region && sub.Description == r.Description && domain.SameLabels(sub.Labels, r.Labels) && (sub.Status == domain.StatusPending || sub.Status == domain.StatusAvailable) {
				return false, nil
			}
			return false, &domain.Error{Kind: domain.Conflict, Msg: "subnet id already exists"}
		}
		return true, st.CreateSubnet(domain.SubnetID(id), r)
	})
}

// UpdateSubnet rejects a CIDR or zone change. The same spec and text is a succeeded no-op.
// A text change is stored and the operation succeeds without dataplane work.
func (s *Service) UpdateSubnet(ctx context.Context, c domain.Call, r domain.CreateSubnet) (domain.Operation, error) {
	if r.ID == "" || r.VPC == "" || (!r.Keep && (r.Zone == "" || !r.CIDR.IsValid())) {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "subnet vpc, cidr, and zone are required"}
	}
	if isDirect(r.VPC, r.ID) {
		return domain.Operation{}, directReadOnly()
	}
	if err := maskedText(r.Text); err != nil {
		return domain.Operation{}, err
	}
	body := any(r)
	if r.Keep {
		body = kept(string(r.ID), string(r.VPC), r.Region, r.Text)
	}
	return s.mutate(ctx, c, domain.OpUpdateSubnet, string(r.ID), body, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		if sub, ok := st.Subnets[r.ID]; ok {
			if otherName(sub.Region, r.Region, sub.VPC, r.VPC) {
				return false, &domain.Error{Kind: domain.NotFound, Msg: "subnet not found"}
			}
			if !r.Keep && sub.CIDR != r.CIDR {
				return false, &domain.Error{Kind: domain.Invalid, Msg: "ipv4_cidr is immutable"}
			}
			if !r.Keep && sub.Zone != r.Zone {
				return false, &domain.Error{Kind: domain.Invalid, Msg: "zone is immutable"}
			}
			return st.ApplySubnetText(r.ID, r)
		}
		if !r.AllowMissing {
			return false, &domain.Error{Kind: domain.NotFound, Msg: "subnet not found"}
		}
		if r.Keep {
			return false, &domain.Error{Kind: domain.Invalid, Msg: "subnet vpc, cidr, and zone are required"}
		}
		return true, st.CreateSubnet(r.ID, r)
	})
}

// DeleteSubnet removes a subnet that has no association.
func (s *Service) DeleteSubnet(ctx context.Context, c domain.Call, r domain.DeleteSubnet) (domain.Operation, error) {
	if r.Subnet == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "subnet id is required"}
	}
	if isDirect(r.VPC, r.Subnet) {
		return domain.Operation{}, directReadOnly()
	}
	return s.mutate(ctx, c, domain.OpDeleteSubnet, string(r.Subnet), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		sub, ok := st.Subnets[r.Subnet]
		if ok && otherName(sub.Region, r.Region, sub.VPC, r.VPC) {
			return absentAtName(r.RejectMissing, "subnet not found")
		}
		if !ok && r.RejectMissing {
			return false, &domain.Error{Kind: domain.NotFound, Msg: "subnet not found"}
		}
		return st.DeleteSubnet(r)
	})
}

// CreateSecurityGroup accepts a group that starts with egress allow-all.
func (s *Service) CreateSecurityGroup(ctx context.Context, c domain.Call, r domain.CreateSecurityGroup) (domain.Operation, error) {
	if r.VPC == "" || r.Name == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "security group vpc and name are required"}
	}
	if err := storedText(r.Text); err != nil {
		return domain.Operation{}, err
	}
	var rules []domain.Rule
	if len(r.Rules) > 0 {
		canon, err := domain.CanonicalRules(r.Rules)
		if err != nil {
			return domain.Operation{}, err
		}
		r.Rules = canon
		rules = canon
	}
	id := string(r.ID)
	if id == "" {
		id = s.newID("sg-")
	}
	return s.mutate(ctx, c, domain.OpCreateSecurityGroup, id, r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		if g, ok := st.Groups[domain.SecurityGroupID(id)]; ok {
			if g.VPC == r.VPC && g.Name == r.Name && g.Region == r.Region && g.Description == r.Description && domain.SameLabels(g.Labels, r.Labels) && sameRules(g.Rules, rules, r.ReplaceRules) && (g.Status == domain.StatusPending || g.Status == domain.StatusAvailable) {
				return false, nil
			}
			return false, &domain.Error{Kind: domain.Conflict, Msg: "security group id already exists"}
		}
		if err := st.CreateSecurityGroup(domain.SecurityGroupID(id), r); err != nil {
			return false, err
		}
		if len(rules) == 0 {
			if !r.ReplaceRules {
				return true, nil
			}
			_, err := st.RevokeRules(domain.RevokeRules{Group: domain.SecurityGroupID(id), Rules: []domain.Rule{domain.DefaultEgress()}})
			return true, err
		}
		if _, err := st.RevokeRules(domain.RevokeRules{Group: domain.SecurityGroupID(id), Rules: []domain.Rule{domain.DefaultEgress()}}); err != nil {
			return false, err
		}
		return true, st.AuthorizeRules(domain.AuthorizeRules{Group: domain.SecurityGroupID(id), Rules: rules})
	})
}

// UpdateSecurityGroup makes the group's rules the requested set.
// An empty set revokes every live rule. allow_missing creates the group.
func (s *Service) UpdateSecurityGroup(ctx context.Context, c domain.Call, r domain.CreateSecurityGroup) (domain.Operation, error) {
	if r.ID == "" || r.VPC == "" || r.Name == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "security group vpc and name are required"}
	}
	if err := maskedText(r.Text); err != nil {
		return domain.Operation{}, err
	}
	var rules []domain.Rule
	if len(r.Rules) > 0 {
		canon, err := domain.CanonicalRules(r.Rules)
		if err != nil {
			return domain.Operation{}, err
		}
		r.Rules = canon
		rules = canon
	}
	return s.mutate(ctx, c, domain.OpUpdateSecurityGroup, string(r.ID), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		g, ok := st.Groups[r.ID]
		if ok && otherName(g.Region, r.Region, g.VPC, r.VPC) {
			return false, &domain.Error{Kind: domain.NotFound, Msg: "security group not found"}
		}
		if !ok {
			if !r.AllowMissing {
				return false, &domain.Error{Kind: domain.NotFound, Msg: "security group not found"}
			}
			if r.KeepRules {
				return false, &domain.Error{Kind: domain.Invalid, Msg: "security group spec is required"}
			}
			if err := st.CreateSecurityGroup(r.ID, r); err != nil {
				return false, err
			}
			if len(rules) == 0 {
				if !r.ReplaceRules {
					return true, nil
				}
				_, err := st.RevokeRules(domain.RevokeRules{Group: r.ID, Rules: []domain.Rule{domain.DefaultEgress()}})
				return true, err
			}
			if _, err := st.RevokeRules(domain.RevokeRules{Group: r.ID, Rules: []domain.Rule{domain.DefaultEgress()}}); err != nil {
				return false, err
			}
			return true, st.AuthorizeRules(domain.AuthorizeRules{Group: r.ID, Rules: rules})
		}
		if g.Default {
			return false, &domain.Error{Kind: domain.Precondition, Msg: "default security group cannot be changed"}
		}
		rulesChanged := false
		if !r.KeepRules {
			var err error
			rulesChanged, err = syncRules(st, g, rules)
			if err != nil {
				return false, err
			}
		}
		textChanged, err := st.ApplyGroupText(r.ID, r)
		if err != nil {
			return false, err
		}
		return rulesChanged || textChanged, nil
	})
}

// DeleteSecurityGroup removes a non-default group that no rule references.
func (s *Service) DeleteSecurityGroup(ctx context.Context, c domain.Call, r domain.DeleteSecurityGroup) (domain.Operation, error) {
	if r.Group == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "security group id is required"}
	}
	return s.mutate(ctx, c, domain.OpDeleteSecurityGroup, string(r.Group), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		g, ok := st.Groups[r.Group]
		if ok && otherName(g.Region, r.Region, g.VPC, r.VPC) {
			return absentAtName(r.RejectMissing, "security group not found")
		}
		if !ok && r.RejectMissing {
			return false, &domain.Error{Kind: domain.NotFound, Msg: "security group not found"}
		}
		return st.DeleteSecurityGroup(r)
	})
}

// AuthorizeRules adds rules. The same tuple does not create a second rule.
func (s *Service) AuthorizeRules(ctx context.Context, c domain.Call, r domain.AuthorizeRules) (domain.Operation, error) {
	rules, err := domain.CanonicalRules(r.Rules)
	if err != nil {
		return domain.Operation{}, err
	}
	r.Rules = rules
	if r.Group == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "security group id is required"}
	}
	return s.mutate(ctx, c, domain.OpAuthorizeRules, string(r.Group), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		return true, st.AuthorizeRules(r)
	})
}

// RevokeRules removes rules. An absent rule succeeds without dataplane work.
func (s *Service) RevokeRules(ctx context.Context, c domain.Call, r domain.RevokeRules) (domain.Operation, error) {
	rules, err := domain.CanonicalRules(r.Rules)
	if err != nil {
		return domain.Operation{}, err
	}
	r.Rules = rules
	if r.Group == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "security group id is required"}
	}
	return s.mutate(ctx, c, domain.OpRevokeRules, string(r.Group), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		return st.RevokeRules(r)
	})
}

// AllocateElasticIP reserves a pool address outside the project's VPC CIDRs.
func (s *Service) AllocateElasticIP(ctx context.Context, c domain.Call, r domain.AllocateElasticIP) (domain.Operation, error) {
	if r.VPC == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "vpc id is required"}
	}
	if r.VPC == DirectVPC {
		return domain.Operation{}, directElasticIP()
	}
	if err := domain.CheckLabels(r.Labels); err != nil {
		return domain.Operation{}, err
	}
	id := string(r.ID)
	if id == "" {
		id = s.newID("eip-")
	}
	return s.mutate(ctx, c, domain.OpAllocateElasticIP, id, r, func(st *domain.State, taken func(netip.Addr) bool) (bool, error) {
		if e, ok := st.ElasticIPs[domain.ElasticIPID(id)]; ok {
			if e.VPC == r.VPC && e.Region == r.Region && domain.SameLabels(e.Labels, r.Labels) && (e.Status == domain.StatusPending || e.Status == domain.StatusAvailable) {
				return false, nil
			}
			return false, &domain.Error{Kind: domain.Conflict, Msg: "elastic ip id already exists"}
		}
		return true, st.AllocateElasticIP(domain.ElasticIPID(id), r, s.pool, taken)
	})
}

// ReleaseElasticIP returns an unassociated address to the pool.
func (s *Service) ReleaseElasticIP(ctx context.Context, c domain.Call, r domain.ReleaseElasticIP) (domain.Operation, error) {
	if r.ElasticIP == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "elastic ip id is required"}
	}
	return s.mutate(ctx, c, domain.OpReleaseElasticIP, string(r.ElasticIP), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		e, ok := st.ElasticIPs[r.ElasticIP]
		if ok && otherName(e.Region, r.Region, "", "") {
			return absentAtName(r.RejectMissing, "elastic ip not found")
		}
		if !ok && r.RejectMissing {
			return false, &domain.Error{Kind: domain.NotFound, Msg: "elastic ip not found"}
		}
		return st.ReleaseElasticIP(r)
	})
}

// AssociateElasticIP binds an elastic IP to a usable host in its VPC.
func (s *Service) AssociateElasticIP(ctx context.Context, c domain.Call, r domain.AssociateElasticIP) (domain.Operation, error) {
	if r.ElasticIP == "" || !r.Private.Is4() {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "elastic ip and private address are required"}
	}
	return s.mutate(ctx, c, domain.OpAssociateElasticIP, string(r.ElasticIP), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		if s.onDirectOnly(st, r) {
			return false, directElasticIP()
		}
		return true, st.AssociateElasticIP(r)
	})
}

// DisassociateElasticIP clears an association. A free address succeeds immediately.
func (s *Service) DisassociateElasticIP(ctx context.Context, c domain.Call, r domain.DisassociateElasticIP) (domain.Operation, error) {
	if r.ElasticIP == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "elastic ip id is required"}
	}
	if r.SetLabels {
		if err := domain.CheckLabels(r.Labels); err != nil {
			return domain.Operation{}, err
		}
	}
	return s.mutate(ctx, c, domain.OpDisassociateElasticIP, string(r.ElasticIP), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		e, ok := st.ElasticIPs[r.ElasticIP]
		if ok && otherName(e.Region, r.Region, "", "") {
			return false, &domain.Error{Kind: domain.NotFound, Msg: "elastic ip not found"}
		}
		if !ok {
			if r.AllowMissing {
				return false, &domain.Error{Kind: domain.Invalid, Msg: "an address cannot be created without a vpc"}
			}
			if r.RejectMissing {
				return false, &domain.Error{Kind: domain.NotFound, Msg: "elastic ip not found"}
			}
			return false, nil
		}
		if r.Keep {
			return st.ApplyAddressText(r.ElasticIP, r.Labels, r.SetLabels)
		}
		changed, err := st.DisassociateElasticIP(r)
		if err != nil {
			return false, err
		}
		textChanged, err := st.ApplyAddressText(r.ElasticIP, r.Labels, r.SetLabels)
		if err != nil {
			return false, err
		}
		return changed || textChanged, nil
	})
}

// UpdateElasticIP applies an UpdateAddress. The stored VPC is kept when r.VPC
// is empty and must match when it is not. AllowMissing allocates a missing
// address through AllocateElasticIP's rules.
func (s *Service) UpdateElasticIP(ctx context.Context, c domain.Call, r domain.UpdateElasticIP) (domain.Operation, error) {
	if r.ElasticIP == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "elastic ip id is required"}
	}
	if r.SetLabels || r.AllowMissing {
		if err := domain.CheckLabels(r.Labels); err != nil {
			return domain.Operation{}, err
		}
	}
	if (r.Instance == "") != (r.Device == "") {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "instance and device name are required together"}
	}
	return s.mutate(ctx, c, domain.OpUpdateElasticIP, string(r.ElasticIP), r, func(st *domain.State, taken func(netip.Addr) bool) (bool, error) {
		e, ok := st.ElasticIPs[r.ElasticIP]
		if ok && otherName(e.Region, r.Region, "", "") {
			return false, &domain.Error{Kind: domain.NotFound, Msg: "elastic ip not found"}
		}
		if !ok {
			if r.AllowMissing {
				if r.VPC == "" {
					return false, &domain.Error{Kind: domain.Invalid, Msg: "an address cannot be created without a vpc"}
				}
				return true, st.AllocateElasticIP(r.ElasticIP, domain.AllocateElasticIP{
					VPC: r.VPC, ID: r.ElasticIP, Region: r.Region, Labels: r.Labels,
					Instance: r.Instance, Device: r.Device,
				}, s.pool, taken)
			}
			if r.RejectMissing {
				return false, &domain.Error{Kind: domain.NotFound, Msg: "elastic ip not found"}
			}
			return false, nil
		}
		if r.VPC != "" && r.VPC != e.VPC {
			return false, &domain.Error{Kind: domain.Invalid, Msg: "vpc is immutable"}
		}
		changed := false
		if r.SetAssociation {
			assoc, err := st.SetElasticIPAssociation(r.ElasticIP, r.Instance, r.Device)
			if err != nil {
				return false, err
			}
			changed = assoc
		}
		textChanged, err := st.ApplyAddressText(r.ElasticIP, r.Labels, r.SetLabels)
		if err != nil {
			return false, err
		}
		return changed || textChanged, nil
	})
}

// keepBody is the fingerprint of an update that does not touch the spec.
// Text is included so a label or description change is a different request.
type keepBody struct {
	ID     string `json:"id"`
	Parent string `json:"parent,omitempty"`
	Region string `json:"region,omitempty"`
	Keep   bool   `json:"keep"`
	domain.Text
}

func kept(id, parent, region string, text domain.Text) keepBody {
	return keepBody{ID: id, Parent: parent, Region: region, Keep: true, Text: text}
}

func storedText(text domain.Text) error {
	if err := domain.CheckDescription(text.Description); err != nil {
		return err
	}
	return domain.CheckLabels(text.Labels)
}

func maskedText(text domain.Text) error {
	if text.SetDescription {
		if err := domain.CheckDescription(text.Description); err != nil {
			return err
		}
	}
	if text.SetLabels {
		return domain.CheckLabels(text.Labels)
	}
	return nil
}

// otherName reports that a stored resource is not the one named by the request.
// An empty wanted region or parent skips that check. An empty stored region matches any region.
func otherName(storedRegion, wantRegion string, storedParent, wantParent domain.VPCID) bool {
	if wantParent != "" && storedParent != wantParent {
		return true
	}
	return wantRegion != "" && storedRegion != "" && storedRegion != wantRegion
}

// absentAtName refuses a delete of a different resource, and treats allow_missing as a no-op.
func absentAtName(reject bool, msg string) (bool, error) {
	if reject {
		return false, &domain.Error{Kind: domain.NotFound, Msg: msg}
	}
	return false, nil
}

func syncRules(st *domain.State, g domain.SecurityGroup, want []domain.Rule) (bool, error) {
	if len(want) == 0 {
		if len(g.Rules) == 0 {
			return false, nil
		}
		return st.RevokeRules(domain.RevokeRules{Group: g.ID, Rules: g.Rules})
	}
	have := make(map[domain.Rule]bool, len(g.Rules))
	for _, rule := range g.Rules {
		have[rule] = true
	}
	need := make(map[domain.Rule]bool, len(want))
	for _, rule := range want {
		need[rule] = true
	}
	var revoke, add []domain.Rule
	for _, rule := range g.Rules {
		if !need[rule] {
			revoke = append(revoke, rule)
		}
	}
	for _, rule := range want {
		if !have[rule] {
			add = append(add, rule)
		}
	}
	write := false
	if len(revoke) > 0 {
		w, err := st.RevokeRules(domain.RevokeRules{Group: g.ID, Rules: revoke})
		if err != nil {
			return false, err
		}
		write = w
	}
	if len(add) > 0 {
		if err := st.AuthorizeRules(domain.AuthorizeRules{Group: g.ID, Rules: add}); err != nil {
			return false, err
		}
		write = true
	}
	return write, nil
}

// GetVPC reads one VPC in the project.
func (s *Service) GetVPC(ctx context.Context, p domain.ProjectID, id domain.VPCID) (domain.VPC, error) {
	st, err := s.loadProject(ctx, p)
	if err != nil {
		return domain.VPC{}, err
	}
	v, ok := st.VPCs[id]
	if !ok {
		return domain.VPC{}, &domain.Error{Kind: domain.NotFound, Msg: "vpc not found"}
	}
	return v, nil
}

// GetSubnet reads one subnet in the project.
func (s *Service) GetSubnet(ctx context.Context, p domain.ProjectID, id domain.SubnetID) (domain.Subnet, error) {
	st, err := s.loadProject(ctx, p)
	if err != nil {
		return domain.Subnet{}, err
	}
	sub, ok := st.Subnets[id]
	if !ok {
		for _, d := range s.direct {
			if d.ID == id {
				return d, nil
			}
		}
		return domain.Subnet{}, &domain.Error{Kind: domain.NotFound, Msg: "subnet not found"}
	}
	return sub, nil
}

// GetSecurityGroup reads one security group in the project.
func (s *Service) GetSecurityGroup(ctx context.Context, p domain.ProjectID, id domain.SecurityGroupID) (domain.SecurityGroup, error) {
	st, err := s.loadProject(ctx, p)
	if err != nil {
		return domain.SecurityGroup{}, err
	}
	g, ok := st.Groups[id]
	if !ok {
		return domain.SecurityGroup{}, &domain.Error{Kind: domain.NotFound, Msg: "security group not found"}
	}
	return g, nil
}

// GetElasticIP reads one elastic IP in the project.
func (s *Service) GetElasticIP(ctx context.Context, p domain.ProjectID, id domain.ElasticIPID) (domain.ElasticIP, error) {
	st, err := s.loadProject(ctx, p)
	if err != nil {
		return domain.ElasticIP{}, err
	}
	e, ok := st.ElasticIPs[id]
	if !ok {
		return domain.ElasticIP{}, &domain.Error{Kind: domain.NotFound, Msg: "elastic ip not found"}
	}
	return e, nil
}

// ListVPCs reads every VPC in the project.
func (s *Service) ListVPCs(ctx context.Context, p domain.ProjectID) ([]domain.VPC, error) {
	st, err := s.loadProject(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]domain.VPC, 0, len(st.VPCs))
	for _, v := range st.VPCs {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListSubnets reads subnets in the project. vpc empty means every subnet.
func (s *Service) ListSubnets(ctx context.Context, p domain.ProjectID, vpc domain.VPCID) ([]domain.Subnet, error) {
	st, err := s.loadProject(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Subnet, 0, len(st.Subnets)+len(s.direct))
	for _, sub := range st.Subnets {
		if vpc == "" || sub.VPC == vpc {
			out = append(out, sub)
		}
	}
	if vpc == "" || vpc == DirectVPC {
		out = append(out, s.direct...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListSecurityGroups reads groups in the project. vpc empty means every group.
func (s *Service) ListSecurityGroups(ctx context.Context, p domain.ProjectID, vpc domain.VPCID) ([]domain.SecurityGroup, error) {
	st, err := s.loadProject(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SecurityGroup, 0, len(st.Groups))
	for _, g := range st.Groups {
		if vpc == "" || g.VPC == vpc {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListElasticIPs reads elastic IPs in the project. vpc empty means every address.
func (s *Service) ListElasticIPs(ctx context.Context, p domain.ProjectID, vpc domain.VPCID) ([]domain.ElasticIP, error) {
	st, err := s.loadProject(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ElasticIP, 0, len(st.ElasticIPs))
	for _, e := range st.ElasticIPs {
		if vpc == "" || e.VPC == vpc {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// GetOperation reads one operation in the project.
func (s *Service) GetOperation(ctx context.Context, p domain.ProjectID, id string) (domain.Operation, error) {
	if p == "" || id == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "project and operation id are required"}
	}
	return s.repo.operation(ctx, p, id)
}

// OperationView is an operation and the resource it still names.
type OperationView struct {
	Operation domain.Operation
	VPC       *domain.VPC
	Subnet    *domain.Subnet
	Group     *domain.SecurityGroup
	ElasticIP *domain.ElasticIP
}

// ViewOperation loads an operation and its resource together.
func (s *Service) ViewOperation(ctx context.Context, p domain.ProjectID, id string) (OperationView, error) {
	op, err := s.GetOperation(ctx, p, id)
	if err != nil {
		return OperationView{}, err
	}
	view := OperationView{Operation: op}
	switch op.Type {
	case domain.OpCreateVPC, domain.OpUpdateVPC, domain.OpDeleteVPC:
		v, err := s.GetVPC(ctx, op.Project, domain.VPCID(op.Resource))
		if err != nil {
			return view, ignoreMissing(err)
		}
		view.VPC = &v
	case domain.OpCreateSubnet, domain.OpUpdateSubnet, domain.OpDeleteSubnet:
		sub, err := s.GetSubnet(ctx, op.Project, domain.SubnetID(op.Resource))
		if err != nil {
			return view, ignoreMissing(err)
		}
		view.Subnet = &sub
	case domain.OpCreateSecurityGroup, domain.OpUpdateSecurityGroup, domain.OpDeleteSecurityGroup, domain.OpAuthorizeRules, domain.OpRevokeRules:
		g, err := s.GetSecurityGroup(ctx, op.Project, domain.SecurityGroupID(op.Resource))
		if err != nil {
			return view, ignoreMissing(err)
		}
		view.Group = &g
	case domain.OpAllocateElasticIP, domain.OpReleaseElasticIP, domain.OpAssociateElasticIP, domain.OpDisassociateElasticIP, domain.OpUpdateElasticIP:
		e, err := s.GetElasticIP(ctx, op.Project, domain.ElasticIPID(op.Resource))
		if err != nil {
			return view, ignoreMissing(err)
		}
		view.ElasticIP = &e
	}
	return view, nil
}

func ignoreMissing(err error) error {
	var de *domain.Error
	if errors.As(err, &de) && de.Kind == domain.NotFound {
		return nil
	}
	return err
}

func (s *Service) loadProject(ctx context.Context, p domain.ProjectID) (domain.State, error) {
	if p == "" {
		return domain.State{}, &domain.Error{Kind: domain.Invalid, Msg: "project id is required"}
	}
	st, _, err := s.repo.load(ctx, p)
	return st, err
}

func (s *Service) mutate(ctx context.Context, c domain.Call, typ domain.OperationType, resource string, req any, apply func(*domain.State, func(netip.Addr) bool) (bool, error)) (domain.Operation, error) {
	if err := c.Validate(); err != nil {
		return domain.Operation{}, err
	}
	fp, err := fingerprint(typ, req)
	if err != nil {
		return domain.Operation{}, err
	}
	var last error
	for i := 0; i < 8; i++ {
		existing, ok, err := s.repo.key(ctx, c.Project, c.Key)
		if err != nil {
			return domain.Operation{}, err
		}
		if ok {
			if existing.fingerprint == fp {
				return existing.op, nil
			}
			return domain.Operation{}, &domain.Error{Kind: domain.Conflict, Msg: "idempotency key reused with a different request"}
		}
		st, ver, err := s.repo.load(ctx, c.Project)
		if err != nil {
			return domain.Operation{}, err
		}
		used, err := s.repo.addressesInUse(ctx)
		if err != nil {
			return domain.Operation{}, err
		}
		clone := st.Clone()
		write, err := apply(&clone, func(a netip.Addr) bool { return used[a] })
		if err != nil {
			return domain.Operation{}, err
		}
		if c.DryRun {
			return domain.Operation{
				ID: "validate-" + resource, Project: c.Project, Type: typ, Resource: resource,
				State: domain.OpSucceeded, Key: c.Key,
			}, nil
		}
		op := domain.Operation{
			ID: s.newID("op-"), Project: c.Project, Type: typ, Resource: resource,
			State: domain.OpPending, Key: c.Key,
		}
		reconcile := write && !st.TextOnly(clone)
		lane, _ := clone.LaneKind(domain.KindOf(typ), resource)
		if !reconcile {
			op.State = domain.OpSucceeded
			lane = ""
		}
		enq := &job{op: op, fingerprint: fp, lane: lane}
		cmt := commit{project: c.Project, enqueue: enq}
		if write {
			cmt.base = ver
			cmt.state = &clone
		}
		if err := s.repo.commit(ctx, cmt); err != nil {
			if errors.Is(err, errStale) || errors.Is(err, errAddressTaken) || errors.Is(err, errKeyTaken) {
				last = err
				continue
			}
			return domain.Operation{}, err
		}
		if reconcile {
			s.nudge()
		}
		return op, nil
	}
	return s.commitFailure(ctx, c, fp, last)
}

// commitFailure turns a store race into a domain error. The sentinels stay
// inside this package so a client never sees them.
func (s *Service) commitFailure(ctx context.Context, c domain.Call, fp [32]byte, err error) (domain.Operation, error) {
	if errors.Is(err, errKeyTaken) {
		existing, ok, keyErr := s.repo.key(ctx, c.Project, c.Key)
		if keyErr != nil {
			return domain.Operation{}, keyErr
		}
		if ok && existing.fingerprint == fp {
			return existing.op, nil
		}
		if ok {
			return domain.Operation{}, &domain.Error{Kind: domain.Conflict, Msg: "idempotency key reused with a different request"}
		}
	}
	if errors.Is(err, errAddressTaken) {
		return domain.Operation{}, &domain.Error{Kind: domain.Conflict, Msg: "pool address is taken"}
	}
	if errors.Is(err, errStale) || errors.Is(err, errKeyTaken) {
		return domain.Operation{}, &domain.Error{Kind: domain.Busy, Msg: "project changed concurrently"}
	}
	if err == nil {
		err = errStale
	}
	return domain.Operation{}, err
}

func (s *Service) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func sameRules(got, want []domain.Rule, replaceEmpty bool) bool {
	if len(want) == 0 {
		if replaceEmpty {
			return len(got) == 0
		}
		return len(got) == 1 && got[0] == domain.DefaultEgress()
	}
	canon, err := domain.CanonicalRules(got)
	if err != nil || len(canon) != len(want) {
		return false
	}
	for i := range canon {
		if canon[i] != want[i] {
			return false
		}
	}
	return true
}

func fingerprint(typ domain.OperationType, req any) ([32]byte, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return [32]byte{}, fmt.Errorf("fingerprint: %w", err)
	}
	sum := sha256.New()
	if _, err := sum.Write([]byte(typ)); err != nil {
		return [32]byte{}, fmt.Errorf("fingerprint: %w", err)
	}
	if _, err := sum.Write(b); err != nil {
		return [32]byte{}, fmt.Errorf("fingerprint: %w", err)
	}
	var out [32]byte
	copy(out[:], sum.Sum(nil))
	return out, nil
}

func (s *Service) loop(ctx context.Context) {
	for {
		s.heartbeat.Store(s.now().UnixNano())
		if ctx.Err() != nil {
			return
		}
		ok, err := s.stepRecovering(ctx)
		if err != nil && s.logf != nil {
			s.logf("network: step: %v", err)
		}
		if ok && err == nil {
			continue
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// errPanic is the only thing a recovered panic leaves in an error. The value
// and stack go to the log, so a stored operation error or a client never sees
// them.
var errPanic = errors.New("internal error")

func (s *Service) recovered(where string, r any) error {
	if s.logf != nil {
		s.logf("network: panic in %s: %v\n%s", where, r, debug.Stack())
	}
	return errPanic
}

// stepRecovering keeps a worker alive when step panics.
func (s *Service) stepRecovering(ctx context.Context) (ok bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			ok, err = false, s.recovered("step", r)
		}
	}()
	return s.step(ctx)
}

// convergeRecovering turns a panic in the dataplane or the planner into an
// error, so the operation counts an attempt and is retried or failed.
func (s *Service) convergeRecovering(ctx context.Context, observed domain.State, typ domain.OperationType, id string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = s.recovered("converge "+string(typ), r)
		}
	}()
	return s.converge(ctx, observed, typ, id)
}

func (s *Service) step(ctx context.Context) (bool, error) {
	j, ok, err := s.repo.claim(ctx, s.now(), s.newID("tok-"))
	if err != nil || !ok {
		return false, err
	}
	// A claim that neither settles nor schedules a retry must not stay running.
	// The next claim in this process would otherwise see the lane as blocked.
	settled := false
	//nolint:contextcheck // ctx may be cancelled here and the claim must still be released
	defer func() {
		if settled {
			return
		}
		rel := j
		rel.op.State = domain.OpPending
		rel.op.Err = nil
		if err := s.repo.commit(context.Background(), commit{project: j.op.Project, settle: &rel}); err != nil && s.logf != nil {
			s.logf("network: release claim of %s: %v", j.op.ID, err)
		}
	}()
	st, _, err := s.repo.load(ctx, j.op.Project)
	if err != nil {
		return false, err
	}
	observed := st.Clone()
	if err := s.convergeRecovering(ctx, observed, j.op.Type, j.op.Resource); err != nil {
		if err := s.failOrRetry(ctx, j, err); err != nil {
			return false, err
		}
		settled = true
		return true, nil
	}
	var last error
	for i := 0; i < 8; i++ {
		cur, ver, err := s.repo.load(ctx, j.op.Project)
		if err != nil {
			return false, err
		}
		cur.SettleKind(domain.KindOf(j.op.Type), j.op.Resource, observed)
		done := j
		done.op.State = domain.OpSucceeded
		done.op.Err = nil
		err = s.repo.commit(ctx, commit{project: j.op.Project, base: ver, state: &cur, settle: &done})
		if err == nil {
			settled = true
			return true, nil
		}
		if !errors.Is(err, errStale) {
			return false, err
		}
		last = err
	}
	return false, last
}

func (s *Service) failOrRetry(ctx context.Context, j job, cause error) error {
	j.attempts++
	var de *domain.Error
	if j.attempts >= s.maxAttempts || (errors.As(cause, &de) && (de.Kind == domain.Invalid || de.Kind == domain.Conflict)) {
		return s.failJob(ctx, j, cause)
	}
	j.op.State = domain.OpPending
	j.op.Err = nil
	j.notBefore = s.now().Add(s.backoff(j.attempts))
	return s.repo.commit(ctx, commit{project: j.op.Project, settle: &j})
}

const dataplaneFailedMsg = "dataplane reconciliation failed"

func (s *Service) failJob(ctx context.Context, j job, cause error) error {
	de := &domain.Error{Kind: domain.Dataplane, Msg: dataplaneFailedMsg}
	var already *domain.Error
	if errors.As(cause, &already) {
		de = already
	} else if errors.Is(cause, errPanic) {
		de.Msg = errPanic.Error()
	} else if s.logf != nil {
		s.logf("network: operation %s %s %s failed: %v", j.op.ID, j.op.Type, j.op.Resource, cause)
	}
	var last error
	for i := 0; i < 8; i++ {
		cur, ver, err := s.repo.load(ctx, j.op.Project)
		if err != nil {
			return err
		}
		cur.FailKind(domain.KindOf(j.op.Type), j.op.Resource)
		done := j
		done.op.State = domain.OpFailed
		done.op.Err = de
		err = s.repo.commit(ctx, commit{project: j.op.Project, base: ver, state: &cur, settle: &done})
		if err == nil {
			return nil
		}
		if !errors.Is(err, errStale) {
			return err
		}
		last = err
	}
	return last
}

func (s *Service) converge(ctx context.Context, observed domain.State, typ domain.OperationType, id string) error {
	kind := domain.KindOf(typ)
	actions, found, err := observed.PlanKind(kind, id)
	if err != nil {
		return err
	}
	if !found {
		return s.deleteByKind(ctx, kind, id)
	}
	for _, action := range actions {
		if err := s.exec(ctx, action); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) exec(ctx context.Context, a domain.Action) error {
	switch a.Kind {
	case domain.ActEnsureVPC:
		return s.dp.ensureVPC(ctx, a.VPC)
	case domain.ActDeleteVPC:
		return s.dp.deleteVPC(ctx, a.VPC)
	case domain.ActEnsureSubnet:
		return s.dp.ensureSubnet(ctx, a.Subnet)
	case domain.ActDeleteSubnet:
		return s.dp.deleteSubnet(ctx, a.Subnet)
	case domain.ActEnsureGroup:
		return s.dp.ensureGroup(ctx, a.Group)
	case domain.ActDeleteGroup:
		return s.dp.deleteGroup(ctx, a.Group)
	case domain.ActEnsureRule:
		return s.dp.ensureRule(ctx, a.Group.ID, a.Rule)
	case domain.ActDeleteRule:
		return s.dp.deleteRule(ctx, a.Group.ID, a.Rule)
	case domain.ActEnsureEIP:
		return s.dp.ensureElasticIP(ctx, a.EIP)
	case domain.ActDeleteEIP:
		return s.dp.deleteElasticIP(ctx, a.EIP)
	case domain.ActEnsureNAT:
		return s.dp.ensureNAT(ctx, a.EIP, a.Addr)
	case domain.ActDeleteNAT:
		return s.dp.deleteNAT(ctx, a.EIP, a.Addr)
	default:
		return &domain.Error{Kind: domain.Dataplane, Msg: "unknown dataplane action"}
	}
}

func (s *Service) deleteByKind(ctx context.Context, kind domain.ResourceKind, id string) error {
	switch kind {
	case domain.KindVPC:
		return s.dp.deleteVPC(ctx, domain.VPC{ID: domain.VPCID(id)})
	case domain.KindSubnet:
		return s.dp.deleteSubnet(ctx, domain.Subnet{ID: domain.SubnetID(id)})
	case domain.KindGroup:
		return s.dp.deleteGroup(ctx, domain.SecurityGroup{ID: domain.SecurityGroupID(id)})
	case domain.KindAddress:
		return s.dp.deleteElasticIP(ctx, domain.ElasticIP{ID: domain.ElasticIPID(id)})
	default:
		return s.deleteByID(ctx, id)
	}
}

func (s *Service) deleteByID(ctx context.Context, id string) error {
	switch {
	case strings.HasPrefix(id, "vpc-"):
		return s.dp.deleteVPC(ctx, domain.VPC{ID: domain.VPCID(id)})
	case strings.HasPrefix(id, "subnet-"):
		return s.dp.deleteSubnet(ctx, domain.Subnet{ID: domain.SubnetID(id)})
	case strings.HasPrefix(id, "sg-"):
		return s.dp.deleteGroup(ctx, domain.SecurityGroup{ID: domain.SecurityGroupID(id)})
	case strings.HasPrefix(id, "eip-"):
		return s.dp.deleteElasticIP(ctx, domain.ElasticIP{ID: domain.ElasticIPID(id)})
	default:
		return &domain.Error{Kind: domain.Dataplane, Msg: "unknown resource"}
	}
}
