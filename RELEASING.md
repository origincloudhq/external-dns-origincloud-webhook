# Releasing

A `v*` tag runs `.github/workflows/release.yml`: it runs the tests, builds the
image for `linux/amd64` and `linux/arm64`, pushes it to
`ghcr.io/origincloudhq/external-dns-origincloud-webhook` with a provenance
attestation and an SBOM, and creates the GitHub release, which names the
image's digest. The image is tagged with the version without its `v`
(`0.2.0`), its minor version (`0.2`), and `latest` unless the version is a
prerelease (`v0.2.0-rc.1`). A version is never moved or deleted once
released.

Before v1, a minor version may change the configuration incompatibly, as the
preview DNS API itself may.

## A release

Tag a commit on `main` whose CI passed:

```sh
git tag v0.2.0 origin/main
git push origin v0.2.0
```

Then update the image tag in `examples/helm/values.yaml` and `README.md`.

## The package

The workflow pushes with its own token (`packages: write`); no secret is
needed. The first push creates the package `external-dns-origincloud-webhook`
in origincloudhq, linked to this repository by the image's
`org.opencontainers.image.source` label. A new package is private: make it
public once, in the package's settings (Package settings → Danger Zone →
Change visibility), so that clusters pull it without credentials.
