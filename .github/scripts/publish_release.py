"""Publish a versioned release only after all four native packages pass CI."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[2]
TARGETS = ("darwin-amd64", "darwin-arm64", "linux-amd64", "linux-arm64")


def prepare(packages, output, tag, commit):
    if not re.fullmatch(r"v\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?", tag):
        raise ValueError("Release tag must be a version such as v1.0.0")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("Release commit must be a full Git SHA")
    # Validate the entire set before staging anything for publication.
    archives = []
    for target in TARGETS:
        package = packages / f"ajx-{target}"
        manifest = json.loads((package / "build.json").read_text())
        if (manifest["commit"] != commit or manifest["version"] != tag[1:]
                or manifest.get("package_format") != 1
                or f"{manifest['os']}-{manifest['arch']}" != target):
            raise ValueError(f"{target} does not match the release version, commit or format")
        name = f"ajx-{target}.tar.gz"
        archive = package / name
        checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
        if (package / "SHA256SUMS").read_text().split() != [checksum, name]:
            raise ValueError(f"{target} checksum mismatch")
        archives.append(archive)
    output.mkdir(parents=True, exist_ok=True)
    if any(output.iterdir()):
        raise ValueError("Release staging directory must be empty")
    for archive in archives:
        shutil.copyfile(archive, output / archive.name)
    shutil.copyfile(ROOT / "install.sh", output / "install.sh")
    (output / "VERSION").write_text(tag + "\n")
    (output / "BUILD_COMMIT").write_text(commit + "\n")
    assets = sorted(output.iterdir())
    (output / "SHA256SUMS").write_text("".join(
        f"{hashlib.sha256(asset.read_bytes()).hexdigest()}  {asset.name}\n" for asset in assets
    ))
    return assets + [output / "SHA256SUMS"]


def publish(packages, output, tag, commit):
    assets = prepare(packages, output, tag, commit)
    actual = subprocess.check_output(
        ["git", "rev-parse", f"refs/tags/{tag}^{{commit}}"], cwd=ROOT, text=True
    ).strip()
    if actual != commit:
        raise ValueError("The release tag points to a different commit")
    repo = "royosherove/ajx"
    existing = subprocess.run(
        ["gh", "release", "view", tag, "--repo", repo, "--json", "isDraft"],
        text=True, capture_output=True,
    )
    if existing.returncode == 0:
        if not json.loads(existing.stdout)["isDraft"]:
            raise ValueError("This release is already published; never replace published assets")
        subprocess.run(
            ["gh", "release", "upload", tag, "--repo", repo, "--clobber",
             *map(str, assets)], check=True,
        )
    else:
        notes = (
            f"AJX {tag[1:]} — native CLI and matching Agent Skills bundle.\n\n"
            "Install on macOS or Linux without a GitHub login, Go or Python:\n\n"
            "```sh\n"
            "bash -o pipefail -c 'curl -fsSL "
            "https://github.com/royosherove/ajx/releases/latest/download/install.sh "
            "| bash' && export PATH=\"$HOME/.local/bin:$PATH\"\n"
            "```\n\n"
            "Then run `ajx console demo trials/console-demo` for a simulated investigation, "
            "or start a fresh agent session and ask it to use the AJX skill.\n\n"
            "Packages for macOS and Linux on ARM64 and x86-64 were tested on native "
            "runners, including installation, console startup and a headless demo. "
            "Checksums and the source commit are included. Binaries are not signed "
            "or notarized. See the README for prerequisites for real agent trials.\n\n"
            f"Source commit: `{commit}`\n"
        )
        with tempfile.TemporaryDirectory(prefix="ajx-release-") as directory:
            path = Path(directory) / "notes.md"
            path.write_text(notes)
            subprocess.run(
                ["gh", "release", "create", tag, "--repo", repo, "--verify-tag",
                 "--draft", "--title", f"AJX {tag}", "--notes-file", str(path),
                 *map(str, assets)], check=True,
            )
    # Only make the release discoverable after every asset has uploaded.
    options = ["--prerelease"] if "-" in tag else ["--prerelease=false", "--latest"]
    subprocess.run(
        ["gh", "release", "edit", tag, "--repo", repo, "--draft=false", *options],
        check=True,
    )
    print(f"Published {tag}: https://github.com/{repo}/releases/tag/{tag}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--packages", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--commit", required=True)
    args = parser.parse_args()
    publish(args.packages, args.output, args.tag, args.commit)
