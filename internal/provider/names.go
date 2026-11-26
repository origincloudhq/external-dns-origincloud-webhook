package provider

// dnsService is the DNS API's service name, the host part of its resources'
// full names.
const dnsService = "//dns.origincloudapis.eu/"

// fullName turns a record set's name, which the API answers relative
// (projects/…/hostedZones/…/recordSets/…), into the full name a reference
// to it must carry, such as a batch change's deletions. The API refuses a
// relative name there.
func fullName(relative string) string {
	return dnsService + relative
}
