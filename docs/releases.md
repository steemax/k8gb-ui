# Building and releasing

Releasing is explicit. CI runs on pushes and pull requests; the **Publish images**
and **Publish Helm chart** workflows run only through `workflow_dispatch` from
the repository's default branch. Neither `make images` nor `make chart` uploads
anything. The `release` GitHub environment can require maintainer approval.

## Versions and image names

This checkout targets application/chart version `0.1.1`. All three components
are built from the same source into one Docker Hub repository:

| Component | Image |
| --- | --- |
| Console | `steemax/k8gb-ui:0.1.1-console` |
| Site agent | `steemax/k8gb-ui:0.1.1-agent` |
| Optional Traefik webhook | `steemax/k8gb-ui:0.1.1-webhook` |

For an application update, change `Chart.yaml` appVersion, the three image tags
in `values.yaml`, standalone `deploy/` images, Makefile's default VERSION and
documentation examples. Bump the chart version for **every** chart change,
including a chart-only update that keeps the existing appVersion/images.
Never overwrite a published image tag or chart version. Enable immutable version
tags in Docker Hub; registry-side immutability also protects against concurrent
publishers. No `latest` tag is used.

## Publish 0.1.1

The [release notes](../CHANGELOG.md) describe bulk site controls and added
dry-run regression coverage. The default source branch is `main`, not `master`.
Version preparation does not publish any artifact or update running deployments.

1. Review and commit the changes, then push `main`. CI starts automatically.
2. After CI succeeds, open **Actions > Publish images > Run workflow**. Select
   `main` and version `0.1.1`. This workflow builds all three component images
   from the selected commit, even when only Console behavior changed.
3. Confirm that `0.1.1-console`, `0.1.1-agent` and `0.1.1-webhook` were published
   successfully. Keep `main` at the same commit during both publication workflows.
4. Run **Actions > Publish Helm chart > Run workflow** from `main`. There is no
   version input: it reads chart version/appVersion from `Chart.yaml`. Wait for
   the publish and Pages jobs to finish.
5. Run `helm repo update k8gb-ui` and `helm search repo k8gb-ui/k8gb-ui --versions`.
   Upgrade each installed release explicitly with `--version 0.1.1` and its
   existing private values and kube-context. Publishing does not deploy the chart.

Do not manually create the `k8gb-ui-0.1.1` chart release tag beforehand: the chart
workflow owns that tag. Do not merge `gh-pages` into `main` or overwrite `0.1.0`.

## Local checks and builds

Use Go 1.26.10+, Node.js 24+, Helm 4.2.3 and Docker with BuildKit:

```sh
go mod verify
go mod tidy -diff
go vet ./...
make test
make vulncheck
make images VERSION=0.1.1
make chart
```

The single Dockerfile selects its binary with `--build-arg COMPONENT=console`,
`agent` or `webhook`. The default is Console. The final images contain no shell,
build toolchain, credentials or working notes. Go cross-compilation supports
`linux/amd64` and `linux/arm64`; the release workflow publishes both architectures
with SBOM/provenance attestations. `--build-arg VERSION=0.1.1` sets the OCI version.
For example, build a multi-platform OCI archive locally without uploading it:

```sh
docker buildx create --name k8gb-ui-release --driver docker-container
docker buildx build --builder k8gb-ui-release --platform linux/amd64,linux/arm64 \
  --build-arg COMPONENT=console --build-arg VERSION=0.1.1 \
  -t steemax/k8gb-ui:0.1.1-console --output type=oci,dest=/tmp/k8gb-ui-console.oci .
docker buildx rm k8gb-ui-release
```

This uses a separate builder without changing the workstation's default. Docker's
classic image store cannot load a multi-platform manifest list; an OCI archive
or a registry push does not have that limitation. `make images` builds only the
local architecture and does not require this extra builder.

The chart package is `dist/charts/k8gb-ui-0.1.1.tgz`. Default chart values refuse
installation until authentication, cluster registry and namespaces are supplied;
validate with the example profiles, not an unauthenticated placeholder account.

## Docker Hub

Create the **public** Docker Hub repository `steemax/k8gb-ui`. In GitHub Actions
secrets (repository or `release` environment), configure `DOCKERHUB_TOKEN` with
write access to that repository. No Docker credentials belong in this source
tree or Helm values. The Docker Hub account is `steemax`; the GitHub owner may
be different.

Images retain project, Go, Lucide and downloaded dependency license/notice files
under `/licenses`; third-party licenses do not become Apache-2.0 just because
the project uses that license.

Run **Publish images** with the matching appVersion, such as `0.1.1`. It runs CI
first, then builds/pushes the three component tags. It refuses tags that already
exist; after a partial publication, inspect successful tags and publish only
the missing components manually from the **same verified commit**, or choose a
new application version. Do not replace successful artifacts. Confirm all
three tags and both architectures are public before publishing the chart.

## GitHub Pages Helm repository

The chart source lives in `charts/k8gb-ui`. The distribution branch `gh-pages`
contains `index.yaml` and chart `.tgz` packages, not application source or secrets.
The [official Helm chart releaser](https://helm.sh/docs/howto/chart_releaser_action/)
maintains the index and preserves older chart versions.

The published project repository is <https://steemax.github.io/k8gb-ui/>.
For deployment rather than publication, use the
[Kubernetes installation guide](installation.md).

GitHub may show a **"gh-pages had recent pushes" / "Compare & pull request"**
banner after chart publication. This is a branch-comparison suggestion, not a
failure or a required release step. Do not merge `gh-pages` into `main`: the
branches intentionally contain different artifacts. Update source documentation
on `main`; publish chart packages through the workflow.

One-time setup after creating the actual GitHub repository:

1. Create an empty `gh-pages` branch with an empty `.nojekyll` file. Keep this
   separate from the application source branch; do not copy source into it.
2. In **Settings > Pages**, select **GitHub Actions** as the publishing source.
3. Allow the chart workflow's `GITHUB_TOKEN` to write repository contents.
   Configure the optional `release` environment approval before using it.
4. Publish all component images first. Then run **Publish Helm chart**. It reruns
   CI, checks the default image tags with Docker Hub, creates the chart release
   and updates the packages/index on `gh-pages`. A separate job explicitly deploys
   that branch's content to Pages with `pages: write` / `id-token: write`. It does
   not depend on a recursive Pages build triggered by the bot's Git push.
   Only the chart-publish job has `contents: write`; there is no automatic
   chart publishing on push.

The workflow refuses an existing chart release tag. If only the final Pages
deployment failed after a successful chart publication, re-run the failed Pages
job rather than publishing the same version again. Inspect partial release/index
failures before retrying; never replace a published package with changed content.

After the source is committed and its `origin` points to the intended GitHub
repository, this one-time bootstrap creates a **separate disposable worktree**;
the source branch and its files are not switched or removed:

```sh
pages_dir="$(mktemp -d)"
git worktree add --detach "$pages_dir" HEAD
git -C "$pages_dir" switch --orphan gh-pages
touch "$pages_dir/.nojekyll"
git -C "$pages_dir" add .nojekyll
git -C "$pages_dir" commit -m "Initialize Helm chart repository"
git -C "$pages_dir" push origin gh-pages
git worktree remove "$pages_dir"
```

Run this only once, if `gh-pages` does not already exist. Protect the
`github-pages` environment so only the source default branch can deploy.

GitHub supplies the actual owner/repository through `GITHUB_REPOSITORY`; the
workflow also works in forks. After Pages finishes deploying, verify this
project's repository below (substitute your own URL when publishing a fork):

```sh
helm repo add k8gb-ui https://steemax.github.io/k8gb-ui/
helm repo update
helm search repo k8gb-ui/k8gb-ui --versions
helm upgrade --install ui k8gb-ui/k8gb-ui --version 0.1.1 \
  -n k8gb-ui --create-namespace -f /absolute/private/console-values.yaml
```

## Public source checklist

- Confirm permission to redistribute the code/assets under the chosen
  [Apache-2.0 license](../LICENSE). Preserve dependency copyright/license notices.
- Review the exact staged files and full demo before the first upload. `.gitignore`
  excludes credentials, local reports, editor settings and private working notes;
  it cannot remove files already tracked in Git or leaked in Git history.
- Enable GitHub private vulnerability reporting and check [SECURITY.md](../SECURITY.md).
- Run CI on the exact release commit and review the compatibility limits. The
  local kind/k8gb v1.0.0 tests are not certification for every installation.
- Scan the final images and review their SBOM/dependency licenses. Keep release
  evidence outside the source tree in ignored `artifacts/`.
