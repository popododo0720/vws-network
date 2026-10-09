package network

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/popododo0720/vws-network/internal/domain"
)

// Config selects the durable store and the elastic IP pool.
// DatabasePath is one SQLite file. AddressPool is a canonical IPv4 prefix
// from /16 through /30. Workers 0 starts two workers.
type Config struct {
	DatabasePath string
	AddressPool  netip.Prefix
	Workers      int
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
}

// Open opens SQLite, requeues operations left running by the previous process,
// and starts workers. The dataplane is the in-process fake.
func Open(ctx context.Context, cfg Config) (*Service, error) {
	if err := validatePool(cfg.AddressPool); err != nil {
		return nil, err
	}
	if cfg.DatabasePath == "" {
		return nil, &domain.Error{Kind: domain.Invalid, Msg: "database path is required"}
	}
	repo, err := openSQLite(ctx, cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	workers := cfg.Workers
	if workers == 0 {
		workers = 2
	}
	svc, err := start(ctx, repo, newFake(), cfg.AddressPool, workers)
	if err != nil {
		_ = repo.close()
		return nil, err
	}
	return svc, nil
}

// Close stops workers and closes the store. A later Open requeues any claim
// that was still running, which is the same path a crash takes.
func (s *Service) Close() error {
	if s.stop != nil {
		s.stop()
	}
	return s.repo.close()
}

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
		logf:        log.Printf,
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
	id := s.newID("vpc-")
	sg := s.newID("sg-")
	return s.mutate(ctx, c, domain.OpCreateVPC, id, r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		return true, st.CreateVPC(domain.VPCID(id), domain.SecurityGroupID(sg), r)
	})
}

// DeleteVPC removes a VPC that has no subnets, extra groups, or elastic IPs.
func (s *Service) DeleteVPC(ctx context.Context, c domain.Call, r domain.DeleteVPC) (domain.Operation, error) {
	if r.VPC == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "vpc id is required"}
	}
	return s.mutate(ctx, c, domain.OpDeleteVPC, string(r.VPC), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		return st.DeleteVPC(r)
	})
}

// CreateSubnet accepts a subnet inside a VPC.
func (s *Service) CreateSubnet(ctx context.Context, c domain.Call, r domain.CreateSubnet) (domain.Operation, error) {
	if r.VPC == "" || r.Zone == "" || !r.CIDR.IsValid() {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "subnet vpc, cidr, and zone are required"}
	}
	id := s.newID("subnet-")
	return s.mutate(ctx, c, domain.OpCreateSubnet, id, r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		return true, st.CreateSubnet(domain.SubnetID(id), r)
	})
}

// DeleteSubnet removes a subnet that has no association.
func (s *Service) DeleteSubnet(ctx context.Context, c domain.Call, r domain.DeleteSubnet) (domain.Operation, error) {
	if r.Subnet == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "subnet id is required"}
	}
	return s.mutate(ctx, c, domain.OpDeleteSubnet, string(r.Subnet), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		return st.DeleteSubnet(r)
	})
}

// CreateSecurityGroup accepts a group that starts with egress allow-all.
func (s *Service) CreateSecurityGroup(ctx context.Context, c domain.Call, r domain.CreateSecurityGroup) (domain.Operation, error) {
	if r.VPC == "" || r.Name == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "security group vpc and name are required"}
	}
	id := s.newID("sg-")
	return s.mutate(ctx, c, domain.OpCreateSecurityGroup, id, r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		return true, st.CreateSecurityGroup(domain.SecurityGroupID(id), r)
	})
}

// DeleteSecurityGroup removes a non-default group that no rule references.
func (s *Service) DeleteSecurityGroup(ctx context.Context, c domain.Call, r domain.DeleteSecurityGroup) (domain.Operation, error) {
	if r.Group == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "security group id is required"}
	}
	return s.mutate(ctx, c, domain.OpDeleteSecurityGroup, string(r.Group), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
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
	id := s.newID("eip-")
	return s.mutate(ctx, c, domain.OpAllocateElasticIP, id, r, func(st *domain.State, taken func(netip.Addr) bool) (bool, error) {
		return true, st.AllocateElasticIP(domain.ElasticIPID(id), r, s.pool, taken)
	})
}

// ReleaseElasticIP returns an unassociated address to the pool.
func (s *Service) ReleaseElasticIP(ctx context.Context, c domain.Call, r domain.ReleaseElasticIP) (domain.Operation, error) {
	if r.ElasticIP == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "elastic ip id is required"}
	}
	return s.mutate(ctx, c, domain.OpReleaseElasticIP, string(r.ElasticIP), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		return st.ReleaseElasticIP(r)
	})
}

// AssociateElasticIP binds an elastic IP to a usable host in its VPC.
func (s *Service) AssociateElasticIP(ctx context.Context, c domain.Call, r domain.AssociateElasticIP) (domain.Operation, error) {
	if r.ElasticIP == "" || !r.Private.Is4() {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "elastic ip and private address are required"}
	}
	return s.mutate(ctx, c, domain.OpAssociateElasticIP, string(r.ElasticIP), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		return true, st.AssociateElasticIP(r)
	})
}

// DisassociateElasticIP clears an association. A free address succeeds immediately.
func (s *Service) DisassociateElasticIP(ctx context.Context, c domain.Call, r domain.DisassociateElasticIP) (domain.Operation, error) {
	if r.ElasticIP == "" {
		return domain.Operation{}, &domain.Error{Kind: domain.Invalid, Msg: "elastic ip id is required"}
	}
	return s.mutate(ctx, c, domain.OpDisassociateElasticIP, string(r.ElasticIP), r, func(st *domain.State, _ func(netip.Addr) bool) (bool, error) {
		return st.DisassociateElasticIP(r)
	})
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
	out := make([]domain.Subnet, 0, len(st.Subnets))
	for _, sub := range st.Subnets {
		if vpc == "" || sub.VPC == vpc {
			out = append(out, sub)
		}
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
		op := domain.Operation{
			ID: s.newID("op-"), Project: c.Project, Type: typ, Resource: resource,
			State: domain.OpPending, Key: c.Key,
		}
		lane, _ := clone.Lane(resource)
		enq := &job{op: op, fingerprint: fp, lane: lane}
		cmt := commit{project: c.Project, enqueue: enq}
		if write {
			cmt.base = ver
			cmt.state = &clone
		} else {
			op.State = domain.OpSucceeded
			enq.op = op
			enq.lane = ""
		}
		if err := s.repo.commit(ctx, cmt); err != nil {
			if errors.Is(err, errStale) || errors.Is(err, errAddressTaken) || errors.Is(err, errKeyTaken) {
				last = err
				continue
			}
			return domain.Operation{}, err
		}
		if write {
			s.nudge()
		}
		return op, nil
	}
	if last == nil {
		last = errStale
	}
	return domain.Operation{}, last
}

func (s *Service) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func fingerprint(typ domain.OperationType, req any) ([32]byte, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return [32]byte{}, err
	}
	sum := sha256.New()
	if _, err := sum.Write([]byte(typ)); err != nil {
		return [32]byte{}, err
	}
	if _, err := sum.Write(b); err != nil {
		return [32]byte{}, err
	}
	var out [32]byte
	copy(out[:], sum.Sum(nil))
	return out, nil
}

func (s *Service) loop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		ok, err := s.step(ctx)
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

func (s *Service) step(ctx context.Context) (bool, error) {
	j, ok, err := s.repo.claim(ctx, s.now(), s.newID("tok-"))
	if err != nil || !ok {
		return false, err
	}
	// A claim that neither settles nor schedules a retry must not stay running.
	// The next claim in this process would otherwise see the lane as blocked.
	settled := false
	defer func() {
		if settled {
			return
		}
		rel := j
		rel.op.State = domain.OpPending
		rel.op.Err = nil
		_ = s.repo.commit(context.Background(), commit{project: j.op.Project, settle: &rel})
	}()
	st, _, err := s.repo.load(ctx, j.op.Project)
	if err != nil {
		return false, err
	}
	observed := st.Clone()
	if err := s.converge(ctx, observed, j.op.Resource); err != nil {
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
		cur.Settle(j.op.Resource, observed)
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
	if j.attempts >= s.maxAttempts {
		return s.failJob(ctx, j, cause)
	}
	j.op.State = domain.OpPending
	j.op.Err = nil
	j.notBefore = s.now().Add(s.backoff(j.attempts))
	return s.repo.commit(ctx, commit{project: j.op.Project, settle: &j})
}

func (s *Service) failJob(ctx context.Context, j job, cause error) error {
	de := &domain.Error{Kind: domain.Dataplane, Msg: cause.Error()}
	var already *domain.Error
	if errors.As(cause, &already) {
		de = already
	}
	var last error
	for i := 0; i < 8; i++ {
		cur, ver, err := s.repo.load(ctx, j.op.Project)
		if err != nil {
			return err
		}
		cur.Fail(j.op.Resource)
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

func (s *Service) converge(ctx context.Context, observed domain.State, id string) error {
	actions, found, err := observed.Plan(id)
	if err != nil {
		return err
	}
	if !found {
		return s.deleteByID(ctx, id)
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
