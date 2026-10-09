package domain

import (
	"encoding/json"
	"net/netip"
)

const stateVersion = 1

// MarshalState encodes State with a versioned document. The JSON names are the
// storage contract. Renaming a Go field does not change them.
func MarshalState(s State) ([]byte, error) {
	doc := stateDoc{V: stateVersion}
	for _, v := range s.VPCs {
		doc.VPCs = append(doc.VPCs, vpcDoc{
			ID: string(v.ID), CIDR: v.CIDR.String(), DefaultGroup: string(v.DefaultGroup), Status: int(v.Status),
		})
	}
	for _, sub := range s.Subnets {
		doc.Subnets = append(doc.Subnets, subnetDoc{
			ID: string(sub.ID), VPC: string(sub.VPC), CIDR: sub.CIDR.String(), Zone: sub.Zone, Status: int(sub.Status),
		})
	}
	for _, g := range s.Groups {
		doc.Groups = append(doc.Groups, groupDoc{
			ID: string(g.ID), VPC: string(g.VPC), Name: g.Name, Default: g.Default,
			Rules: encodeRules(g.Rules), Revoking: encodeRules(g.revoking), Status: int(g.Status),
		})
	}
	for _, e := range s.ElasticIPs {
		item := eipDoc{
			ID: string(e.ID), VPC: string(e.VPC), Address: e.Address.String(),
			Subnet: string(e.Subnet), Status: int(e.Status),
		}
		if e.Private.IsValid() {
			item.Private = e.Private.String()
		}
		for _, addr := range e.detaching {
			item.Detaching = append(item.Detaching, addr.String())
		}
		doc.EIPs = append(doc.EIPs, item)
	}
	return json.Marshal(doc)
}

// UnmarshalState decodes a document produced by MarshalState.
func UnmarshalState(b []byte) (State, error) {
	if len(b) == 0 {
		return State{}, nil
	}
	var doc stateDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return State{}, invalid("state document is corrupt")
	}
	if doc.V != stateVersion {
		return State{}, invalid("state document version is unsupported")
	}
	var s State
	s.init()
	for _, v := range doc.VPCs {
		cidr, err := netip.ParsePrefix(v.CIDR)
		if err != nil {
			return State{}, invalid("state document cidr is corrupt")
		}
		s.VPCs[VPCID(v.ID)] = VPC{ID: VPCID(v.ID), CIDR: cidr, DefaultGroup: SecurityGroupID(v.DefaultGroup), Status: Status(v.Status)}
	}
	for _, sub := range doc.Subnets {
		cidr, err := netip.ParsePrefix(sub.CIDR)
		if err != nil {
			return State{}, invalid("state document cidr is corrupt")
		}
		s.Subnets[SubnetID(sub.ID)] = Subnet{ID: SubnetID(sub.ID), VPC: VPCID(sub.VPC), CIDR: cidr, Zone: sub.Zone, Status: Status(sub.Status)}
	}
	for _, g := range doc.Groups {
		rules, err := decodeRules(g.Rules)
		if err != nil {
			return State{}, err
		}
		revoking, err := decodeRules(g.Revoking)
		if err != nil {
			return State{}, err
		}
		s.Groups[SecurityGroupID(g.ID)] = SecurityGroup{
			ID: SecurityGroupID(g.ID), VPC: VPCID(g.VPC), Name: g.Name, Default: g.Default,
			Rules: rules, revoking: revoking, Status: Status(g.Status),
		}
	}
	for _, e := range doc.EIPs {
		addr, err := netip.ParseAddr(e.Address)
		if err != nil {
			return State{}, invalid("state document address is corrupt")
		}
		item := ElasticIP{ID: ElasticIPID(e.ID), VPC: VPCID(e.VPC), Address: addr, Subnet: SubnetID(e.Subnet), Status: Status(e.Status)}
		if e.Private != "" {
			item.Private, err = netip.ParseAddr(e.Private)
			if err != nil {
				return State{}, invalid("state document address is corrupt")
			}
		}
		for _, raw := range e.Detaching {
			detach, err := netip.ParseAddr(raw)
			if err != nil {
				return State{}, invalid("state document address is corrupt")
			}
			item.detaching = append(item.detaching, detach)
		}
		s.ElasticIPs[item.ID] = item
	}
	return s, nil
}

type stateDoc struct {
	V       int         `json:"v"`
	VPCs    []vpcDoc    `json:"vpcs,omitempty"`
	Subnets []subnetDoc `json:"subnets,omitempty"`
	Groups  []groupDoc  `json:"groups,omitempty"`
	EIPs    []eipDoc    `json:"eips,omitempty"`
}

type vpcDoc struct {
	ID           string `json:"id"`
	CIDR         string `json:"cidr"`
	DefaultGroup string `json:"default_group"`
	Status       int    `json:"status"`
}

type subnetDoc struct {
	ID     string `json:"id"`
	VPC    string `json:"vpc"`
	CIDR   string `json:"cidr"`
	Zone   string `json:"zone"`
	Status int    `json:"status"`
}

type groupDoc struct {
	ID       string    `json:"id"`
	VPC      string    `json:"vpc"`
	Name     string    `json:"name"`
	Default  bool      `json:"default"`
	Rules    []ruleDoc `json:"rules,omitempty"`
	Revoking []ruleDoc `json:"revoking,omitempty"`
	Status   int       `json:"status"`
}

type eipDoc struct {
	ID        string   `json:"id"`
	VPC       string   `json:"vpc"`
	Address   string   `json:"address"`
	Private   string   `json:"private,omitempty"`
	Subnet    string   `json:"subnet,omitempty"`
	Detaching []string `json:"detaching,omitempty"`
	Status    int      `json:"status"`
}

type ruleDoc struct {
	Direction int    `json:"direction"`
	Protocol  int    `json:"protocol"`
	FromPort  int32  `json:"from_port"`
	ToPort    int32  `json:"to_port"`
	CIDR      string `json:"cidr,omitempty"`
	Group     string `json:"group,omitempty"`
}

func encodeRules(rules []Rule) []ruleDoc {
	if len(rules) == 0 {
		return nil
	}
	out := make([]ruleDoc, len(rules))
	for i, r := range rules {
		out[i] = ruleDoc{
			Direction: int(r.Direction), Protocol: int(r.Protocol),
			FromPort: r.FromPort, ToPort: r.ToPort, Group: string(r.Peer.Group),
		}
		if r.Peer.CIDR.IsValid() {
			out[i].CIDR = r.Peer.CIDR.String()
		}
	}
	return out
}

func decodeRules(docs []ruleDoc) ([]Rule, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	out := make([]Rule, len(docs))
	for i, d := range docs {
		out[i] = Rule{
			Direction: Direction(d.Direction), Protocol: Protocol(d.Protocol),
			FromPort: d.FromPort, ToPort: d.ToPort, Peer: Peer{Group: SecurityGroupID(d.Group)},
		}
		if d.CIDR != "" {
			p, err := netip.ParsePrefix(d.CIDR)
			if err != nil {
				return nil, invalid("state document cidr is corrupt")
			}
			out[i].Peer.CIDR = p
		}
	}
	return out, nil
}
