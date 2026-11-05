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
REPOSITORY = "royosherove/ajx"


def github_json(path):
    return json.loads(subprocess.check_output(
        ["gh", "api", f"repos/{REPOSITORY}/{path}"], text=True
    ))


def is_main_build(run, commit):
    """Accept completed CI from this repository, never a PR or another ref."""
    return (
        type(run.get("id")) is int and run["id"] > 0
        and type(run.get("run_attempt")) is int and run["run_attempt"] > 0
        and run.get("head_sha") == commit and run.get("head_branch") == "main"
        and run.get("event") in ("push", "workflow_dispatch")
        and run.get("path") == ".github/workflows/ci.yml"
        and run.get("status") == "completed" and run.get("conclusion") == "success"
        and run.get("repository", {}).get("full_name") == REPOSITORY
        and run.get("head_repository", {}).get("full_name") == REPOSITORY
    )


def has_native_packages(inventory, run):
    artifacts = inventory.get("artifacts", [])
    names = {f"ajx-{target}" for target in TARGETS}
    if (inventory.get("total_count") != len(artifacts)
            or len(artifacts) != len(names)
            or {item.get("name") for item in artifacts} != names):
        return False
    repository_id = run.get("repository", {}).get("id")
    if type(repository_id) is not int or repository_id <= 0:
        return False
    return all(
        type(item.get("id")) is int and item["id"] > 0
        and item.get("expired") is False
        and item.get("workflow_run", {}).get("id") == run["id"]
        and item["workflow_run"].get("head_sha") == run["head_sha"]
        and item["workflow_run"].get("head_branch") == "main"
        and item["workflow_run"].get("repository_id") == repository_id
        and item["workflow_run"].get("head_repository_id") == repository_id
        for item in artifacts
    )


def download_main_packages(packages, commit):
    """Promote the exact tested main build instead of rebuilding a release tag."""
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("Release commit must be a full Git SHA")
    if packages.exists() and any(packages.iterdir()):
        raise ValueError("Package download directory must be empty")
    response = github_json(
        "actions/workflows/ci.yml/runs"
        f"?branch=main&head_sha={commit}&status=success&per_page=100"
    )
    candidates = sorted(
        (run for run in response.get("workflow_runs", []) if is_main_build(run, commit)),
        key=lambda run: run["id"], reverse=True,
    )
    for run in candidates:
        inventory = github_json(f"actions/runs/{run['id']}/artifacts?per_page=100")
        if not has_native_packages(inventory, run):
            continue
        packages.mkdir(parents=True, exist_ok=True)
        command = [
            "gh", "run", "download", str(run["id"]), "--repo", REPOSITORY,
            "--dir", str(packages),
        ]
        for target in TARGETS:
            command.extend(["--name", f"ajx-{target}"])
        subprocess.run(command, check=True)
        # A rerun may replace artifacts while downloading. Do not publish a
        # mixture of attempts, or an attempt whose checks are still running.
        latest = github_json(f"actions/runs/{run['id']}")
        after = github_json(f"actions/runs/{run['id']}/artifacts?per_page=100")
        if (not is_main_build(latest, commit)
                or latest["run_attempt"] != run["run_attempt"]
                or not has_native_packages(after, latest)
                or {item["id"] for item in inventory["artifacts"]}
                != {item["id"] for item in after["artifacts"]}):
            raise ValueError("Main CI changed during download; retry after it completes")
        print(f"Using tested packages from https://github.com/{REPOSITORY}/actions/runs/{run['id']}")
        return run["id"]
    raise ValueError(
        "No successful main CI build has all four unexpired native packages for "
        f"{commit}. Rerun main CI for this commit before releasing."
    )


def prepare(packages, output, tag, commit):
    if not re.fullmatch(r"v\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?", tag):
        raise ValueError("Release tag must be a version such as v1.0.0")
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("Release commit must be a full Git SHA")
    # Validate the entire set before staging anything for publication.
    archives = []
    skill_sha256 = None
    for target in TARGETS:
        package = packages / f"ajx-{target}"
        manifest = json.loads((package / "build.json").read_text())
        if (manifest["commit"] != commit or manifest["version"] != tag[1:]
                or manifest.get("package_format") != 1
                or f"{manifest['os']}-{manifest['arch']}" != target):
            raise ValueError(f"{target} does not match the release version, commit or format")
        digest = manifest.get("skill_sha256", "")
        if not re.fullmatch(r"[0-9a-f]{64}", digest) or (skill_sha256 and digest != skill_sha256):
            raise ValueError(f"{target} does not match the release skill content")
        skill_sha256 = digest
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
    (output / "RELEASE.json").write_text(json.dumps({
        "schema": 1, "version": tag[1:], "commit": commit, "skill_sha256": skill_sha256,
    }, indent=2) + "\n")
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
    repo = REPOSITORY
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
            "For later updates, run `ajx update` and choose CLI + skill or CLI only. "
            "`ajx update --check` checks without changing files.\n\n"
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
    parser.add_argument("--from-main-ci", action="store_true",
                        help="Download the successful native package build for this exact main commit")
    args = parser.parse_args()
    if args.from_main_ci:
        download_main_packages(args.packages, args.commit)
    publish(args.packages, args.output, args.tag, args.commit)
