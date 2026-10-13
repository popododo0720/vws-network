package domain

import (
	"encoding/json"
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func kindOf(err error) ErrKind {
	var e *Error
	if !errors.As(err, &e) {
		return 0
	}
	return e.Kind
}

func TestVPCOverlapAndSubnetContainment(t *testing.T) {
	var s State
	if err := s.CreateVPC("vpc-1", "sg-1", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	before := s.Clone()
	err := s.CreateVPC("vpc-2", "sg-2", CreateVPC{CIDR: mustPrefix(t, "10.0.1.0/24")})
	if kindOf(err) != Conflict {
		t.Fatalf("overlap err = %v", err)
	}
	if !reflect.DeepEqual(s, before) {
		t.Fatal("overlap mutated state")
	}
	err = s.CreateVPC("vpc-2", "sg-2", CreateVPC{CIDR: mustPrefix(t, "10.1.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSubnet("subnet-1", CreateSubnet{VPC: "vpc-1", CIDR: mustPrefix(t, "10.0.1.0/24"), Zone: "az1"}); err != nil {
		t.Fatal(err)
	}
	before = s.Clone()
	err = s.CreateSubnet("subnet-2", CreateSubnet{VPC: "vpc-1", CIDR: mustPrefix(t, "10.0.1.128/25"), Zone: "az1"})
	if kindOf(err) != Conflict {
		t.Fatalf("subnet overlap err = %v", err)
	}
	if !reflect.DeepEqual(s, before) {
		t.Fatal("subnet overlap mutated state")
	}
	err = s.CreateSubnet("subnet-3", CreateSubnet{VPC: "vpc-1", CIDR: mustPrefix(t, "10.2.0.0/24"), Zone: "az1"})
	if kindOf(err) != Invalid {
		t.Fatalf("outside subnet err = %v", err)
	}
	if err := s.CreateSubnet("subnet-2", CreateSubnet{VPC: "vpc-1", CIDR: mustPrefix(t, "10.0.2.0/24"), Zone: "az1"}); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteDependencies(t *testing.T) {
	s := projectWithSubnet(t)
	if _, err := s.DeleteVPC(DeleteVPC{VPC: "vpc-1"}); kindOf(err) != Precondition {
		t.Fatalf("delete vpc with subnet = %v", err)
	}
	if _, err := s.DeleteSecurityGroup(DeleteSecurityGroup{Group: "sg-1"}); kindOf(err) != Precondition {
		t.Fatalf("delete default group = %v", err)
	}
	if err := s.CreateSecurityGroup("sg-app", CreateSecurityGroup{VPC: "vpc-1", Name: "app"}); err != nil {
		t.Fatal(err)
	}
	peer := Rule{Direction: Ingress, Protocol: TCP, FromPort: 22, ToPort: 22, Peer: Peer{Group: "sg-app"}}
	if err := s.AuthorizeRules(AuthorizeRules{Group: "sg-1", Rules: []Rule{peer}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSecurityGroup(DeleteSecurityGroup{Group: "sg-app"}); kindOf(err) != Precondition {
		t.Fatalf("delete referenced group = %v", err)
	}
	if _, err := s.DeleteSubnet(DeleteSubnet{Subnet: "missing"}); err != nil {
		t.Fatal(err)
	}
	found, err := s.DeleteSubnet(DeleteSubnet{Subnet: "missing"})
	if err != nil || found {
		t.Fatalf("absent subnet found %v err %v", found, err)
	}
}

func TestSecurityGroupRules(t *testing.T) {
	var s State
	if err := s.CreateVPC("vpc-1", "sg-1", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	rule := Rule{Direction: Ingress, Protocol: TCP, FromPort: 443, ToPort: 443, Peer: Peer{CIDR: mustPrefix(t, "0.0.0.0/0")}}
	if err := s.AuthorizeRules(AuthorizeRules{Group: "sg-1", Rules: []Rule{rule, rule}}); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Groups["sg-1"].Rules); got != 2 {
		t.Fatalf("rules = %d, want default egress plus one ingress", got)
	}
	found, err := s.RevokeRules(RevokeRules{Group: "sg-1", Rules: []Rule{rule}})
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if len(s.Groups["sg-1"].revoking) != 1 {
		t.Fatal("revoked rule was not marked")
	}
	found, err = s.RevokeRules(RevokeRules{Group: "sg-1", Rules: []Rule{rule}})
	if err != nil || !found {
		t.Fatalf("revoke still in progress found %v err %v", found, err)
	}
	if len(s.Groups["sg-1"].revoking) != 1 {
		t.Fatal("retry duplicated the revoke mark")
	}
	other := Rule{Direction: Ingress, Protocol: TCP, FromPort: 22, ToPort: 22, Peer: Peer{CIDR: mustPrefix(t, "192.0.2.0/24")}}
	found, err = s.RevokeRules(RevokeRules{Group: "sg-1", Rules: []Rule{other}})
	if err != nil || found {
		t.Fatalf("absent rule found %v err %v", found, err)
	}
	if err := s.AuthorizeRules(AuthorizeRules{Group: "sg-1", Rules: []Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	if len(s.Groups["sg-1"].revoking) != 0 {
		t.Fatal("re-authorize left the revoke mark")
	}
}

func TestDeleteVPCWithoutDefaultGroup(t *testing.T) {
	var s State
	if err := s.CreateVPC("vpc-1", "sg-1", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteVPC(DeleteVPC{VPC: "vpc-1"}); err != nil {
		t.Fatal(err)
	}
	observed := s.Clone()
	s.Settle("sg-1", observed)
	if _, ok := s.Groups["sg-1"]; ok {
		t.Fatal("settled default group remained")
	}
	if _, err := s.DeleteVPC(DeleteVPC{VPC: "vpc-1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Groups[""]; ok {
		t.Fatal("second delete stored an empty security group")
	}
	if s.VPCs["vpc-1"].Status != StatusDeleting {
		t.Fatal("vpc is not deleting")
	}
	actions, found, err := s.Plan("vpc-1")
	if err != nil || !found || len(actions) != 1 || actions[0].Kind != ActDeleteVPC {
		t.Fatalf("plan %+v found %v err %v", actions, found, err)
	}
}

func TestPlanRevokeAndSettleClearsMark(t *testing.T) {
	var s State
	if err := s.CreateVPC("vpc-1", "sg-1", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	rule := Rule{Direction: Ingress, Protocol: TCP, FromPort: 443, ToPort: 443, Peer: Peer{CIDR: mustPrefix(t, "0.0.0.0/0")}}
	if err := s.AuthorizeRules(AuthorizeRules{Group: "sg-1", Rules: []Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	actions, _, err := s.Plan("sg-1")
	if err != nil || !hasAction(actions, ActEnsureRule, rule) {
		t.Fatalf("ensure plan %+v err %v", actions, err)
	}
	if _, err := s.RevokeRules(RevokeRules{Group: "sg-1", Rules: []Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	actions, _, err = s.Plan("sg-1")
	if err != nil || !hasAction(actions, ActDeleteRule, rule) {
		t.Fatalf("delete plan %+v err %v", actions, err)
	}
	observed := s.Clone()
	s.Settle("sg-1", observed)
	if len(s.Groups["sg-1"].revoking) != 0 {
		t.Fatal("settle left the revoke mark")
	}
}

func TestRevokingPeerBlocksDelete(t *testing.T) {
	var s State
	if err := s.CreateVPC("vpc-1", "sg-1", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSecurityGroup("sg-2", CreateSecurityGroup{VPC: "vpc-1", Name: "peer"}); err != nil {
		t.Fatal(err)
	}
	rule := Rule{Direction: Ingress, Protocol: TCP, FromPort: 80, ToPort: 80, Peer: Peer{Group: "sg-2"}}
	if err := s.AuthorizeRules(AuthorizeRules{Group: "sg-1", Rules: []Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeRules(RevokeRules{Group: "sg-1", Rules: []Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteSecurityGroup(DeleteSecurityGroup{Group: "sg-2"}); kindOf(err) != Precondition {
		t.Fatalf("delete of revoking peer = %v", err)
	}
}

func TestPlanEnsuresPeerBeforeRule(t *testing.T) {
	var s State
	if err := s.CreateVPC("vpc-1", "sg-1", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSecurityGroup("sg-2", CreateSecurityGroup{VPC: "vpc-1", Name: "peer"}); err != nil {
		t.Fatal(err)
	}
	rule := Rule{Direction: Ingress, Protocol: TCP, FromPort: 80, ToPort: 80, Peer: Peer{Group: "sg-2"}}
	if err := s.AuthorizeRules(AuthorizeRules{Group: "sg-1", Rules: []Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	actions, _, err := s.Plan("vpc-1")
	if err != nil {
		t.Fatal(err)
	}
	peerAt, ruleAt := -1, -1
	for i, a := range actions {
		if a.Kind == ActEnsureGroup && a.Group.ID == "sg-2" {
			peerAt = i
		}
		if a.Kind == ActEnsureRule && a.Rule == rule {
			ruleAt = i
		}
	}
	if peerAt < 0 || ruleAt < 0 || peerAt > ruleAt {
		t.Fatalf("peer %d rule %d plan %+v", peerAt, ruleAt, actions)
	}
}

func TestPlanDeletesChildOfFailedVPC(t *testing.T) {
	s := projectWithSubnet(t)
	pool := mustPrefix(t, "198.51.100.0/28")
	if err := s.AllocateElasticIP("eip-1", AllocateElasticIP{VPC: "vpc-1"}, pool, func(netip.Addr) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSecurityGroup("sg-2", CreateSecurityGroup{VPC: "vpc-1", Name: "extra"}); err != nil {
		t.Fatal(err)
	}
	s.Fail("vpc-1")
	if _, _, err := s.Plan("subnet-1"); kindOf(err) != Precondition {
		t.Fatalf("ensure under failed vpc = %v", err)
	}
	if _, err := s.DeleteSubnet(DeleteSubnet{Subnet: "subnet-1"}); err != nil {
		t.Fatal(err)
	}
	actions, _, err := s.Plan("subnet-1")
	if err != nil || len(actions) != 1 || actions[0].Kind != ActDeleteSubnet {
		t.Fatalf("delete subnet plan %+v err %v", actions, err)
	}
	if _, err := s.DeleteSecurityGroup(DeleteSecurityGroup{Group: "sg-2"}); err != nil {
		t.Fatal(err)
	}
	actions, _, err = s.Plan("sg-2")
	if err != nil || actions[len(actions)-1].Kind != ActDeleteGroup {
		t.Fatalf("delete group plan %+v err %v", actions, err)
	}
	if _, err := s.ReleaseElasticIP(ReleaseElasticIP{ElasticIP: "eip-1"}); err != nil {
		t.Fatal(err)
	}
	actions, _, err = s.Plan("eip-1")
	if err != nil || actions[len(actions)-1].Kind != ActDeleteEIP {
		t.Fatalf("delete eip plan %+v err %v", actions, err)
	}
}

func TestRequestJSONNames(t *testing.T) {
	b, err := json.Marshal(CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"cidr":"10.0.0.0/16"}` {
		t.Fatalf("create vpc json %s", b)
	}
	var got CreateVPC
	if err := json.Unmarshal([]byte(`{"cidr":"10.2.0.0/16"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.CIDR != mustPrefix(t, "10.2.0.0/16") {
		t.Fatalf("decoded %v", got.CIDR)
	}
	rule := Rule{Direction: Ingress, Protocol: TCP, FromPort: 22, ToPort: 22, Peer: Peer{Group: "sg-2"}}
	b, err = json.Marshal(AuthorizeRules{Group: "sg-1", Rules: []Rule{rule}})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b) || !strings.Contains(string(b), `"group":"sg-1"`) || !strings.Contains(string(b), `"from_port":22`) {
		t.Fatalf("authorize json %s", b)
	}
}

func hasAction(actions []Action, kind ActionKind, rule Rule) bool {
	for _, a := range actions {
		if a.Kind == kind && a.Rule == rule {
			return true
		}
	}
	return false
}

func TestElasticIPRules(t *testing.T) {
	s := projectWithSubnet(t)
	pool := mustPrefix(t, "198.51.100.0/28")
	taken := map[netip.Addr]bool{}
	if err := s.AllocateElasticIP("eip-1", AllocateElasticIP{VPC: "vpc-1"}, pool, func(a netip.Addr) bool { return taken[a] }); err != nil {
		t.Fatal(err)
	}
	addr := s.ElasticIPs["eip-1"].Address
	if addr != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("allocated %v", addr)
	}
	before := s.Clone()
	err := s.CreateVPC("vpc-9", "sg-9", CreateVPC{CIDR: mustPrefix(t, "198.51.100.0/24")})
	if kindOf(err) != Conflict || !reflect.DeepEqual(s, before) {
		t.Fatalf("vpc containing eip err=%v mutated=%v", err, !reflect.DeepEqual(s, before))
	}
	if err := s.AssociateElasticIP(AssociateElasticIP{ElasticIP: "eip-1", Private: netip.MustParseAddr("10.0.1.1")}); kindOf(err) != Precondition {
		t.Fatalf("reserved host err = %v", err)
	}
	if err := s.AssociateElasticIP(AssociateElasticIP{ElasticIP: "eip-1", Private: netip.MustParseAddr("10.0.1.4")}); err != nil {
		t.Fatal(err)
	}
	if s.ElasticIPs["eip-1"].Subnet != "subnet-1" {
		t.Fatal("association did not record the subnet")
	}
	if _, err := s.DeleteSubnet(DeleteSubnet{Subnet: "subnet-1"}); kindOf(err) != Precondition {
		t.Fatal(err)
	}
	if _, err := s.ReleaseElasticIP(ReleaseElasticIP{ElasticIP: "eip-1"}); kindOf(err) != Precondition {
		t.Fatal(err)
	}
	found, err := s.DisassociateElasticIP(DisassociateElasticIP{ElasticIP: "eip-1"})
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if s.ElasticIPs["eip-1"].Private.IsValid() || len(s.ElasticIPs["eip-1"].detaching) != 1 {
		t.Fatal("disassociate did not move the address")
	}
	if _, err := s.DeleteSubnet(DeleteSubnet{Subnet: "subnet-1"}); kindOf(err) != Precondition {
		t.Fatal("detach in progress allowed subnet delete")
	}
}

func TestPlanAndSettle(t *testing.T) {
	var s State
	if err := s.CreateVPC("vpc-1", "sg-1", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	actions, found, err := s.Plan("vpc-1")
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if actions[0].Kind != ActEnsureVPC || actions[1].Kind != ActEnsureGroup || actions[2].Kind != ActEnsureRule {
		t.Fatalf("plan = %+v", actions)
	}
	observed := s.Clone()
	if freed := s.Settle("vpc-1", observed); len(freed) != 0 || s.VPCs["vpc-1"].Status != StatusAvailable {
		t.Fatalf("settle status %v", s.VPCs["vpc-1"].Status)
	}
	pool := mustPrefix(t, "198.51.100.0/28")
	if err := s.AllocateElasticIP("eip-1", AllocateElasticIP{VPC: "vpc-1"}, pool, func(netip.Addr) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseElasticIP(ReleaseElasticIP{ElasticIP: "eip-1"}); err != nil {
		t.Fatal(err)
	}
	observed = s.Clone()
	freed := s.Settle("eip-1", observed)
	if len(freed) != 1 || freed[0] != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("freed %v", freed)
	}
	if _, ok := s.ElasticIPs["eip-1"]; ok {
		t.Fatal("released eip still present")
	}
	_, found, err = s.Plan("eip-1")
	if err != nil || found {
		t.Fatal("absent id should not plan")
	}
}

func TestFailAllowsOnlyDelete(t *testing.T) {
	var s State
	if err := s.CreateVPC("vpc-1", "sg-1", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	s.Fail("vpc-1")
	if s.VPCs["vpc-1"].Status != StatusFailed || s.Groups["sg-1"].Status != StatusFailed {
		t.Fatal("fail did not mark vpc and default group")
	}
	if err := s.CreateSubnet("subnet-1", CreateSubnet{VPC: "vpc-1", CIDR: mustPrefix(t, "10.0.1.0/24"), Zone: "az1"}); kindOf(err) != Precondition {
		t.Fatalf("create under failed vpc = %v", err)
	}
	found, err := s.DeleteVPC(DeleteVPC{VPC: "vpc-1"})
	if err != nil || !found || s.VPCs["vpc-1"].Status != StatusDeleting {
		t.Fatalf("delete failed vpc found %v err %v status %v", found, err, s.VPCs["vpc-1"].Status)
	}
}

func TestMarshalStateRoundTrip(t *testing.T) {
	s := projectWithSubnet(t)
	rule := Rule{Direction: Ingress, Protocol: TCP, FromPort: 80, ToPort: 80, Peer: Peer{CIDR: mustPrefix(t, "192.0.2.0/24")}}
	if err := s.AuthorizeRules(AuthorizeRules{Group: "sg-1", Rules: []Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RevokeRules(RevokeRules{Group: "sg-1", Rules: []Rule{rule}}); err != nil {
		t.Fatal(err)
	}
	b, err := MarshalState(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalState(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, got) {
		t.Fatalf("round trip\n got %#v\nwant %#v", got, s)
	}
	if _, err := UnmarshalState([]byte(`{"v":3}`)); err == nil {
		t.Fatal("future version was accepted")
	}
}

func TestPlanKindKeepsSiblingID(t *testing.T) {
	var s State
	if err := s.CreateVPC("app", "sg-def", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16"), Region: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSubnet("app", CreateSubnet{VPC: "app", CIDR: mustPrefix(t, "10.0.1.0/24"), Zone: "az1", Region: "r1"}); err != nil {
		t.Fatal(err)
	}
	actions, found, err := s.PlanKind(KindSubnet, "app")
	if err != nil || !found || len(actions) == 0 || actions[0].Kind != ActEnsureSubnet {
		t.Fatalf("plan %+v found %v err %v", actions, found, err)
	}
	s.FailKind(KindSubnet, "app")
	if s.VPCs["app"].Status != StatusPending || s.Subnets["app"].Status != StatusFailed {
		t.Fatalf("vpc %d subnet %d", s.VPCs["app"].Status, s.Subnets["app"].Status)
	}
}

func TestLabelsRoundTrip(t *testing.T) {
	labels := map[string]string{"vws-fingerprint": "fp-1", "team": "net"}
	var s State
	if err := s.CreateVPC("app", "sg-1", CreateVPC{
		CIDR: mustPrefix(t, "10.0.0.0/16"), Region: "r1",
		Text: Text{Description: "edge", Labels: labels},
	}); err != nil {
		t.Fatal(err)
	}
	labels["vws-fingerprint"] = "mutated"
	vpc := s.VPCs["app"]
	if vpc.Description != "edge" || vpc.Labels["vws-fingerprint"] != "fp-1" || vpc.Labels["team"] != "net" {
		t.Fatalf("vpc text %+v", vpc)
	}
	if g := s.Groups["sg-1"]; g.Description != "" || len(g.Labels) != 0 {
		t.Fatalf("default group copied vpc text %+v", g)
	}
	if err := s.CreateSubnet("web", CreateSubnet{
		VPC: "app", CIDR: mustPrefix(t, "10.0.1.0/24"), Zone: "az1",
		Text: Text{Description: "web tier", Labels: map[string]string{"vws-fingerprint": "sub"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSecurityGroup("sg-web", CreateSecurityGroup{
		VPC: "app", Name: "web", Text: Text{Description: "ssh", Labels: map[string]string{"role": "web"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllocateElasticIP("eip-1", AllocateElasticIP{
		VPC: "app", Labels: map[string]string{"vws-fingerprint": "addr"},
	}, mustPrefix(t, "198.51.100.0/24"), func(netip.Addr) bool { return false }); err != nil {
		t.Fatal(err)
	}

	before := s.Clone()
	err := s.CreateVPC("other", "sg-2", CreateVPC{CIDR: mustPrefix(t, "10.1.0.0/16"), Text: Text{Labels: map[string]string{"Bad": "x"}}})
	if kindOf(err) != Invalid || !reflect.DeepEqual(s, before) {
		t.Fatalf("bad label err %v mutated %v", err, !reflect.DeepEqual(s, before))
	}
	tooMany := map[string]string{}
	for i := 0; i < 65; i++ {
		tooMany[labelKey(i)] = "v"
	}
	if kindOf(CheckLabels(tooMany)) != Invalid {
		t.Fatal("65 labels accepted")
	}
	okLabels := map[string]string{}
	for i := 0; i < 64; i++ {
		okLabels[labelKey(i)] = "v"
	}
	if err := CheckLabels(okLabels); err != nil {
		t.Fatal(err)
	}
	if kindOf(CheckLabels(map[string]string{"vws-fingerprint": strings.Repeat("a", 64)})) != Invalid {
		t.Fatal("long value accepted")
	}
	if kindOf(CheckLabels(map[string]string{"vws-fingerprint": "a\nb"})) != Invalid {
		t.Fatal("control character accepted")
	}
	if kindOf(CheckDescription(strings.Repeat("a", 1025))) != Invalid {
		t.Fatal("long description accepted")
	}
	if err := CheckDescription(strings.Repeat("a", 1024)); err != nil {
		t.Fatal(err)
	}

	changed, err := s.ApplyVPCText("app", CreateVPC{Text: Text{SetDescription: true, Description: "edge-2", SetLabels: true}})
	if err != nil || !changed || s.VPCs["app"].Description != "edge-2" || len(s.VPCs["app"].Labels) != 0 {
		t.Fatalf("clear labels changed %v vpc %+v err %v", changed, s.VPCs["app"], err)
	}
	if s.VPCs["app"].CIDR != mustPrefix(t, "10.0.0.0/16") {
		t.Fatal("text update changed cidr")
	}
	changed, err = s.ApplySubnetText("web", CreateSubnet{Text: Text{SetLabels: true, Labels: map[string]string{"vws-fingerprint": "sub-2"}}})
	if err != nil || !changed || s.Subnets["web"].Description != "web tier" || s.Subnets["web"].Labels["vws-fingerprint"] != "sub-2" {
		t.Fatalf("subnet text %+v err %v", s.Subnets["web"], err)
	}
	changed, err = s.ApplyGroupText("sg-web", CreateSecurityGroup{Text: Text{SetDescription: true, Description: "ssh-2"}})
	if err != nil || !changed || s.Groups["sg-web"].Description != "ssh-2" || s.Groups["sg-web"].Labels["role"] != "web" {
		t.Fatalf("group text %+v err %v", s.Groups["sg-web"], err)
	}
	changed, err = s.ApplyAddressText("eip-1", map[string]string{"vws-fingerprint": "addr-2"}, true)
	if err != nil || !changed || s.ElasticIPs["eip-1"].Labels["vws-fingerprint"] != "addr-2" {
		t.Fatalf("address text %+v err %v", s.ElasticIPs["eip-1"], err)
	}
	changed, err = s.ApplyVPCText("app", CreateVPC{Text: Text{SetDescription: true, Description: "edge-2"}})
	if err != nil || changed {
		t.Fatalf("identical text changed %v err %v", changed, err)
	}

	blob, err := MarshalState(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"v":2`) || !strings.Contains(string(blob), `"vws-fingerprint":"sub-2"`) {
		t.Fatalf("document %s", blob)
	}
	got, err := UnmarshalState(blob)
	if err != nil {
		t.Fatal(err)
	}
	if got.VPCs["app"].Description != "edge-2" || len(got.VPCs["app"].Labels) != 0 || got.Subnets["web"].Labels["vws-fingerprint"] != "sub-2" || got.Groups["sg-web"].Description != "ssh-2" || got.ElasticIPs["eip-1"].Labels["vws-fingerprint"] != "addr-2" {
		t.Fatalf("round trip %+v", got)
	}
	legacy, err := UnmarshalState([]byte(`{"v":1,"vpcs":[{"id":"old","cidr":"10.8.0.0/16","default_group":"sg-old","region":"r1","status":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	old := legacy.VPCs["old"]
	if old.CIDR != mustPrefix(t, "10.8.0.0/16") || old.Region != "r1" || old.Description != "" || len(old.Labels) != 0 {
		t.Fatalf("v1 document %+v", old)
	}
}

func TestEmptyLabelIsADistinctEntry(t *testing.T) {
	if SameLabels(map[string]string{"pending": ""}, map[string]string{"vws-fingerprint": "fp-2"}) {
		t.Fatal("a missing key matched an empty value")
	}
	if !SameLabels(nil, map[string]string{}) {
		t.Fatal("nil and empty labels differ")
	}
	var s State
	if err := s.CreateVPC("app", "sg-1", CreateVPC{
		CIDR: mustPrefix(t, "10.0.0.0/16"),
		Text: Text{Labels: map[string]string{"pending": ""}},
	}); err != nil {
		t.Fatal(err)
	}
	changed, err := s.ApplyVPCText("app", CreateVPC{Text: Text{SetLabels: true, Labels: map[string]string{"vws-fingerprint": "fp-2"}}})
	if err != nil || !changed || s.VPCs["app"].Labels["vws-fingerprint"] != "fp-2" || len(s.VPCs["app"].Labels) != 1 {
		t.Fatalf("changed %v vpc %+v err %v", changed, s.VPCs["app"], err)
	}
	v := s.VPCs["app"]
	v.Status = StatusDeleting
	s.VPCs["app"] = v
	changed, err = s.ApplyVPCText("app", CreateVPC{Text: Text{SetLabels: true, Labels: map[string]string{"vws-fingerprint": "fp-2"}}})
	if err != nil || changed {
		t.Fatalf("unchanged text on a deleting vpc changed %v err %v", changed, err)
	}
	_, err = s.ApplyVPCText("app", CreateVPC{Text: Text{SetDescription: true, Description: "later"}})
	if kindOf(err) != Precondition || s.VPCs["app"].Description != "" {
		t.Fatalf("deleting vpc accepted a new description: %v %+v", err, s.VPCs["app"])
	}
	other := s.Clone()
	other.VPCs["app"] = VPC{
		ID: "app", CIDR: v.CIDR, DefaultGroup: v.DefaultGroup, Status: StatusDeleting,
		Description: "stored", Labels: map[string]string{"vws-fingerprint": "other"},
	}
	if !s.SameWork(other) || !s.TextOnly(other) {
		t.Fatal("text changed the dataplane comparison")
	}
	if s.TextOnly(s.Clone()) {
		t.Fatal("an equal state is text-only")
	}
	other.VPCs["app"] = VPC{ID: "app", CIDR: mustPrefix(t, "10.1.0.0/16"), DefaultGroup: v.DefaultGroup, Status: StatusDeleting}
	if s.SameWork(other) {
		t.Fatal("a cidr change compared equal")
	}
}

func labelKey(i int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	return "k" + string(alphabet[i%26]) + strings.Repeat("x", i/26)
}

func projectWithSubnet(t *testing.T) State {
	t.Helper()
	var s State
	if err := s.CreateVPC("vpc-1", "sg-1", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSubnet("subnet-1", CreateSubnet{VPC: "vpc-1", CIDR: mustPrefix(t, "10.0.1.0/24"), Zone: "az1"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestElasticIPInstanceAssociation(t *testing.T) {
	var s State
	if err := s.CreateVPC("vpc-1", "sg-1", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	pool := mustPrefix(t, "198.51.100.0/24")
	free := func(netip.Addr) bool { return false }
	const instance = "projects/p/zones/z1/instances/i1"
	if err := s.AllocateElasticIP("eip-1", AllocateElasticIP{VPC: "vpc-1", Instance: instance, Device: "nic0"}, pool, free); err != nil {
		t.Fatal(err)
	}
	if e := s.ElasticIPs["eip-1"]; e.Instance != instance || e.Device != "nic0" {
		t.Fatalf("stored attachment = %q %q", e.Instance, e.Device)
	}
	before := s.Clone()
	if err := s.AllocateElasticIP("eip-2", AllocateElasticIP{VPC: "vpc-1", Instance: instance, Device: "nic0"}, pool, free); kindOf(err) != Invalid {
		t.Fatalf("second address on one interface err = %v", err)
	}
	if err := s.AllocateElasticIP("eip-2", AllocateElasticIP{VPC: "vpc-1", Instance: instance}, pool, free); kindOf(err) != Invalid {
		t.Fatalf("half attachment err = %v", err)
	}
	if !reflect.DeepEqual(s, before) {
		t.Fatal("rejected allocate mutated state")
	}
	if err := s.AllocateElasticIP("eip-2", AllocateElasticIP{VPC: "vpc-1"}, pool, free); err != nil {
		t.Fatal(err)
	}
	if changed, err := s.SetElasticIPAssociation("eip-2", instance, "nic0"); kindOf(err) != Invalid || changed {
		t.Fatalf("attach to a taken interface = %v, %v", changed, err)
	}
	if changed, err := s.SetElasticIPAssociation("eip-2", instance, "nic1"); err != nil || !changed {
		t.Fatalf("attach to a free interface = %v, %v", changed, err)
	}
	if changed, err := s.SetElasticIPAssociation("eip-2", instance, "nic1"); err != nil || changed {
		t.Fatalf("same attachment again = %v, %v", changed, err)
	}
	if _, err := s.ReleaseElasticIP(ReleaseElasticIP{ElasticIP: "eip-2"}); kindOf(err) != Precondition {
		t.Fatalf("release while attached err = %v", err)
	}
	if changed, err := s.SetElasticIPAssociation("eip-2", "", ""); err != nil || !changed {
		t.Fatalf("clear = %v, %v", changed, err)
	}
	if changed, err := s.SetElasticIPAssociation("eip-2", "", ""); err != nil || changed {
		t.Fatalf("clear again = %v, %v", changed, err)
	}
	if changed, err := s.ReleaseElasticIP(ReleaseElasticIP{ElasticIP: "eip-2"}); err != nil || !changed {
		t.Fatalf("release after clear = %v, %v", changed, err)
	}
	// A deleting address still holds its interface until it is removed.
	if _, err := s.ReleaseElasticIP(ReleaseElasticIP{ElasticIP: "eip-1"}); kindOf(err) != Precondition {
		t.Fatalf("release attached eip-1 err = %v", err)
	}
	if err := s.AssociateElasticIP(AssociateElasticIP{ElasticIP: "eip-1", Private: netip.MustParseAddr("10.0.0.4")}); kindOf(err) != Precondition {
		t.Fatalf("private bind while attached err = %v", err)
	}
}

func TestElasticIPAssociationSurvivesEncoding(t *testing.T) {
	var s State
	if err := s.CreateVPC("vpc-1", "sg-1", CreateVPC{CIDR: mustPrefix(t, "10.0.0.0/16")}); err != nil {
		t.Fatal(err)
	}
	const instance = "projects/p/zones/z1/instances/i1"
	if err := s.AllocateElasticIP("eip-1", AllocateElasticIP{VPC: "vpc-1", Instance: instance, Device: "nic0"}, mustPrefix(t, "198.51.100.0/24"), func(netip.Addr) bool { return false }); err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalState(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalState(raw)
	if err != nil {
		t.Fatal(err)
	}
	if e := got.ElasticIPs["eip-1"]; e.Instance != instance || e.Device != "nic0" {
		t.Fatalf("decoded attachment = %q %q", e.Instance, e.Device)
	}
}

func attachedState() State {
	p := netip.MustParsePrefix
	return State{
		VPCs:    map[VPCID]VPC{"vpc-1": {ID: "vpc-1", CIDR: p("10.8.0.0/16"), Status: StatusAvailable}},
		Subnets: map[SubnetID]Subnet{"subnet-1": {ID: "subnet-1", VPC: "vpc-1", CIDR: p("10.8.1.0/24"), Status: StatusAvailable}},
		ElasticIPs: map[ElasticIPID]ElasticIP{"eip-1": {
			ID: "eip-1", VPC: "vpc-1", Address: netip.MustParseAddr("198.51.100.7"),
			Instance: "projects/p/zones/z/instances/i-1", Device: "eth0", Status: StatusAvailable,
		}},
	}
}

// TestAttachedAddressPlansResolve: an address attached to an instance with no
// private address yet asks the worker to resolve it, and NAT follows once
// it is bound.
func TestAttachedAddressPlansResolve(t *testing.T) {
	st := attachedState()
	acts, _, err := st.PlanKind(KindAddress, "eip-1")
	if err != nil || len(acts) != 2 || acts[1].Kind != ActResolveNAT {
		t.Fatalf("plan %v %v", acts, err)
	}
	if err := st.BindInterfaceAddress("eip-1", netip.MustParseAddr("10.8.1.5")); err != nil {
		t.Fatal(err)
	}
	if e := st.ElasticIPs["eip-1"]; e.Subnet != "subnet-1" {
		t.Fatalf("subnet %q", e.Subnet)
	}
	acts, _, err = st.PlanKind(KindAddress, "eip-1")
	if err != nil || len(acts) != 2 || acts[1].Kind != ActEnsureNAT || acts[1].Addr != netip.MustParseAddr("10.8.1.5") {
		t.Fatalf("plan after bind %v", acts)
	}
}

func TestBindInterfaceAddressOutsideTheVPCIsInvalid(t *testing.T) {
	st := attachedState()
	err := st.BindInterfaceAddress("eip-1", netip.MustParseAddr("10.42.0.9"))
	var de *Error
	if !errors.As(err, &de) || de.Kind != Invalid {
		t.Fatalf("err %v, want Invalid", err)
	}
}

// TestSettleCarriesTheResolvedAddressOnlyForTheSameAttachment: a resolved
// address is stored while the attachment is unchanged; when it changed under
// the worker, the address goes to detaching so its NAT is removed.
func TestSettleCarriesTheResolvedAddressOnlyForTheSameAttachment(t *testing.T) {
	addr := netip.MustParseAddr("10.8.1.5")
	cur := attachedState()
	observed := cur.Clone()
	if err := observed.BindInterfaceAddress("eip-1", addr); err != nil {
		t.Fatal(err)
	}
	cur.SettleKind(KindAddress, "eip-1", observed)
	if cur.ElasticIPs["eip-1"].Private != addr {
		t.Fatalf("same attachment: private %v", cur.ElasticIPs["eip-1"].Private)
	}

	cur = attachedState()
	if _, err := cur.SetElasticIPAssociation("eip-1", "", ""); err != nil {
		t.Fatal(err)
	}
	cur.SettleKind(KindAddress, "eip-1", observed)
	e := cur.ElasticIPs["eip-1"]
	if e.Private.IsValid() || len(e.detaching) != 1 || e.detaching[0] != addr {
		t.Fatalf("changed attachment: private %v detaching %v", e.Private, e.detaching)
	}
	acts, _, err := cur.PlanKind(KindAddress, "eip-1")
	if err != nil || acts[len(acts)-1].Kind != ActDeleteNAT || acts[len(acts)-1].Addr != addr {
		t.Fatalf("plan after detach %v", acts)
	}
}
