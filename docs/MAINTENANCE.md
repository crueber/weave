# Maintenance

## Pinned base images

Every base image is pinned by digest, so a build is reproducible and a compromised or re-tagged
upstream image cannot slip in silently. The tag stays in a comment next to the digest.

| Where | Image |
|---|---|
| `Dockerfile` (build stage) | `golang:1.27` |
| `Dockerfile` (runtime) | `gcr.io/distroless/static-debian13:nonroot` |
| `.forgejo/workflows/ci.yml` (`test`, `release`) | `golang:1.27` |
| `.forgejo/workflows/ci.yml` (`package`) | `quay.io/buildah/stable:latest` |
| `runner/docker-compose.yml` | `code.forgejo.org/forgejo/runner:6`, job label `node:20-bookworm` |

The digest is the multi-architecture index digest, so amd64 and arm64 builders resolve it alike.

### Bumping the pins

```sh
scripts/update-pins.sh           # prints tag@digest for every pinned image (registry metadata only)
scripts/update-pins.sh --check   # exit 1 and list STALE entries when the repo differs from the registry
```

1. Run `scripts/update-pins.sh` and replace the old `@sha256:...` in the files above with the printed values.
2. `docker build -t weave:pin-test .` and smoke test: the image must run as `nonroot`, report
   `healthy` (HEALTHCHECK runs `weave healthcheck`) and work with the hardening in `docker-compose.yml`
   (`read_only: true`, `cap_drop: ALL`, `/tmp` tmpfs).
3. Commit "Bump pinned images". Do this monthly or when a base-image CVE is announced; `--check` is cheap
   enough to run in a scheduled job.

To change a tag (for example `golang:1.28`) edit the `IMAGES` list in `scripts/update-pins.sh` too, so
`--check` keeps watching the right thing. The script needs `docker buildx`, `crane`, or `skopeo`.

## Release signing

`SHA256SUMS.txt` of every release is signed with ed25519 (`SHA256SUMS.txt.sig`, base64 of the raw 64-byte
signature). Only the Go standard library is used (`internal/relsign`), so the CI `golang` image needs no
extra tools.

One-time setup:

1. `go run ./cmd/weave-release -genkey -keyfile /safe/place/weave-release.key` writes the private seed (mode
   0600, refuses to overwrite) and prints the public key. The seed is never printed.
2. Put the public key in `internal/relsign/relsign.go` (`PublicKey`) and its SPKI form (also printed) in
   `scripts/install.sh` and `scripts/install.ps1`. `go test ./internal/relsign` fails when `install.sh` and
   the Go constant disagree.
3. Add the seed as the Forgejo Actions secret **`RELEASE_SIGNING_KEY`** (repository settings, Actions, Secrets).
   The `release` job signs when it exists and prints a warning (the release is published unsigned) when not.
   It then verifies the signature with the freshly built CLI before publishing.

Rotation: generate a new pair, replace the three embedded copies, update the secret, and publish a release.
Installers from older releases embed the old key and keep verifying older releases. Keep the seed offline or in
a secrets manager; anyone with it can publish releases that installers accept.

Verifying by hand: `weave verify-release SHA256SUMS.txt SHA256SUMS.txt.sig`, or with OpenSSL >= 3 (the
installer does this): decode the signature (`openssl base64 -d -A`) and run
`openssl pkeyutl -verify -rawin -pubin -inkey release-pub.pem -sigfile sig.bin -in SHA256SUMS.txt`.

Forks that sign with their own key pass `WEAVE_RELEASE_PUBKEY=/path/pub.pem` to `install.sh` /
`install.ps1`, and `--pubkey` to `weave verify-release`.
