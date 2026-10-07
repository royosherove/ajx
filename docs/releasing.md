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

The GitHub repository starts from a cleaned source snapshot with fresh history.
Historical design and review documents describe development before that
snapshot; their findings are retained for context, not as release certification.
