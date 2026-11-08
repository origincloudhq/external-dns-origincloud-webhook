package provider

import (
	"net/netip"
	"slices"
	"strings"
	"time"

	dnsv1beta1 "go.origincloud.eu/sdk/dns/v1beta1"
	"google.golang.org/protobuf/types/known/durationpb"
	"sigs.k8s.io/external-dns/endpoint"
)

// The TTLs a record set takes, in seconds.
const (
	minTTL = 30
	maxTTL = 7 * 24 * 60 * 60
)

// recordTypes maps external-dns's record types to the API's: the types
// both have. The API's CAA record sets aren't among them, as external-dns
// has no CAA type.
var recordTypes = map[string]dnsv1beta1.RecordSet_Type{
	endpoint.RecordTypeA:     dnsv1beta1.RecordSet_A,
	endpoint.RecordTypeAAAA:  dnsv1beta1.RecordSet_AAAA,
	endpoint.RecordTypeCNAME: dnsv1beta1.RecordSet_CNAME,
	endpoint.RecordTypeTXT:   dnsv1beta1.RecordSet_TXT,
	endpoint.RecordTypeMX:    dnsv1beta1.RecordSet_MX,
	endpoint.RecordTypeSRV:   dnsv1beta1.RecordSet_SRV,
}

// recordKey identifies a record set within a zone.
type recordKey struct {
	dnsName    string
	recordType string
}

func (k recordKey) String() string { return k.dnsName + " " + k.recordType }

// endpointOf maps a record set to the endpoint external-dns sees: every
// value of the record set is one of its targets. An alias, whose address
// the platform follows, and a type external-dns doesn't know, aren't
// mapped: false.
func endpointOf(rs *dnsv1beta1.RecordSet) (*endpoint.Endpoint, bool) {
	basic := rs.GetBasic()
	if basic == nil {
		return nil, false
	}
	recordType := rs.GetType().String()
	if _, ok := recordTypes[recordType]; !ok {
		return nil, false
	}
	ep := &endpoint.Endpoint{
		DNSName:    rs.GetDnsName(),
		RecordType: recordType,
		Targets:    slices.Clone(basic.GetValues()),
	}
	if ttl := rs.GetTtl(); ttl != nil {
		ep.RecordTTL = endpoint.TTL(ttl.GetSeconds())
	}
	return ep, true
}

// recordSet is what a record set is to hold: its name, type, TTL and
// values, merged from every endpoint of that name and type.
type recordSet struct {
	key     recordKey
	ttl     endpoint.TTL
	targets []string
}

// merge adds an endpoint's targets, each once, and takes its TTL if none
// is set yet.
func (s *recordSet) merge(ep *endpoint.Endpoint) {
	for _, t := range normalizeTargets(ep.RecordType, ep.Targets) {
		if !slices.Contains(s.targets, t) {
			s.targets = append(s.targets, t)
		}
	}
	if !s.ttl.IsConfigured() {
		s.ttl = ep.RecordTTL
	}
}

// apiRecordSet maps the record set to the API's, for a create; name is
// set for an update.
func (s *recordSet) apiRecordSet(name string) *dnsv1beta1.RecordSet {
	rs := &dnsv1beta1.RecordSet{
		Name:    name,
		DnsName: s.key.dnsName,
		Type:    recordTypes[s.key.recordType],
		Data: &dnsv1beta1.RecordSet_Basic{
			Basic: &dnsv1beta1.BasicRecordData{Values: slices.Clone(s.targets)},
		},
	}
	if s.ttl.IsConfigured() {
		rs.Ttl = durationpb.New(time.Duration(clampTTL(s.ttl)) * time.Second)
	}
	return rs
}

func clampTTL(ttl endpoint.TTL) endpoint.TTL {
	return min(max(ttl, minTTL), maxTTL)
}

// normalizeName writes a DNS name as the API answers it: lowercase,
// without the final dot.
func normalizeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// normalizeTargets writes each target as the API answers it, each once, so
// that what external-dns asks for compares equal to what it reads back.
func normalizeTargets(recordType string, targets []string) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if t = normalizeTarget(recordType, t); !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// normalizeTarget writes one target as the API takes and answers it.
func normalizeTarget(recordType, target string) string {
	switch recordType {
	case endpoint.RecordTypeA, endpoint.RecordTypeAAAA:
		target = strings.TrimSpace(target)
		if addr, err := netip.ParseAddr(target); err == nil {
			return addr.String()
		}
		return target
	case endpoint.RecordTypeCNAME:
		return normalizeName(target)
	case endpoint.RecordTypeMX:
		// {preference} {exchange}
		return normalizeLastName(target, 2)
	case endpoint.RecordTypeSRV:
		// {priority} {weight} {port} {target}
		return normalizeLastName(target, 4)
	case endpoint.RecordTypeTXT:
		return txtText(target)
	}
	return target
}

// normalizeLastName rewrites a value of n fields whose last is a name,
// such as an MX or SRV value, with single spaces and the name normalized.
// A value of another shape is left for the API to refuse.
func normalizeLastName(value string, n int) string {
	fields := strings.Fields(value)
	if len(fields) != n {
		return strings.TrimSpace(value)
	}
	if name := fields[n-1]; name != "." {
		fields[n-1] = normalizeName(name)
	}
	return strings.Join(fields, " ")
}

// txtText returns the text a TXT target stands for. external-dns writes the
// TXT registry's ownership records, and many users their own TXT values, in
// DNS presentation format: one or more quoted strings. The API takes the
// text itself, which it splits into strings of 255 characters on the wire,
// so the strings are unquoted and joined. A target that isn't a sequence of
// quoted strings is text as it is.
func txtText(target string) string {
	if text, ok := unquote(target); ok {
		return text
	}
	return target
}

// unquote parses a sequence of quoted strings, separated by blanks, with
// `\"` and `\\` escapes, and returns them joined. false when value isn't
// such a sequence.
func unquote(value string) (string, bool) {
	s := strings.TrimSpace(value)
	if !strings.HasPrefix(s, `"`) {
		return "", false
	}
	var b strings.Builder
	for s != "" {
		if s[0] != '"' {
			return "", false
		}
		s = s[1:]
		closed := false
		for len(s) > 0 && !closed {
			switch c := s[0]; {
			case c == '\\' && len(s) > 1:
				b.WriteByte(s[1])
				s = s[2:]
			case c == '"':
				closed = true
				s = s[1:]
			default:
				b.WriteByte(c)
				s = s[1:]
			}
		}
		if !closed {
			return "", false
		}
		next := strings.TrimLeft(s, " \t")
		if next != "" && len(next) == len(s) {
			// Something other than a blank follows a closing quote.
			return "", false
		}
		s = next
	}
	return b.String(), true
}
