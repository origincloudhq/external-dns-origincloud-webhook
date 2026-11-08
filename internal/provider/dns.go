package provider

import (
	"context"

	"go.origincloud.eu/sdk/core"
	dnsv1beta1 "go.origincloud.eu/sdk/dns/v1beta1"
)

// DNS is what the provider asks of the Origin Cloud DNS API.
type DNS interface {
	// ListHostedZones returns every hosted zone of project,
	// projects/{project}, oldest first.
	ListHostedZones(ctx context.Context, project string) ([]*dnsv1beta1.HostedZone, error)
	// ListRecordSets returns every record set of zone,
	// projects/{project}/hostedZones/{zone}.
	ListRecordSets(ctx context.Context, zone string) ([]*dnsv1beta1.RecordSet, error)
	// BatchChangeRecordSets makes the changes of one zone, all or none.
	BatchChangeRecordSets(ctx context.Context, req *dnsv1beta1.BatchChangeRecordSetsRequest) error
}

// NewDNS returns the DNS API over the Go SDK's client.
func NewDNS(client *core.Client) DNS {
	return &sdkDNS{
		zones:   dnsv1beta1.NewHostedZoneClient(client),
		records: dnsv1beta1.NewRecordSetClient(client),
	}
}

type sdkDNS struct {
	zones   *dnsv1beta1.HostedZoneClient
	records *dnsv1beta1.RecordSetClient
}

func (d *sdkDNS) ListHostedZones(ctx context.Context, project string) ([]*dnsv1beta1.HostedZone, error) {
	var zones []*dnsv1beta1.HostedZone
	for zone, err := range d.zones.AllHostedZones(ctx, &dnsv1beta1.ListHostedZonesRequest{Parent: project}) {
		if err != nil {
			return nil, err
		}
		zones = append(zones, zone)
	}
	return zones, nil
}

func (d *sdkDNS) ListRecordSets(ctx context.Context, zone string) ([]*dnsv1beta1.RecordSet, error) {
	var records []*dnsv1beta1.RecordSet
	for rs, err := range d.records.AllRecordSets(ctx, &dnsv1beta1.ListRecordSetsRequest{Parent: zone}) {
		if err != nil {
			return nil, err
		}
		records = append(records, rs)
	}
	return records, nil
}

func (d *sdkDNS) BatchChangeRecordSets(ctx context.Context, req *dnsv1beta1.BatchChangeRecordSetsRequest) error {
	_, err := d.records.BatchChangeRecordSets(ctx, req)
	return err
}
