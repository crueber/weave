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

## Project website (GitHub Pages)

The site lives in `site/` (static HTML/CSS and two tiny scripts, no external requests). `scripts/site-stage.sh`
copies it and `docs/demo.webm` (kept once, in `docs/`) into one directory; `make site` stages to `dist/site`,
`make site-preview` also serves it on http://localhost:8099/. The GitHub workflow `scripts/publish/pages.yml`
runs the same script, so the preview is what deploys. It is installed on the GitHub mirror by
`scripts/publish-to-github.sh` like the other `scripts/publish/*.yml` templates; it does not run on Forgejo.

Maintainer steps (once):

1. On GitHub: repository **Settings -> Pages -> Source: GitHub Actions**. Do this before the next publish,
   otherwise the first Pages run fails at `configure-pages`; if that happens, enable it and re-run the
   workflow (Actions -> pages -> Run workflow).
2. Publish from Forgejo as usual (Actions -> publish to github). The site goes live at
   https://crueber.github.io/weave/ after that publish: the squash commit touches `site/**`, which triggers
   the workflow. Later changes deploy whenever a publish changes `site/**` or `docs/demo.webm`.

The workflow also lists `release: published` and `workflow_dispatch`. A release created by another workflow
with the built-in `GITHUB_TOKEN` does not start new runs, so in practice the push trigger (the publish) and
the manual button are what deploy.

Screenshots: `scripts/site/seed.sh` seeds a throwaway instance with fictional data, `scripts/site/shots.mjs`
shoots it, `scripts/site/webp.py SHOTS site/img` converts (see each file's header). Read every image before
committing: no real names, hosts, paths or keys. `python3 scripts/site/check-contrast.py` checks the site
tokens (copied from `internal/web/static/weave.css`; keep them in sync by hand).
