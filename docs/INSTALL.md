# Install AJX

AJX installs as a native CLI and a matching Agent Skills folder. You need macOS
or Linux on ARM64 or x86-64, Bash, `curl`, and standard shell tools.
No GitHub account, sign-in, Go compiler, Python, Git checkout, administrator
access, or model key is needed to install or try the simulated demo.

## One command

Run in Bash or Zsh:

```sh
bash -o pipefail -c 'curl -fsSL https://github.com/royosherove/ajx/releases/latest/download/install.sh | bash' && export PATH="$HOME/.local/bin:$PATH"
```

Then try:

```sh
ajx version
ajx console demo trials/console-demo
```

Start a fresh agent session to use the skill. Ask: “Use the AJX skill to review
my project,” or follow the [first brief](../README.md#2-give-your-agent-this-first-brief).
Actual trials also need an installed and authenticated worker and reporter CLI.
The installer does not install those CLIs or change their credentials.

## What gets installed

| Location | Purpose |
|---|---|
| `~/.local/bin/ajx` | Native CLI |
| `~/.local/share/ajx/versions/` | Versioned CLI, complete skill, licenses, and build metadata |
| `~/.local/share/ajx/current` | Selected installed version |
| `~/.agents/skills/ajx` | Codex skill |
| `~/.claude/skills/ajx` | Claude Code skill |
| `~/.kiro/skills/ajx` | Kiro skill |

The CLI and skill paths point to the same installed version. Each skill also
contains its own native `bin/ajx`, so an agent can invoke it without relying on
the terminal's `PATH`. Kiro custom agents need the
[resource entry](HARNESS-SUPPORT.md#install-and-invoke) in their configuration.

The installer adds an idempotent `PATH` line to `.zshrc` for Zsh, `.bashrc` and
`.bash_profile` for Bash, or `.profile` for a POSIX shell. Other shells require
their own PATH setup. The one-liner also updates the current Bash/Zsh session.
An already open desktop agent may need a new session, but its installed skill
can invoke the bundled binary directly.

## Updates, options, and existing installations

Rerun the one-liner to install the latest stable release. The installer resolves
that version once, selects its package for your platform, checks its SHA-256
checksum and source commit, and verifies the executable before changing the
installation. It never selects an unfinished build or a prerelease by default.
Release publication waits until all four native packages have passed CI.

To inspect the script or pass options, download it first:

```sh
curl -fsSL https://github.com/royosherove/ajx/releases/latest/download/install.sh \
  -o ajx-install.sh
less ajx-install.sh
bash ajx-install.sh --help
```

| Option | Use |
|---|---|
| `--skill-dir "$HOME/.agents/skills/ajx"` | Install only the Codex skill; repeat for more hosts or use another host's documented directory |
| `--no-skills` | Install only the CLI |
| `--no-path` | Leave shell startup files untouched; invoke `~/.local/bin/ajx` directly |
| `--version v1.0.0` | Pin to a specific released version instead of selecting the newest |
| `--replace` | Move conflicting existing files/folders to adjacent `.ajx-backup-*` paths before installing |
| `--package-dir DIR` | Install from a trusted, already downloaded package; no network needed |

Custom skill destinations must be absolute paths ending in `/ajx`, outside
the CLI's installation directory.

Existing manual skills and binaries are not overwritten by default. Inspect
them and use `--replace` when you want to switch; backups preserve those files.
If the one-liner stops with `Already exists`, it prints a complete replacement
command you can copy. For the default installation, that command is:

```sh
bash -o pipefail -c 'curl -fsSL https://github.com/royosherove/ajx/releases/latest/download/install.sh | bash -s -- --replace' && export PATH="$HOME/.local/bin:$PATH"
```

Options go after `bash -s --`, inside the quotes. The suggested command preserves
any options you supplied, including pinned versions and custom skill directories.
Managed installations update together, and previous version directories remain
under `versions/`. No old installations or backups are deleted automatically.

Downloads are **versioned GitHub Releases** and do not expire with CI artifact
retention. They are public and require no GitHub login. Binaries are not signed
or notarized. Checksums detect damaged downloads; the public repository,
release and HTTPS connection are the source of trust.
See [manual download instructions](GO-CLI.md#download-and-install).

## Manual and offline installation

Download the `.tar.gz` for your platform and `SHA256SUMS` from the same
[release](https://github.com/royosherove/ajx/releases). Put them in one directory.
For example, on Apple silicon:

```sh
grep '  ajx-darwin-arm64.tar.gz$' SHA256SUMS | shasum -a 256 -c -
tar -xzf ajx-darwin-arm64.tar.gz
bash ajx-darwin-arm64/install.sh --package-dir .
export PATH="$HOME/.local/bin:$PATH"
```

Use `sha256sum -c -` on Linux and substitute its package name in both commands. The
installer verifies the archive again. Only install downloaded artifacts from a
source you trust. Do not combine a binary from one build with a skill from another.

## Work from a source checkout

For development, install Git and Python 3.11+, clone the desired revision,
and link its complete skill directory:

```sh
git clone https://github.com/royosherove/ajx.git
cd ajx
mkdir -p "$HOME/.agents/skills"
ln -s "$PWD/skills/ajx" "$HOME/.agents/skills/ajx"
export PATH="$PWD/skills/ajx/bin:$PATH"
ajx --version
```

Use `.claude/skills` or `.kiro/skills` for those hosts. If the destination already
exists, inspect it first; do not overwrite a working installation blindly.
To build the native executable instead, see [the Go CLI](GO-CLI.md#build-from-source).

Python plugins require the Python runtime. In a native installation, the
original Python launcher is retained as `bin/ajx-python` inside the skill:

```sh
python3 "$HOME/.agents/skills/ajx/bin/ajx-python" --version
```
