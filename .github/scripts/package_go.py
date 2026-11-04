"""Build, smoke-test and package the native Go CLI for the current runner."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile


ROOT = Path(__file__).resolve().parents[2]
TARGETS = {("linux", "amd64"), ("linux", "arm64"), ("darwin", "amd64"), ("darwin", "arm64")}


def skill_hash(skill):
    """Match internal/update.SkillHash; CLI version/binaries are independent."""
    files = []
    for part in ("SKILL.md", "prompts", "schemas", "references", "assets", "examples"):
        path = skill / part
        files.extend([path] if path.is_file() else path.rglob("*"))
    digest = hashlib.sha256()
    for path in sorted((p for p in files if p.is_file()), key=lambda p: p.relative_to(skill).as_posix()):
        relative = path.relative_to(skill).as_posix()
        if "__pycache__" in relative:
            continue
        digest.update(relative.encode() + b"\0")
        digest.update(hashlib.sha256(path.read_bytes()).digest())
    return digest.hexdigest()


def command(*args, env=None):
    return subprocess.check_output(args, cwd=ROOT, env=env, text=True).strip()


def package(output, expected_target):
    output = output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    env = {**os.environ, "CGO_ENABLED": "0", "GOFLAGS": "-mod=vendor", "GOPROXY": "off", "GOWORK": "off"}
    target = json.loads(command("go", "env", "-json", "GOOS", "GOARCH", "GOHOSTOS", "GOHOSTARCH", env=env))
    goos, arch = target["GOOS"], target["GOARCH"]
    if (goos, arch) not in TARGETS or (goos, arch) != (target["GOHOSTOS"], target["GOHOSTARCH"]):
        raise SystemExit("Packaging requires a supported native runner so the downloaded binary can be tested.")
    if f"{goos}-{arch}" != expected_target:
        raise SystemExit("The runner architecture does not match the requested download package.")
    commit = command("git", "rev-parse", "HEAD")
    name = f"ajx-{goos}-{arch}"
    archive = output / f"{name}.tar.gz"
    with tempfile.TemporaryDirectory(prefix="ajx-package-") as tmp:
        folder = Path(tmp) / name
        folder.mkdir()
        binary = folder / "ajx"
        subprocess.run([
            "go", "build", "-trimpath", "-buildvcs=false",
            "-ldflags", f"-s -w -X github.com/royosherove/ajx/internal/cli.Build={commit}",
            "-o", str(binary), "./cmd/ajx",
        ], cwd=ROOT, env=env, check=True)
        subprocess.run([str(binary), "--help"], check=True, stdout=subprocess.DEVNULL)
        version = json.loads(command(str(binary), "version", "--json"))
        if version["build"] != commit or version["implementation"] != "go":
            raise SystemExit("The packaged executable does not identify this build.")
        shutil.copyfile(ROOT / "LICENSE", folder / "LICENSE")
        shutil.copyfile(ROOT / "install.sh", folder / "install.sh")
        (folder / "PACKAGE_FORMAT").write_text("1\n")
        (folder / "BUILD_COMMIT").write_text(commit + "\n")
        # Copy tracked resources only: local trials, caches and credentials must
        # never enter an installation package. Keep the skill and CLI in lockstep.
        tracked = subprocess.check_output(
            ["git", "ls-files", "-z", "--", "skills/ajx"], cwd=ROOT
        ).decode().split("\0")
        for relative in filter(None, tracked):
            source = ROOT / relative
            if source.is_symlink():
                raise SystemExit("Skill packages must not contain symbolic links.")
            dest = folder / "skill" / source.relative_to(ROOT / "skills" / "ajx")
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(source, dest)
        (folder / "skill/bin/ajx").rename(folder / "skill/bin/ajx-python")
        shutil.copy2(binary, folder / "skill/bin/ajx")
        # Distribute the notices required by the vendored dependencies too.
        for source in (ROOT / "vendor").rglob("*"):
            if source.is_file() and source.name.upper().startswith(("LICENSE", "LICENCE", "COPYING", "NOTICE")):
                dest = folder / "licenses" / source.relative_to(ROOT / "vendor")
                dest.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(source, dest)
        manifest = {
            "package_format": 1,
            "commit": commit, "version": version["version"], "os": goos, "arch": arch,
            "go_version": command("go", "version", env=env),
            "skill_fingerprint": version["skill_fingerprint"],
            "skill_sha256": skill_hash(folder / "skill"),
            "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
        }
        (folder / "build.json").write_text(json.dumps(manifest, indent=2) + "\n")
        (folder / "INSTALL.txt").write_text(
            f"AJX {version['version']} for {goos}/{arch}\nSource commit: {commit}\n\n"
            "Install the CLI and skill together (from this extracted directory,\n"
            "with the original archive and SHA256SUMS in its parent):\n"
            "  bash install.sh --package-dir ..\n\n"
            "Or install only the executable manually:\n"
            "From this extracted directory:\n"
            '  mkdir -p "$HOME/.local/bin"\n'
            '  install -m 755 ajx "$HOME/.local/bin/ajx"\n'
            '  export PATH="$HOME/.local/bin:$PATH"\n'
            "  ajx version\n"
            "  ajx console demo trials/console-demo\n\n"
            "Add the PATH export to your shell profile to keep it in new terminals.\n"
            "The demo uses no model or cloud credentials. Actual trials need the chosen\n"
            "agent CLI and its authentication. Python plugins need Python 3.11+\n"
            "and the separate skill/bin/ajx-python launcher.\n"
            "See VERSION in a release download for its tag. Main CI artifacts are\n"
            "development builds. Binaries are not signed or notarized.\n"
            "Documentation: https://github.com/royosherove/ajx/blob/main/docs/GO-CLI.md\n"
        )
        def archive_metadata(entry):
            entry.uid = entry.gid = 0
            entry.uname = entry.gname = ""
            entry.mode = 0o755 if entry.isdir() or entry.mode & 0o111 else 0o644
            return entry

        with tarfile.open(archive, "w:gz") as tar:
            tar.add(folder, arcname=name, filter=archive_metadata)
    checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
    (output / "SHA256SUMS").write_text(f"{checksum}  {archive.name}\n")
    (output / "build.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"Packaged and smoke-tested {name} at {commit}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--target", choices=sorted(f"{os_}-{arch}" for os_, arch in TARGETS), required=True)
    args = parser.parse_args()
    package(args.output, args.target)
