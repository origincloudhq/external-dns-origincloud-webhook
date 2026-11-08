# Contributing

Thank you for helping improve Origin Cloud's webhook provider for
external-dns.

## Issues

Open an issue for anything that is wrong, unclear or missing: a record that
isn't created or deleted as you expect, a sync that changes something every
time, a setting you need. Include the webhook's and external-dns's logs where
you can, with `LOG_LEVEL=debug` on the webhook, and the resource's
annotations, with tokens and account details removed.

To report a security vulnerability, don't open an issue: use **Report a
vulnerability** on this repository's Security tab, which reaches us privately.

## Pull requests

Pull requests are reviewed and merged here. For anything larger than a small
fix, open an issue first, so that we can agree on the change before you spend
time on it. CI must pass: the checks below, run on every pull request.

## How the webhook is built

- `internal/webhook` serves external-dns's webhook API and the health
  endpoints. It speaks external-dns's own types, from
  `sigs.k8s.io/external-dns`'s `endpoint` and `plan` packages, so a change to
  the protocol arrives with a new version of that module.
- `internal/provider` maps between those types and the DNS API's record sets,
  and makes a plan's changes with one batch change per zone. `DNS` is what it
  asks of the API; `NewDNS` implements it over the
  [Go SDK](https://github.com/origincloudhq/sdk-go).
- `internal/fakedns` is an in-memory DNS API the tests run against. It
  refuses a batch change as the API does: a record set named twice, two
  record sets of one name and type, a CNAME that isn't alone.
- `internal/config` reads the environment.

## Checks

CI (`.github/workflows/ci.yml`) runs, and you can run locally:

```sh
gofmt -l .
go mod tidy -diff
go vet ./...
golangci-lint run          # v2, as .golangci.yml configures it
go test -race ./...
docker build .
helm template external-dns external-dns \
  --repo https://kubernetes-sigs.github.io/external-dns/ --version 1.23.0 \
  --values examples/helm/values.yaml
```

`TestOwnersShareAZone` runs two external-dns instances, each with
external-dns's own webhook client and TXT registry, against webhooks sharing
one fake zone: it is the check that clusters sharing a zone keep to their own
records. `TestLiveOwnersShareAZone` runs the same against the live API, below
a name of its own in a zone you name, and deletes what it created; it skips
unless both are set:

```sh
EXTERNAL_DNS_LIVE_PROJECT=my-project-123 EXTERNAL_DNS_LIVE_DOMAIN=example.com \
  go test -run TestLive ./internal/webhook/
```

It takes the credentials the SDK finds, such as `ORIGINCLOUD_CLIENT_ID` and
`ORIGINCLOUD_CLIENT_SECRET` of a service account with `roles/dns/editor` on
the project. The zone may be pending verification.
