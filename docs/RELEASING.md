# Releasing OFA

The short version: bump the npm package version, commit it, tag that commit,
then approve the staged npm publish and bump the Homebrew tap by hand.

## 1. Bump and tag

```sh
# 1. Bump the npm package version. CI restamps the Go binary from the tag,
#    so do not edit internal/cli.version by hand.
$EDITOR npm/package.json          # "version": "X.Y.Z"

git commit -am "Bump npm package version to X.Y.Z"
git tag vX.Y.Z                    # must be on the bump commit
git push origin main
git push origin vX.Y.Z
```

The tag **must** point at the version-bump commit. The release workflow checks
the stamping and fails if the tag and `npm/package.json` disagree.

## 2. What the workflow does

Pushing a `v*.*.*` tag triggers `.github/workflows/release.yml`, which:

1. Verifies the stamping in a real build before anything ships.
2. Cross-compiles four binaries: `darwin-arm64`, `darwin-amd64`, `linux-amd64`,
   `linux-arm64` (in CI, not on your machine).
3. Creates the GitHub release with notes built from the commit headlines since
   the previous tag, plus `checksums.txt`.
4. **Stages** the npm publish — it does not publish.

Pushes to `main` and PRs only run `go build`, `go vet`, and `go test -race ./...`.
Tagging is the only thing that ships.

If the npm step fails (permissions, registry hiccup), do **not** cut a new tag.
Re-run the workflow against the existing tag:

```sh
gh workflow run release.yml -f tag=vX.Y.Z
```

The workflow is idempotent: it refreshes the release assets instead of failing.

## 3. Approve the npm publish (needs the owner's 2FA)

CI uses a trusted publisher whose permission is limited to `npm stage publish`.
A human must approve the staged version:

<https://www.npmjs.com/package/@khaentertainment/ofa/staged>

Direct publishing from CI is not possible by design, so a release is not live on
npm until this step happens. Check with:

```sh
npm view @khaentertainment/ofa version
```

## 4. Bump the Homebrew tap (manual, no automation)

In `KHAEntertainment/homebrew-tap`, edit `Formula/ofa.rb`:

- `url` → `https://github.com/KHAEntertainment/ofa/archive/refs/tags/vX.Y.Z.tar.gz`
- `sha256` → checksum of that tarball:

```sh
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
curl -fsSL --retry 3 -o "$tmp" \
  https://github.com/KHAEntertainment/ofa/archive/refs/tags/vX.Y.Z.tar.gz
shasum -a 256 "$tmp"
```

`-f` matters: without it `curl` writes an HTTP error body to the file and the
checksum is of that body, which would publish a formula that can never install.

Then commit and push the tap. Verify with `brew update && brew info ofa`.

## 5. Verify the whole chain

```sh
gh release view vX.Y.Z                 # assets + checksums present
npm view @khaentertainment/ofa version # matches the tag
brew info KHAEntertainment/tap/ofa     # formula version + url match
```

## Review capacity fallback

CodeRabbit's free OSS review quota can run out mid-month. When it does, a
review substitute that has caught real blockers here is a Pi child on
`inception/mercury-3` running the `traycer-review` skill (it found the silent
`ofa --refresh` no-op on PR #9). Do not merge a non-trivial PR unreviewed just
because CodeRabbit is unavailable.

## Labels

Current triage labels: `priority:low|medium` plus `scope:cli|scan|update`, alongside
the GitHub defaults (`bug`, `enhancement`, `documentation`, …). There is no
`priority:high` yet — add it if the need appears.
