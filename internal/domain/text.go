package domain

import (
	"maps"
	"reflect"
	"regexp"
	"unicode/utf8"
)

const maxDescriptionBytes = 1024

// labelKeyPattern is the resource-envelope key grammar:
// [a-z][a-z0-9_-]{0,62}.
var labelKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

// CheckDescription accepts a UTF-8 string of at most 1024 bytes.
func CheckDescription(description string) error {
	if len(description) > maxDescriptionBytes {
		return invalid("description is too long")
	}
	if !utf8.ValidString(description) {
		return invalid("description is not utf-8")
	}
	return nil
}

// CheckLabels accepts at most 64 entries. Keys match [a-z][a-z0-9_-]{0,62}.
// Values are 0 to 63 bytes of UTF-8 with no ASCII control characters.
func CheckLabels(labels map[string]string) error {
	if len(labels) > 64 {
		return invalid("labels exceed 64 entries")
	}
	for key, value := range labels {
		if !labelKeyPattern.MatchString(key) {
			return invalid("label key is invalid")
		}
		if len(value) > 63 {
			return invalid("label value is too long")
		}
		if !utf8.ValidString(value) {
			return invalid("label value is not utf-8")
		}
		for i := 0; i < len(value); i++ {
			if value[i] < 0x20 || value[i] == 0x7F {
				return invalid("label value has a control character")
			}
		}
	}
	return nil
}

// SameLabels reports whether two label maps carry the same entries.
// A nil map and an empty map are the same. A missing key is not an empty value.
func SameLabels(a, b map[string]string) bool {
	return maps.Equal(a, b)
}

func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// assignText validates and applies the selected fields. It does not alias in.
func assignText(curDesc string, curLabels map[string]string, text Text) (string, map[string]string, bool, error) {
	if text.SetDescription {
		if err := CheckDescription(text.Description); err != nil {
			return curDesc, curLabels, false, err
		}
	}
	if text.SetLabels {
		if err := CheckLabels(text.Labels); err != nil {
			return curDesc, curLabels, false, err
		}
	}
	desc := curDesc
	labels := curLabels
	changed := false
	if text.SetDescription && curDesc != text.Description {
		desc = text.Description
		changed = true
	}
	if text.SetLabels && !SameLabels(curLabels, text.Labels) {
		labels = copyLabels(text.Labels)
		changed = true
	}
	return desc, labels, changed, nil
}

func checkStoredText(text Text) error {
	if err := CheckDescription(text.Description); err != nil {
		return err
	}
	return CheckLabels(text.Labels)
}

// ApplyVPCText replaces the description and labels selected by r.
// An unchanged request succeeds even when the VPC is deleting or failed.
func (s *State) ApplyVPCText(id VPCID, r CreateVPC) (bool, error) {
	s.init()
	v, ok := s.VPCs[id]
	if !ok {
		return false, notFound("vpc not found")
	}
	desc, labels, changed, err := assignText(v.Description, v.Labels, r.Text)
	if err != nil || !changed {
		return false, err
	}
	if err := usable(v.Status); err != nil {
		return false, err
	}
	v.Description = desc
	v.Labels = labels
	s.VPCs[id] = v
	return true, nil
}

// ApplySubnetText replaces the description and labels selected by r.
func (s *State) ApplySubnetText(id SubnetID, r CreateSubnet) (bool, error) {
	s.init()
	sub, ok := s.Subnets[id]
	if !ok {
		return false, notFound("subnet not found")
	}
	desc, labels, changed, err := assignText(sub.Description, sub.Labels, r.Text)
	if err != nil || !changed {
		return false, err
	}
	if err := usable(sub.Status); err != nil {
		return false, err
	}
	sub.Description = desc
	sub.Labels = labels
	s.Subnets[id] = sub
	return true, nil
}

// ApplyGroupText replaces the description and labels selected by r.
func (s *State) ApplyGroupText(id SecurityGroupID, r CreateSecurityGroup) (bool, error) {
	s.init()
	g, ok := s.Groups[id]
	if !ok {
		return false, notFound("security group not found")
	}
	desc, labels, changed, err := assignText(g.Description, g.Labels, r.Text)
	if err != nil || !changed {
		return false, err
	}
	if err := usable(g.Status); err != nil {
		return false, err
	}
	g.Description = desc
	g.Labels = labels
	s.Groups[id] = g
	return true, nil
}

// ApplyAddressText replaces labels when set is true. An address has no description.
func (s *State) ApplyAddressText(id ElasticIPID, labels map[string]string, set bool) (bool, error) {
	s.init()
	e, ok := s.ElasticIPs[id]
	if !ok {
		return false, notFound("elastic ip not found")
	}
	_, next, changed, err := assignText("", e.Labels, Text{Labels: labels, SetLabels: set})
	if err != nil || !changed {
		return false, err
	}
	if err := usable(e.Status); err != nil {
		return false, err
	}
	e.Labels = next
	s.ElasticIPs[id] = e
	return true, nil
}

// SameWork reports whether two states ask the dataplane for the same work.
// Description and labels are stored text.
func (s State) SameWork(other State) bool {
	a := s.Clone()
	b := other.Clone()
	a.clearText()
	b.clearText()
	return reflect.DeepEqual(a, b)
}

// TextOnly reports that other differs from s only in description and labels.
// Equal states are not text-only, so a retry of the same desired state still
// has dataplane work.
func (s State) TextOnly(other State) bool {
	if reflect.DeepEqual(s, other) {
		return false
	}
	return s.SameWork(other)
}

func (s *State) clearText() {
	for id, v := range s.VPCs {
		v.Description = ""
		v.Labels = nil
		s.VPCs[id] = v
	}
	for id, sub := range s.Subnets {
		sub.Description = ""
		sub.Labels = nil
		s.Subnets[id] = sub
	}
	for id, g := range s.Groups {
		g.Description = ""
		g.Labels = nil
		s.Groups[id] = g
	}
	for id, e := range s.ElasticIPs {
		e.Labels = nil
		s.ElasticIPs[id] = e
	}
}
