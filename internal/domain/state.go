package domain

import "net/netip"

// State is one project's desired resources. The zero value is an empty project.
// Methods either return an error and leave State unchanged, or preserve these
// invariants: VPC CIDRs are pairwise disjoint and contain no elastic IP of the
// project; every child names a VPC in State; subnets of one VPC are disjoint;
// group names are unique per VPC; a group peer is a group in the same VPC; a
// private address is associated to at most one elastic IP in the VPC.
type State struct {
	VPCs       map[VPCID]VPC
	Subnets    map[SubnetID]Subnet
	Groups     map[SecurityGroupID]SecurityGroup
	ElasticIPs map[ElasticIPID]ElasticIP
}

// Clone returns a deep copy.
func (s State) Clone() State {
	out := State{
		VPCs:       make(map[VPCID]VPC, len(s.VPCs)),
		Subnets:    make(map[SubnetID]Subnet, len(s.Subnets)),
		Groups:     make(map[SecurityGroupID]SecurityGroup, len(s.Groups)),
		ElasticIPs: make(map[ElasticIPID]ElasticIP, len(s.ElasticIPs)),
	}
	for id, v := range s.VPCs {
		out.VPCs[id] = v
	}
	for id, sub := range s.Subnets {
		out.Subnets[id] = sub
	}
	for id, g := range s.Groups {
		g.Rules = copyRules(g.Rules)
		g.revoking = copyRules(g.revoking)
		out.Groups[id] = g
	}
	for id, e := range s.ElasticIPs {
		e.detaching = copyAddrs(e.detaching)
		out.ElasticIPs[id] = e
	}
	return out
}

// Lane returns the VPC whose operations run in order with id.
func (s State) Lane(id string) (VPCID, bool) {
	if v, ok := s.VPCs[VPCID(id)]; ok {
		return v.ID, true
	}
	if sub, ok := s.Subnets[SubnetID(id)]; ok {
		return sub.VPC, true
	}
	if g, ok := s.Groups[SecurityGroupID(id)]; ok {
		return g.VPC, true
	}
	if e, ok := s.ElasticIPs[ElasticIPID(id)]; ok {
		return e.VPC, true
	}
	return "", false
}

// CreateVPC adds a pending VPC and its default security group.
func (s *State) CreateVPC(id VPCID, sg SecurityGroupID, r CreateVPC) error {
	if id == "" || sg == "" {
		return invalid("vpc id is required")
	}
	if err := CheckVPCCIDR(r.CIDR); err != nil {
		return err
	}
	s.init()
	if _, ok := s.VPCs[id]; ok {
		return conflict("vpc id already exists")
	}
	if _, ok := s.Groups[sg]; ok {
		return conflict("security group id already exists")
	}
	for _, v := range s.VPCs {
		if v.CIDR.Overlaps(r.CIDR) {
			return conflict("vpc cidr overlaps an existing vpc")
		}
	}
	for _, e := range s.ElasticIPs {
		if r.CIDR.Contains(e.Address) {
			return conflict("vpc cidr contains an elastic ip")
		}
	}
	s.VPCs[id] = VPC{ID: id, CIDR: r.CIDR, DefaultGroup: sg, Status: StatusPending}
	s.Groups[sg] = SecurityGroup{
		ID: sg, VPC: id, Name: "default", Default: true,
		Rules:  []Rule{DefaultEgress()},
		Status: StatusPending,
	}
	return nil
}

// CreateSubnet adds a pending subnet inside an existing VPC.
func (s *State) CreateSubnet(id SubnetID, r CreateSubnet) error {
	if id == "" {
		return invalid("subnet id is required")
	}
	if r.Zone == "" || len(r.Zone) > 64 {
		return invalid("availability zone is required")
	}
	s.init()
	v, ok := s.VPCs[r.VPC]
	if !ok {
		return notFound("vpc not found")
	}
	if err := usable(v.Status); err != nil {
		return err
	}
	if err := CheckSubnetCIDR(v.CIDR, r.CIDR); err != nil {
		return err
	}
	if _, ok := s.Subnets[id]; ok {
		return conflict("subnet id already exists")
	}
	for _, sub := range s.Subnets {
		if sub.VPC == r.VPC && sub.CIDR.Overlaps(r.CIDR) {
			return conflict("subnet cidr overlaps an existing subnet")
		}
	}
	s.Subnets[id] = Subnet{ID: id, VPC: r.VPC, CIDR: r.CIDR, Zone: r.Zone, Status: StatusPending}
	return nil
}

// CreateSecurityGroup adds a pending group with the default egress rule.
func (s *State) CreateSecurityGroup(id SecurityGroupID, r CreateSecurityGroup) error {
	if id == "" {
		return invalid("security group id is required")
	}
	if r.Name == "" || len(r.Name) > 255 {
		return invalid("security group name is required")
	}
	s.init()
	v, ok := s.VPCs[r.VPC]
	if !ok {
		return notFound("vpc not found")
	}
	if err := usable(v.Status); err != nil {
		return err
	}
	if _, ok := s.Groups[id]; ok {
		return conflict("security group id already exists")
	}
	for _, g := range s.Groups {
		if g.VPC == r.VPC && g.Name == r.Name {
			return conflict("security group name already exists")
		}
	}
	s.Groups[id] = SecurityGroup{
		ID: id, VPC: r.VPC, Name: r.Name,
		Rules:  []Rule{DefaultEgress()},
		Status: StatusPending,
	}
	return nil
}

// AuthorizeRules unions rules into the group and drops any matching revoke mark.
func (s *State) AuthorizeRules(r AuthorizeRules) error {
	rules, err := CanonicalRules(r.Rules)
	if err != nil {
		return err
	}
	s.init()
	g, ok := s.Groups[r.Group]
	if !ok {
		return notFound("security group not found")
	}
	if err := usable(g.Status); err != nil {
		return err
	}
	v, ok := s.VPCs[g.VPC]
	if !ok {
		return notFound("vpc not found")
	}
	if err := usable(v.Status); err != nil {
		return err
	}
	for _, rule := range rules {
		if rule.Peer.Group == "" {
			continue
		}
		peer, ok := s.Groups[rule.Peer.Group]
		if !ok || peer.VPC != g.VPC {
			return notFound("peer security group not found")
		}
		if err := usable(peer.Status); err != nil {
			return err
		}
	}
	merged := append(copyRules(g.Rules), rules...)
	canon, err := CanonicalRules(merged)
	if err != nil {
		return err
	}
	g.Rules = canon
	for _, rule := range rules {
		g.revoking = removeRule(g.revoking, rule)
	}
	s.Groups[g.ID] = g
	return nil
}

// RevokeRules moves matching rules onto the revoke list. A rule already on
// that list still counts as found, so a failed dataplane delete can be retried
// without a second mark. found is false only when none of the rules are in
// either list, which is a success with no dataplane work.
func (s *State) RevokeRules(r RevokeRules) (bool, error) {
	rules, err := CanonicalRules(r.Rules)
	if err != nil {
		return false, err
	}
	s.init()
	g, ok := s.Groups[r.Group]
	if !ok {
		return false, notFound("security group not found")
	}
	if err := usable(g.Status); err != nil {
		return false, err
	}
	drop := make(map[Rule]bool, len(rules))
	for _, rule := range rules {
		drop[rule] = true
	}
	var keep []Rule
	found := false
	revoking := copyRules(g.revoking)
	for _, rule := range g.Rules {
		if drop[rule] {
			found = true
			revoking = append(revoking, rule)
			continue
		}
		keep = append(keep, rule)
	}
	for _, rule := range g.revoking {
		if drop[rule] {
			found = true
		}
	}
	if !found {
		return false, nil
	}
	g.Rules = keep
	g.revoking = dedupRules(revoking)
	s.Groups[g.ID] = g
	return true, nil
}

// AllocateElasticIP reserves the lowest pool address outside every VPC CIDR
// in the project. taken reports addresses already reserved in any project.
func (s *State) AllocateElasticIP(id ElasticIPID, r AllocateElasticIP, pool netip.Prefix, taken func(netip.Addr) bool) error {
	if id == "" {
		return invalid("elastic ip id is required")
	}
	s.init()
	v, ok := s.VPCs[r.VPC]
	if !ok {
		return notFound("vpc not found")
	}
	if err := usable(v.Status); err != nil {
		return err
	}
	if _, ok := s.ElasticIPs[id]; ok {
		return conflict("elastic ip id already exists")
	}
	avoid := make([]netip.Prefix, 0, len(s.VPCs))
	for _, vpc := range s.VPCs {
		avoid = append(avoid, vpc.CIDR)
	}
	addr, ok := NextAddress(pool, taken, avoid)
	if !ok {
		return exhausted("elastic ip pool is exhausted")
	}
	s.ElasticIPs[id] = ElasticIP{ID: id, VPC: r.VPC, Address: addr, Status: StatusPending}
	return nil
}

// AssociateElasticIP binds eip to a usable host in exactly one subnet of its VPC.
// The same address again is a no-op. A different address is a precondition failure.
func (s *State) AssociateElasticIP(r AssociateElasticIP) error {
	if !r.Private.Is4() {
		return invalid("private address is required")
	}
	s.init()
	e, ok := s.ElasticIPs[r.ElasticIP]
	if !ok {
		return notFound("elastic ip not found")
	}
	if err := usable(e.Status); err != nil {
		return err
	}
	if len(e.detaching) > 0 {
		return precondition("elastic ip is disassociating")
	}
	var (
		subnet Subnet
		n      int
	)
	for _, sub := range s.Subnets {
		if sub.VPC == e.VPC && sub.CIDR.Contains(r.Private) {
			n++
			subnet = sub
		}
	}
	if n != 1 {
		return precondition("private address is not in a subnet of the vpc")
	}
	if err := usable(subnet.Status); err != nil {
		return err
	}
	if !Assignable(subnet.CIDR, r.Private) {
		return precondition("private address is reserved")
	}
	for _, other := range s.ElasticIPs {
		if other.ID != e.ID && other.VPC == e.VPC && other.Private == r.Private {
			return conflict("private address is already associated")
		}
	}
	if e.Private.IsValid() && e.Private != r.Private {
		return precondition("elastic ip is associated to a different address")
	}
	e.Private = r.Private
	e.Subnet = subnet.ID
	s.ElasticIPs[e.ID] = e
	return nil
}

// DeleteVPC marks the VPC and its default group deleting. It does not cascade.
func (s *State) DeleteVPC(r DeleteVPC) (bool, error) {
	s.init()
	v, ok := s.VPCs[r.VPC]
	if !ok {
		return false, nil
	}
	for _, sub := range s.Subnets {
		if sub.VPC == r.VPC {
			return false, precondition("vpc has subnets")
		}
	}
	for _, g := range s.Groups {
		if g.VPC == r.VPC && !g.Default {
			return false, precondition("vpc has security groups")
		}
	}
	for _, e := range s.ElasticIPs {
		if e.VPC == r.VPC {
			return false, precondition("vpc has elastic ips")
		}
	}
	v.Status = StatusDeleting
	s.VPCs[v.ID] = v
	g := s.Groups[v.DefaultGroup]
	g.Status = StatusDeleting
	s.Groups[g.ID] = g
	return true, nil
}

// DeleteSubnet marks the subnet deleting unless an association still names it.
func (s *State) DeleteSubnet(r DeleteSubnet) (bool, error) {
	s.init()
	sub, ok := s.Subnets[r.Subnet]
	if !ok {
		return false, nil
	}
	for _, e := range s.ElasticIPs {
		if e.Subnet == r.Subnet && e.Private.IsValid() {
			return false, precondition("subnet has an associated address")
		}
		for _, addr := range e.detaching {
			if sub.CIDR.Contains(addr) {
				return false, precondition("subnet has an associated address")
			}
		}
	}
	sub.Status = StatusDeleting
	s.Subnets[sub.ID] = sub
	return true, nil
}

// DeleteSecurityGroup marks a non-default group deleting. A group referenced
// by a live or revoking rule cannot be deleted.
func (s *State) DeleteSecurityGroup(r DeleteSecurityGroup) (bool, error) {
	s.init()
	g, ok := s.Groups[r.Group]
	if !ok {
		return false, nil
	}
	if g.Default {
		return false, precondition("default security group cannot be deleted")
	}
	for _, other := range s.Groups {
		if other.ID == g.ID {
			continue
		}
		for _, rule := range other.Rules {
			if rule.Peer.Group == g.ID {
				return false, precondition("security group is referenced by a rule")
			}
		}
		for _, rule := range other.revoking {
			if rule.Peer.Group == g.ID {
				return false, precondition("security group is referenced by a rule")
			}
		}
	}
	g.Status = StatusDeleting
	s.Groups[g.ID] = g
	return true, nil
}

// ReleaseElasticIP marks an unassociated address deleting.
func (s *State) ReleaseElasticIP(r ReleaseElasticIP) (bool, error) {
	s.init()
	e, ok := s.ElasticIPs[r.ElasticIP]
	if !ok {
		return false, nil
	}
	if e.Private.IsValid() || len(e.detaching) > 0 {
		return false, precondition("elastic ip is associated")
	}
	e.Status = StatusDeleting
	s.ElasticIPs[e.ID] = e
	return true, nil
}

// DisassociateElasticIP moves the private address onto the detach list.
// An address that is already free reports found false.
func (s *State) DisassociateElasticIP(r DisassociateElasticIP) (bool, error) {
	s.init()
	e, ok := s.ElasticIPs[r.ElasticIP]
	if !ok {
		return false, nil
	}
	if !e.Private.IsValid() && len(e.detaching) == 0 {
		return false, nil
	}
	if e.Private.IsValid() {
		e.detaching = append(copyAddrs(e.detaching), e.Private)
		e.Private = netip.Addr{}
		e.Subnet = ""
	}
	s.ElasticIPs[e.ID] = e
	return true, nil
}

// Plan returns the dataplane calls that make id match State.
// found is false when id is absent. A failed parent blocks ensure.
// A deleting child is still deleted when its parent is failed or gone.
func (s State) Plan(id string) ([]Action, bool, error) {
	if v, ok := s.VPCs[VPCID(id)]; ok {
		return s.planVPC(v)
	}
	if sub, ok := s.Subnets[SubnetID(id)]; ok {
		return s.planSubnet(sub)
	}
	if g, ok := s.Groups[SecurityGroupID(id)]; ok {
		return s.planGroup(g)
	}
	if e, ok := s.ElasticIPs[ElasticIPID(id)]; ok {
		return s.planEIP(e)
	}
	return nil, false, nil
}

// Settle records that the dataplane now matches observed for id.
// It advances a still-pending resource to available, removes a still-deleting
// resource, and drops revoke and detach marks that were in observed.
// It returns pool addresses that are no longer reserved.
func (s *State) Settle(id string, observed State) []netip.Addr {
	s.init()
	if v, ok := observed.VPCs[VPCID(id)]; ok {
		return s.settleVPC(v)
	}
	if sub, ok := observed.Subnets[SubnetID(id)]; ok {
		s.settleSubnet(sub)
		return nil
	}
	if g, ok := observed.Groups[SecurityGroupID(id)]; ok {
		s.settleGroup(g)
		return nil
	}
	if e, ok := observed.ElasticIPs[ElasticIPID(id)]; ok {
		return s.settleEIP(e)
	}
	return nil
}

// Fail marks a still-pending resource failed and keeps its marks.
// A deleting resource stays deleting so a later delete can retry it.
func (s *State) Fail(id string) {
	s.init()
	if v, ok := s.VPCs[VPCID(id)]; ok && v.Status == StatusPending {
		v.Status = StatusFailed
		s.VPCs[v.ID] = v
		if g, ok := s.Groups[v.DefaultGroup]; ok && g.Status == StatusPending {
			g.Status = StatusFailed
			s.Groups[g.ID] = g
		}
		return
	}
	if sub, ok := s.Subnets[SubnetID(id)]; ok && sub.Status == StatusPending {
		sub.Status = StatusFailed
		s.Subnets[sub.ID] = sub
		return
	}
	if g, ok := s.Groups[SecurityGroupID(id)]; ok && g.Status == StatusPending {
		g.Status = StatusFailed
		s.Groups[g.ID] = g
		return
	}
	if e, ok := s.ElasticIPs[ElasticIPID(id)]; ok && e.Status == StatusPending {
		e.Status = StatusFailed
		s.ElasticIPs[e.ID] = e
	}
}

func (s *State) init() {
	if s.VPCs == nil {
		s.VPCs = map[VPCID]VPC{}
	}
	if s.Subnets == nil {
		s.Subnets = map[SubnetID]Subnet{}
	}
	if s.Groups == nil {
		s.Groups = map[SecurityGroupID]SecurityGroup{}
	}
	if s.ElasticIPs == nil {
		s.ElasticIPs = map[ElasticIPID]ElasticIP{}
	}
}

func usable(st Status) error {
	if st == StatusPending || st == StatusAvailable {
		return nil
	}
	return precondition("resource is deleting or failed")
}

func (s State) planVPC(v VPC) ([]Action, bool, error) {
	g := s.Groups[v.DefaultGroup]
	if v.Status == StatusFailed {
		return nil, true, precondition("vpc is failed")
	}
	if v.Status == StatusDeleting {
		actions := deleteRules(g, append(copyRules(g.Rules), g.revoking...))
		actions = append(actions, Action{Kind: ActDeleteGroup, Group: g.public()}, Action{Kind: ActDeleteVPC, VPC: v})
		return actions, true, nil
	}
	actions := []Action{{Kind: ActEnsureVPC, VPC: v}, {Kind: ActEnsureGroup, Group: g.public()}}
	actions = append(actions, s.ensureRules(g)...)
	actions = append(actions, deleteRules(g, g.revoking)...)
	return actions, true, nil
}

func (s State) planSubnet(sub Subnet) ([]Action, bool, error) {
	if sub.Status == StatusDeleting {
		return []Action{{Kind: ActDeleteSubnet, Subnet: sub}}, true, nil
	}
	if sub.Status == StatusFailed {
		return nil, true, precondition("subnet is failed")
	}
	v, ok := s.VPCs[sub.VPC]
	if !ok || v.Status == StatusFailed {
		return nil, true, precondition("vpc is failed")
	}
	return []Action{{Kind: ActEnsureSubnet, Subnet: sub}}, true, nil
}

func (s State) planGroup(g SecurityGroup) ([]Action, bool, error) {
	if g.Status == StatusDeleting {
		actions := deleteRules(g, append(copyRules(g.Rules), g.revoking...))
		return append(actions, Action{Kind: ActDeleteGroup, Group: g.public()}), true, nil
	}
	if g.Status == StatusFailed {
		return nil, true, precondition("security group is failed")
	}
	v, ok := s.VPCs[g.VPC]
	if !ok || v.Status == StatusFailed {
		return nil, true, precondition("vpc is failed")
	}
	actions := []Action{{Kind: ActEnsureGroup, Group: g.public()}}
	actions = append(actions, s.ensureRules(g)...)
	actions = append(actions, deleteRules(g, g.revoking)...)
	return actions, true, nil
}

func (s State) planEIP(e ElasticIP) ([]Action, bool, error) {
	pub := e.public()
	if e.Status == StatusDeleting {
		actions := natDeletes(pub, e)
		return append(actions, Action{Kind: ActDeleteEIP, EIP: pub}), true, nil
	}
	if e.Status == StatusFailed {
		return nil, true, precondition("elastic ip is failed")
	}
	v, ok := s.VPCs[e.VPC]
	if !ok || v.Status == StatusFailed {
		return nil, true, precondition("vpc is failed")
	}
	actions := []Action{{Kind: ActEnsureEIP, EIP: pub}}
	if e.Private.IsValid() {
		actions = append(actions, Action{Kind: ActEnsureNAT, EIP: pub, Addr: e.Private})
	}
	for _, addr := range e.detaching {
		actions = append(actions, Action{Kind: ActDeleteNAT, EIP: pub, Addr: addr})
	}
	return actions, true, nil
}

func (s *State) settleVPC(observed VPC) []netip.Addr {
	cur, ok := s.VPCs[observed.ID]
	if !ok {
		return nil
	}
	if observed.Status == StatusPending && cur.Status == StatusPending {
		cur.Status = StatusAvailable
		s.VPCs[cur.ID] = cur
		if g, ok := s.Groups[cur.DefaultGroup]; ok && g.Status == StatusPending {
			g.Status = StatusAvailable
			s.Groups[g.ID] = g
		}
		return nil
	}
	if observed.Status == StatusDeleting && cur.Status == StatusDeleting {
		delete(s.Groups, cur.DefaultGroup)
		delete(s.VPCs, cur.ID)
	}
	return nil
}

func (s *State) settleSubnet(observed Subnet) {
	cur, ok := s.Subnets[observed.ID]
	if !ok {
		return
	}
	if observed.Status == StatusPending && cur.Status == StatusPending {
		cur.Status = StatusAvailable
		s.Subnets[cur.ID] = cur
		return
	}
	if observed.Status == StatusDeleting && cur.Status == StatusDeleting {
		delete(s.Subnets, cur.ID)
	}
}

func (s *State) settleGroup(observed SecurityGroup) {
	cur, ok := s.Groups[observed.ID]
	if !ok {
		return
	}
	cur.revoking = subtractRules(cur.revoking, observed.revoking)
	if observed.Status == StatusPending && cur.Status == StatusPending {
		cur.Status = StatusAvailable
	}
	if observed.Status == StatusDeleting && cur.Status == StatusDeleting {
		delete(s.Groups, cur.ID)
		return
	}
	s.Groups[cur.ID] = cur
}

func (s *State) settleEIP(observed ElasticIP) []netip.Addr {
	cur, ok := s.ElasticIPs[observed.ID]
	if !ok {
		return nil
	}
	cur.detaching = subtractAddrs(cur.detaching, observed.detaching)
	if observed.Status == StatusPending && cur.Status == StatusPending {
		cur.Status = StatusAvailable
		s.ElasticIPs[cur.ID] = cur
		return nil
	}
	if observed.Status == StatusDeleting && cur.Status == StatusDeleting {
		delete(s.ElasticIPs, cur.ID)
		if cur.Address.IsValid() {
			return []netip.Addr{cur.Address}
		}
		return nil
	}
	s.ElasticIPs[cur.ID] = cur
	return nil
}

func (g SecurityGroup) public() SecurityGroup {
	g.Rules = nil
	g.revoking = nil
	return g
}

func (e ElasticIP) public() ElasticIP {
	e.detaching = nil
	return e
}

// ensureRules emits the group's rules. A peer security group is ensured
// before the first rule that names it, so an earlier job that already sees
// the rule does not install the rule first.
func (s State) ensureRules(g SecurityGroup) []Action {
	actions := make([]Action, 0, len(g.Rules))
	pub := g.public()
	seen := map[SecurityGroupID]bool{g.ID: true}
	for _, rule := range g.Rules {
		if peer := rule.Peer.Group; peer != "" && !seen[peer] {
			seen[peer] = true
			if pg, ok := s.Groups[peer]; ok && pg.Status != StatusDeleting && pg.Status != StatusFailed {
				actions = append(actions, Action{Kind: ActEnsureGroup, Group: pg.public()})
			}
		}
		actions = append(actions, Action{Kind: ActEnsureRule, Group: pub, Rule: rule})
	}
	return actions
}

func deleteRules(g SecurityGroup, rules []Rule) []Action {
	actions := make([]Action, 0, len(rules))
	pub := g.public()
	for _, rule := range rules {
		actions = append(actions, Action{Kind: ActDeleteRule, Group: pub, Rule: rule})
	}
	return actions
}

func natDeletes(pub ElasticIP, e ElasticIP) []Action {
	var addrs []netip.Addr
	if e.Private.IsValid() {
		addrs = append(addrs, e.Private)
	}
	addrs = append(addrs, e.detaching...)
	actions := make([]Action, 0, len(addrs))
	for _, addr := range addrs {
		actions = append(actions, Action{Kind: ActDeleteNAT, EIP: pub, Addr: addr})
	}
	return actions
}

func copyRules(in []Rule) []Rule {
	if len(in) == 0 {
		return nil
	}
	out := make([]Rule, len(in))
	copy(out, in)
	return out
}

func copyAddrs(in []netip.Addr) []netip.Addr {
	if len(in) == 0 {
		return nil
	}
	out := make([]netip.Addr, len(in))
	copy(out, in)
	return out
}

func removeRule(in []Rule, rule Rule) []Rule {
	out := in[:0]
	for _, r := range in {
		if r != rule {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func dedupRules(in []Rule) []Rule {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[Rule]bool, len(in))
	out := make([]Rule, 0, len(in))
	for _, r := range in {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

func subtractRules(cur, observed []Rule) []Rule {
	drop := make(map[Rule]bool, len(observed))
	for _, r := range observed {
		drop[r] = true
	}
	out := make([]Rule, 0, len(cur))
	for _, r := range cur {
		if !drop[r] {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func subtractAddrs(cur, observed []netip.Addr) []netip.Addr {
	drop := make(map[netip.Addr]bool, len(observed))
	for _, a := range observed {
		drop[a] = true
	}
	out := make([]netip.Addr, 0, len(cur))
	for _, a := range cur {
		if !drop[a] {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
