# Preparing a public release

Before changing visibility or tagging a release:

1. Review the full Git history and the exact files to publish, including
   attachments, commit messages, and author/committer email addresses. Use
   `gitleaks git --redact --log-opts=--all` and manually inspect additions.
2. Run the offline test suite and confirm CI is green for the intended commit.
   Live harness compatibility needs its own controlled trials; an offline pass
   does not establish it.
3. Confirm the MIT license covers the material being published and preserve
   required third-party notices if dependencies or contributed code are added.
4. Review README installation instructions, examples, supported platforms,
   known limitations, and the private vulnerability reporting route.
5. Confirm that no local trials, generated reports, cloud identity output,
   workstation configuration, real agent captures, or transfer bundles are
   included. Review release archives and attachments as well as tracked files.
6. Have the repository owner explicitly authorize the visibility change or
   release. Repository preparation does not publish it automatically.

AJX v1 is the first public version. Keep the package version, CLI version output,
documentation, and any release tag consistent. Review public claims against the
shipped behavior and evidence available to readers. Report projections as modeled
and keep unsupported performance, adoption, and compatibility claims out of the
release material.

## Publish a CLI version

Set the Go version in `embed.go` and the Python version in
`skills/ajx/lib/ajx/__init__.py` to the same version. After the intended commit
is on `main` and its checks pass, create and push an annotated `vX.Y.Z` tag at
that commit. Use your GitHub no-reply identity for the tag and commits.

The tag starts the full CI workflow, including Python, Go, container and privacy
checks. It then tests, builds and installs packages on all four native platforms.
The publishing job checks that the tag belongs to `main` and that every package
matches its version and commit. It uploads the complete asset set to a draft
release before making the release public. Published assets are not overwritten;
a fix needs a new version.

Stable tags update the latest release used by the anonymous one-line installer.
A tag with a prerelease suffix creates a prerelease and does not replace the
latest stable version. `main` pushes still produce development CI artifacts,
but the installer uses versioned releases only.

Before calling the release complete, test the public one-liner without GitHub
credentials in a disposable home directory, then verify `ajx version`, skill
discovery, and the simulated console/headless trial.
