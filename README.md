# external-dns webhook provider for Origin Cloud DNS

Lets [external-dns](https://github.com/kubernetes-sigs/external-dns) keep the
records of Kubernetes Services, Ingresses and the other sources it reads in
Origin Cloud DNS hosted zones. It is a
[webhook provider](https://kubernetes-sigs.github.io/external-dns/latest/docs/tutorials/webhook-provider/):
a small HTTP server that runs next to external-dns, usually as a sidecar in its
pod, and makes external-dns's changes through the DNS API, over the
[Go SDK](https://github.com/origincloudhq/sdk-go).

```text
ghcr.io/origincloudhq/external-dns-origincloud-webhook
```

## Using it

1. **A service account.** Create a `WORKLOAD` service account, and grant it
   `roles/dns.editor` on the project that holds the zones: it manages record
   sets and reads zones, but can't create or delete a zone.
2. **A federated credential** on it that trusts the cluster's service account
   tokens for external-dns's Kubernetes service account: the cluster's issuer,
   and the condition `sub=system:serviceaccount:<namespace>:external-dns`. The
   webhook exchanges a projected token for the service account's access
   tokens at the security token service, so no secret is stored in the
   cluster. The issuer must serve its OIDC discovery document and keys over
   HTTPS at a public address.
3. **external-dns**, from its own Helm chart, with the webhook as its
   provider. [`examples/helm/values.yaml`](examples/helm/values.yaml) is a
   complete example; the part that matters:

   ```yaml
   txtOwnerId: my-cluster
   policy: sync
   domainFilters: [example.com]
   provider:
     name: webhook
     webhook:
       image:
         repository: ghcr.io/origincloudhq/external-dns-origincloud-webhook
         tag: "0.2.1"
       env:
         - name: ORIGINCLOUD_PROJECT
           value: my-project-123
         - name: DOMAIN_FILTER
           value: example.com
         - name: ORIGINCLOUD_OIDC_TOKEN_FILE
           value: /var/run/secrets/origincloud/token
       extraVolumeMounts:
         - name: origincloud-token
           mountPath: /var/run/secrets/origincloud
           readOnly: true
       readinessProbe:
         httpGet: {path: /readyz, port: http-webhook}
   extraVolumes:
     - name: origincloud-token
       projected:
         sources:
           - serviceAccountToken:
               audience: https://sts.origincloudapis.eu
               expirationSeconds: 3600
               path: token
   ```

   ```sh
   helm upgrade --install external-dns external-dns \
     --repo https://kubernetes-sigs.github.io/external-dns/ \
     --values values.yaml
   ```

A Service of type `LoadBalancer` annotated with
`external-dns.kubernetes.io/hostname: app.example.com` then gets an `A` record
set `app.example.com` holding its load balancer's address, and loses it when
the Service is deleted.

## Configuration

The webhook is configured from the environment.

| Variable | Default | What it sets |
|---|---|---|
| `ORIGINCLOUD_PROJECT` | Required | The project whose hosted zones are managed: its id, or `projects/{project}`. |
| `ORIGINCLOUD_OIDC_TOKEN_FILE` | | The file holding the cluster's projected service account token, issued for the audience `https://sts.origincloudapis.eu`. It is read again at every exchange, so the kubelet's renewals are picked up. |
| `ORIGINCLOUD_CREDENTIALS_FILE` | | A service account key's credentials file, instead of a projected token, for a cluster whose issuer the security token service can't reach. |
| `ORIGINCLOUD_ACCESS_TOKEN` | | An access token, sent as it is. It isn't renewed: for trying the webhook out. |
| `ORIGINCLOUD_ZONE_IDS` | Every zone | Comma-separated zone ids, or zones' resource names: only these zones are managed. |
| `ORIGINCLOUD_ZONE_VISIBILITY` | Both | `public` or `private`: only zones of that visibility are managed. |
| `DOMAIN_FILTER` | Every domain | Comma-separated domains: only these domains and the names below them are managed, as external-dns's `--domain-filter` takes them. |
| `EXCLUDE_DOMAIN_FILTER` | | Comma-separated domains below those that aren't managed, as `--exclude-domains` takes them. |
| `REGEXP_DOMAIN_FILTER`, `REGEXP_DOMAIN_FILTER_EXCLUSION` | | Regular expressions for the names managed, and those that aren't, instead of `DOMAIN_FILTER` and `EXCLUDE_DOMAIN_FILTER`. |
| `DRY_RUN` | `false` | `true` logs the changes instead of making them. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `LOG_FORMAT` | `text` | `text` or `json`. |
| `WEBHOOK_ADDRESS` | `localhost:8888` | Where the webhook API listens. external-dns calls `http://localhost:8888` by default. |
| `HEALTH_ADDRESS` | `:8080` | Where `/healthz` and `/readyz` listen. |

The zone id filter, the visibility and the domain filter all apply: a zone is
managed when each selects it. The domain filter selects a zone it matches, or
one that holds a domain it matches, so `DOMAIN_FILTER=k8s.example.com`
manages the names below `k8s.example.com` in the zone `example.com`, and no
other name of that zone.

## Behaviour

- **Zones.** A zone is managed whatever its state: the record sets of a zone
  that is pending verification, or suspended, are kept, and answered once the
  zone is active. When the project holds several zones of one name, an active
  one is taken over the others, and of the rest the oldest. A public and a
  private zone of one name, both active, are refused until
  `ORIGINCLOUD_ZONE_VISIBILITY` or `ORIGINCLOUD_ZONE_IDS` chooses one.
- **Records.** A record set is one endpoint to external-dns, each of its
  values one of the endpoint's targets: the `A` record set of a name holds
  every address external-dns asks for at that name. A name belongs to the
  zone with the longest name that holds it, so a zone `dev.example.com` takes
  the names below it from `example.com`.
- **Record types.** `A`, `AAAA`, `CNAME`, `TXT`, `MX` and `SRV`. external-dns
  manages `A`, `AAAA` and `CNAME` unless its `--managed-record-types` says
  otherwise. An endpoint of another type, or one with a set identifier, as
  routing policies need, is left out with a warning: Origin Cloud DNS has
  neither. A zone's `CAA` record sets, a type external-dns has no name for,
  are left as they are.
- **TXT values.** external-dns writes TXT values in DNS presentation format,
  in quotes; the API takes the text itself. A value of one or more quoted
  strings is stored unquoted, the strings joined, and read back that way:
  the API splits text longer than 255 characters into strings on the wire
  again, up to 4,000 characters. A value without quotes is stored as it is.
- **Ownership.** external-dns's TXT registry keeps an ownership record next to
  each record it creates, named after it, such as `a-app.example.com` for
  `app.example.com`'s `A` record set. Several clusters can share a zone, each
  with its own `txtOwnerId`: none changes or deletes another's records, and a
  name one cluster holds isn't taken by another until it lets go of it.
- **TTL.** A TTL external-dns sets, with the
  `external-dns.kubernetes.io/ttl` annotation, is kept within the 30 seconds
  to 7 days a record set takes. Without one, a record set gets the API's
  default, 300 seconds.
- **Changes.** The changes of one external-dns sync are made with one batch
  change per zone, all or none within a zone; more than 1,000 changes in a
  zone take several batches, a name's changes always in one. A zone whose
  batch fails doesn't stop the others. A record set both deleted and created
  in one sync is updated in place, so a CNAME can take an `A` record set's
  place. A record set external-dns creates that exists already, unknown to
  it, is left as it is with a warning.
- **Aliases.** A record set that is an alias to a Floating IP is never read,
  changed or deleted: its address is the platform's to follow.
- **Errors.** A call the API refuses, or that fails, is answered with a 500,
  which external-dns retries, logs, and tries again at its next sync. Only a
  request the webhook can't decode is answered with a 4xx, which stops
  external-dns.
- **Credentials.** The Go SDK reads them, in this order:
  `ORIGINCLOUD_ACCESS_TOKEN`, `ORIGINCLOUD_CREDENTIALS_FILE`, then
  `ORIGINCLOUD_OIDC_TOKEN_FILE`. Releases before 0.2.0 took a client ID and
  secret, which service accounts no longer have.
- **Health.** `/healthz` answers while the process serves. `/readyz` answers
  once the zones have been listed, which proves the credentials and the
  project; until then, the webhook retries and logs why it can't. A token the
  security token service refuses is answered `PERMISSION_DENIED` without a
  reason: check the federated credential's issuer and `sub`, and the
  projected token's audience.
- **Shutdown.** On `SIGTERM` the webhook stops taking calls and lets the
  calls under way finish, for up to 20 seconds.
- **Timeouts.** external-dns gives a call to the webhook 15 seconds by
  default (`--webhook-provider-read-timeout` and
  `--webhook-provider-write-timeout` together). A sync that changes many
  zones takes a batch change per zone; raise them through the chart's
  `extraArgs` if its calls time out.

## Running it locally

```sh
export ORIGINCLOUD_PROJECT=my-project-123
export ORIGINCLOUD_ACCESS_TOKEN="$(origin id token --raw)"
go run . &

curl -s -H 'Accept: application/external.dns.webhook+json;version=1' localhost:8888/
curl -s localhost:8888/records
curl -s -X POST localhost:8888/records \
  -H 'Content-Type: application/external.dns.webhook+json;version=1' \
  -d '{"create":[{"dnsName":"test.example.com","recordType":"A","targets":["192.0.2.1"],"recordTTL":60}]}'
```

`DRY_RUN=true` logs what a plan would change without changing it.

## Contributing

Issues and pull requests are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md)
for how the webhook is built and checked, and [RELEASING.md](RELEASING.md) for
how it is released.

## License

Apache License 2.0; see [LICENSE](LICENSE).
