#!/usr/bin/env bash
# Install a released AJX CLI and its matching Agent Skills bundle. Bash 3.2+.
set -euo pipefail

die() { printf 'AJX install: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

usage() {
    cat <<'EOF'
Usage: bash install.sh [options]

Installs the CLI + matching agent skills from the latest stable GitHub Release.
No GitHub sign-in required. Skills are installed for Codex, Claude Code and Kiro.
Requires curl and standard shell tools. No Go, Python, Git or sudo needed.

  --version TAG      Install a particular release, for example v1.0.0
  --package-dir DIR  Install an already downloaded, trusted package
  --skill-dir DIR    Install the skill here (repeatable); replaces default targets
  --no-skills        Install only the CLI
  --no-path          Do not update shell startup files
  --update           Refresh existing managed skill locations without adding new ones
  --compact-summary  Show the concise CLI update summary
  --no-summary       Omit the completion summary; keep progress and diagnostics
  --no-progress      Hide the download progress bar; keep status and errors
  --replace          Back up conflicting files/folders before replacing them
  --help            Show this help

Defaults: ~/.local/bin/ajx, with versioned files under ~/.local/share/ajx.
Skills: ~/.agents/skills/ajx, ~/.claude/skills/ajx, ~/.kiro/skills/ajx.
Rerun the same command to update. Existing managed installations update together.
EOF
}

print_banner() {
    printf '\n'
    # BEGIN GENERATED LOGO (skills/ajx/assets/logo.txt)
    cat <<'AJX_LOGO'
   ░███        ░█████ ░██    ░██
  ░██░██         ░██   ░██  ░██
 ░██  ░██        ░██    ░██░██
░█████████       ░██     ░███
░██    ░██ ░██   ░██    ░██░██
░██    ░██ ░██   ░██   ░██  ░██
░██    ░██  ░██████   ░██    ░██
AJX_LOGO
    # END GENERATED LOGO
    printf 'v%s\n\n' "$1"
}

summary_box() {
    local width=40 row border
    for row in "$@"; do
        if [[ ${#row} -gt "$width" ]]; then width=${#row}; fi
    done
    printf -v border '%*s' "$((width + 2))" ''
    border=${border// /-}
    printf '+%s+\n' "$border"
    for row in "$@"; do printf '| %-*s |\n' "$width" "$row"; done
    printf '+%s+\n' "$border"
}

print_compact_summary() {
    local version=$1 title=$2 cli_only=$3 had_skill=$4 skill_status=Installed
    if [[ "$cli_only" == true ]]; then
        skill_status='Not managed by AJX'
        if [[ "$had_skill" == true ]]; then skill_status='Left unchanged'; fi
    elif [[ "$had_skill" == true ]]; then
        skill_status='Updated; locations kept'
    fi
    print_banner "$version"
    summary_box "$title" "" "$(printf '%-14s%s' CLI Ready)" \
        "$(printf '%-14s%s' 'Agent skills' "$skill_status")"
    if [[ "$cli_only" == false ]]; then
        say ''
        say 'Next: start a fresh agent session to load the updated skill.'
    fi
    say ''
    say 'Details: ajx version --json'
}

sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | cut -d ' ' -f 1
    else
        shasum -a 256 "$1" | cut -d ' ' -f 1
    fi
}

download_file() {
    local url=$1 destination=$2 progress=${3:-false}
    local -a display=(--silent)
    # stderr, not stdin, determines whether curl can redraw a progress bar.
    # This also works when the installer itself is piped into Bash.
    if [[ "$progress" == true && -t 2 ]]; then display=(--progress-bar); fi
    curl --fail --show-error --location --proto '=https' --tlsv1.2 \
        --retry 3 "${display[@]}" "$url" --output "$destination"
}

destination_path() {
    # Resolve existing parent symlinks without following the destination itself.
    local parent suffix
    parent=$(dirname "$1")
    suffix=$(basename "$1")
    while [[ ! -e "$parent" && ! -L "$parent" ]]; do
        suffix="$(basename "$parent")/$suffix"
        parent=$(dirname "$parent")
    done
    [[ -d "$parent" ]] || die "Parent is not a directory: $parent"
    printf '%s/%s\n' "$(cd "$parent" && pwd -P)" "$suffix"
}

replacement_command() {
    local command arg script=${BASH_SOURCE[0]:-} from_file=false
    command='curl -fsSL https://github.com/royosherove/ajx/releases/latest/download/install.sh | bash -s --'
    if [[ "$offline_install" == true && -f "$script" ]]; then
        printf -v command 'bash %q' "$script"
        from_file=true
    fi
    for arg in ${original_args[@]+"${original_args[@]}"}; do
        printf -v command '%s %q' "$command" "$arg"
    done
    command+=' --replace'
    if [[ "$from_file" == false ]]; then
        # Quote the complete pipeline, including any quotes in option values.
        command=${command//\'/\'\\\'\'}
        printf "bash -o pipefail -c '%s'" "$command"
    else
        printf '%s' "$command"
    fi
    if [[ "$no_path" == false ]]; then
        printf ' && export PATH="$HOME/.local/bin:$PATH"'
    fi
    printf '\n'
}

check_destination() {
    local path=$1 target=$2 previous=${3:-}
    if [[ -e "$path" || -L "$path" ]]; then
        if [[ -L "$path" && "$(readlink "$path")" == "$target" ]]; then return; fi
        if [[ -n "$previous" && -L "$path" && "$(readlink "$path")" == "$previous" ]]; then return; fi
        if [[ "$replace" != true ]]; then
            printf 'AJX install: Already exists: %s\n' "$path" >&2
            printf 'To preserve existing files as backups and install AJX, run:\n\n' >&2
            replacement_command >&2
            exit 1
        fi
    fi
}

link_destination() {
    local path=$1 target=$2 previous=${3:-}
    mkdir -p "$(dirname "$path")"
    if [[ -L "$path" && "$(readlink "$path")" == "$target" ]]; then return; fi
    if [[ -n "$previous" && -L "$path" && "$(readlink "$path")" == "$previous" ]]; then
        ln -sfn "$target" "$path"
        return
    fi
    if [[ -e "$path" || -L "$path" ]]; then
        mv "$path" "$path.ajx-backup-$stamp"
        say "Preserved previous installation: $path.ajx-backup-$stamp"
    fi
    ln -s "$target" "$path"
}

configure_path() {
    local file line shell_name
    # Literal $HOME keeps these startup lines portable and out of public artifacts.
    line='case ":$PATH:" in *:"$HOME/.local/bin":*) ;; *) export PATH="$HOME/.local/bin:$PATH" ;; esac # AJX installer'
    shell_name=${SHELL:-}
    case "${shell_name##*/}" in
        zsh) profiles=("$HOME/.zshrc") ;;
        bash) profiles=("$HOME/.bashrc" "$HOME/.bash_profile") ;;
        sh|dash|"") profiles=("$HOME/.profile") ;;
        *) say 'Shell PATH left unchanged; add ~/.local/bin to PATH for your shell.'; return ;;
    esac
    for file in "${profiles[@]}"; do
        if [[ -f "$file" ]] && grep -Fqx "$line" "$file"; then continue; fi
        printf '\n%s\n' "$line" >> "$file"
    done
    path_configured=true
}

main() {
    local release_version="" package_dir="" no_skills=false no_path=false replace=false
    local offline_install=false no_summary=false no_progress=false compact_summary=false had_skill=false
    local path_configured=false kiro_skill=false
    local update_existing=false refresh_skills=false pointer
    local root bin target os arch tmp expected="" base home_physical index matches
    local checksum filename extra archive listing pkg commit release stamp dest native_version
    local installed_version version_pattern title scope progress
    local -a skills profiles original_args
    original_args=("$@")
    skills=()
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --version|--package-dir|--skill-dir)
                [[ $# -ge 2 && -n "$2" ]] || die "$1 needs a value"
                case "$1" in
                    --version) release_version=$2 ;;
                    --package-dir) package_dir=$2; offline_install=true ;;
                    --skill-dir) skills+=("$2") ;;
                esac
                shift 2 ;;
            --no-skills) no_skills=true; shift ;;
            --no-path) no_path=true; shift ;;
            --update) update_existing=true; shift ;;
            --compact-summary) compact_summary=true; shift ;;
            --no-summary) no_summary=true; shift ;;
            --no-progress) no_progress=true; shift ;;
            --replace) replace=true; shift ;;
            --help|-h) usage; return ;;
            *) die "Unknown option: $1 (see --help)" ;;
        esac
    done
    [[ -n "${HOME:-}" && "$HOME" == /* && "$HOME" != / ]] || die 'HOME must be an absolute user directory'
    [[ "$HOME" != *$'\n'* && "$HOME" != *:* ]] || die 'HOME cannot contain a newline or colon'
    [[ -z "$release_version" || -z "$package_dir" ]] || die 'Choose --version or --package-dir, not both'
    if [[ "$no_skills" == true ]]; then
        say 'Installing the AJX CLI only (--no-skills).'
    else
        say 'Installing the AJX CLI + matching agent skills.'
    fi
    home_physical=$(cd "$HOME" && pwd -P)
    root=$(destination_path "$HOME/.local/share/ajx")
    bin=$(destination_path "$HOME/.local/bin/ajx")
    os=$(uname -s)
    arch=$(uname -m)
    case "$os" in Darwin) os=darwin ;; Linux) os=linux ;; *) die "Unsupported OS: $os (macOS and Linux supported)" ;; esac
    case "$arch" in arm64|aarch64) arch=arm64 ;; x86_64|amd64) arch=amd64 ;; *) die "Unsupported architecture: $arch" ;; esac
    target="$os-$arch"
    for dest in tar cut grep awk mktemp readlink; do
        command -v "$dest" >/dev/null 2>&1 || die "Missing required tool: $dest"
    done
    command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 ||
        die 'Install sha256sum or shasum to verify the download'
    tmp=$(mktemp -d "${TMPDIR:-/tmp}/ajx-install.XXXXXX")
    # The trap stores the expanded, shell-quoted path; no dependency on local scope.
    trap "$(printf 'rm -rf -- %q' "$tmp")" EXIT
    if [[ -z "$package_dir" ]]; then
        command -v curl >/dev/null 2>&1 || die 'Install curl to download AJX'
        base=https://github.com/royosherove/ajx/releases
        if [[ -z "$release_version" ]]; then
            download_file "$base/latest/download/VERSION" "$tmp/version" ||
                die 'Could not find a stable AJX release. Check the repository Releases page.'
            release_version=$(cat "$tmp/version")
        fi
        [[ "$release_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] ||
            die 'Invalid release tag; use a version such as v1.0.0'
        say "Downloading AJX $release_version for $target..."
        # Resolve latest once, then use only that version's assets.
        base="$base/download/$release_version"
        package_dir="$tmp/download"
        mkdir -p "$package_dir"
        for filename in SHA256SUMS BUILD_COMMIT "ajx-$target.tar.gz"; do
            progress=false
            if [[ "$filename" == *.tar.gz && "$no_progress" == false ]]; then progress=true; fi
            download_file "$base/$filename" "$package_dir/$filename" "$progress" ||
                die "Could not download $filename for $release_version. Check the release and network connection."
        done
        say 'Download complete. Verifying package...'
        expected=$(cat "$package_dir/BUILD_COMMIT")
        [[ "$expected" =~ ^[0-9a-f]{40}$ ]] || die 'The release did not provide a valid source commit'
    fi
    [[ -f "$package_dir/SHA256SUMS" ]] || die 'Missing SHA256SUMS in the downloaded package'
    matches=0
    checksum=""
    while IFS=' ' read -r sha filename extra; do
        if [[ "$filename" == "ajx-$target.tar.gz" ]]; then
            [[ "$sha" =~ ^[0-9a-f]{64}$ && -z "$extra" ]] || die 'Invalid checksum entry'
            checksum=$sha
            matches=$((matches + 1))
        fi
    done < "$package_dir/SHA256SUMS"
    [[ "$matches" == 1 ]] || die 'Missing or duplicate checksum for this platform; package may be for a different platform'
    archive="$package_dir/ajx-$target.tar.gz"
    [[ -f "$archive" && "$(sha256 "$archive")" == "$checksum" ]] || die 'Package checksum mismatch; nothing installed'
    listing="$tmp/archive-list"
    tar -tzf "$archive" > "$listing"
    while IFS= read -r dest; do
        [[ "$dest" == "ajx-$target" || "$dest" == "ajx-$target/"* ]] || die 'Unsafe archive path'
        [[ "/$dest/" != *"/../"* && "/$dest/" != *"/./"* ]] || die 'Unsafe archive path'
    done < "$listing"
    # Our packages contain regular files and directories only. Reject link escapes.
    tar -tvzf "$archive" | awk 'substr($0,1,1) != "-" && substr($0,1,1) != "d" {exit 1}' ||
        die 'Unexpected archive entry type'
    tar -xzf "$archive" -C "$tmp"
    pkg="$tmp/ajx-$target"
    [[ -f "$pkg/PACKAGE_FORMAT" && "$(cat "$pkg/PACKAGE_FORMAT")" == 1 &&
        -f "$pkg/skill/SKILL.md" && -x "$pkg/skill/bin/ajx" ]] ||
        die 'This package predates the combined CLI/skill installer. Choose a newer release.'
    commit=$(cat "$pkg/BUILD_COMMIT")
    [[ "$commit" =~ ^[0-9a-f]{40}$ && ( -z "$expected" || "$commit" == "$expected" ) ]] ||
        die 'Package source commit does not match the selected release'
    [[ "$(sha256 "$pkg/ajx")" == "$(sha256 "$pkg/skill/bin/ajx")" ]] ||
        die 'The bundled skill CLI differs from the installed CLI'
    native_version=$("$pkg/ajx" version --json)
    grep -Eq '"implementation"[[:space:]]*:[[:space:]]*"go"' <<< "$native_version" &&
        grep -Eq "\"build\"[[:space:]]*:[[:space:]]*\"$commit\"" <<< "$native_version" ||
        die 'The executable does not identify the expected build'
    version_pattern='"version"[[:space:]]*:[[:space:]]*"([^"]+)"'
    [[ "$native_version" =~ $version_pattern ]] || die 'The executable did not report its version'
    installed_version=${BASH_REMATCH[1]}
    [[ "$installed_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] ||
        die 'The executable reported an invalid version'
    if [[ -n "$release_version" ]]; then
        [[ "$installed_version" == "${release_version#v}" ]] ||
            die 'The executable version does not match the selected release'
    fi
    if [[ -d "$root/current/skill" ]]; then had_skill=true; fi
    if [[ "$no_skills" == true ]]; then
        skills=()
    elif [[ "$update_existing" == true && ${#skills[@]} -eq 0 && "$had_skill" == true ]]; then
        refresh_skills=true
    elif [[ ${#skills[@]} -eq 0 ]]; then
        skills=("$HOME/.agents/skills/ajx" "$HOME/.claude/skills/ajx" "$HOME/.kiro/skills/ajx")
    fi
    for ((index=0; index<${#skills[@]}; index++)); do
        dest=${skills[$index]}
        [[ "$dest" == /*/ajx && "$dest" != *$'\n'* &&
            "$dest" != *//* && "/$dest/" != *"/../"* && "/$dest/" != *"/./"* ]] ||
            die '--skill-dir must be an absolute directory ending in /ajx, without dot components'
        if [[ "$dest" == "$HOME/.kiro/skills/ajx" || "$dest" == "$home_physical/.kiro/skills/ajx" ]]; then
            kiro_skill=true
        fi
        dest=$(destination_path "$dest")
        skills[$index]=$dest
        [[ "$home_physical/" != "$dest/"* && "$root/" != "$dest/"* &&
            "$dest/" != "$root/"* && "$dest" != "$bin" ]] ||
            die '--skill-dir must not overlap the home or CLI installation directories'
        check_destination "$dest" "$root/current/skill"
    done
    check_destination "$bin" "$root/cli-current/ajx" "$root/current/ajx"
    for pointer in current cli-current; do
        if [[ -e "$root/$pointer" || -L "$root/$pointer" ]]; then
            [[ -L "$root/$pointer" && "$(readlink "$root/$pointer")" == "$root/versions/"* ]] ||
                die "Unmanaged install directory: $root/$pointer"
        fi
    done
    stamp="$(date -u +%Y%m%dT%H%M%SZ)-$$"
    release="$root/versions/$commit-$target"
    mkdir -p "$root/versions"
    if [[ -e "$release" || -L "$release" ]]; then
        # Reinstall into a fresh directory; preserve even a modified old installation.
        release="$release-$stamp"
    fi
    mv "$pkg" "$release"
    # Keep the old current pointer for skills, including arbitrary custom hosts
    # installed by older releases. CLI-only updates never move that pointer.
    if [[ "$no_skills" == false ]]; then ln -sfn "$release" "$root/current"; fi
    ln -sfn "$release" "$root/cli-current"
    link_destination "$bin" "$root/cli-current/ajx" "$root/current/ajx"
    for dest in ${skills[@]+"${skills[@]}"}; do
        link_destination "$dest" "$root/current/skill"
    done
    if [[ "$no_path" == false ]]; then configure_path; fi
    # Validate the installed entry point without dumping build and hash metadata.
    # Errors remain visible, and a failed executable must not report completion.
    "$bin" version >/dev/null
    if [[ "$no_summary" == true ]]; then return; fi
    title='Installation complete'
    if [[ "$update_existing" == true ]]; then title='Update complete'; fi
    if [[ "$compact_summary" == true ]]; then
        print_compact_summary "$installed_version" "$title" "$no_skills" "$had_skill"
        return
    fi
    if [[ "$refresh_skills" == true ]]; then
        scope='CLI + existing agent skills'
    elif [[ ${#skills[@]} -gt 0 ]]; then
        scope="CLI + agent skills (${#skills[@]} locations)"
    else
        scope='CLI only; agent skills not changed'
    fi
    print_banner "$installed_version"
    summary_box "$title" "" "$scope"
    say ''
    say "CLI: $bin"
    if [[ "$refresh_skills" == true ]]; then
        say "Skill bundle: $root/current/skill"
        say 'Your existing agent skill locations were retained.'
    else
        for dest in ${skills[@]+"${skills[@]}"}; do say "Skill: $dest"; done
    fi
    say ''
    if [[ "$path_configured" == true ]]; then
        say 'New terminal sessions will have ajx on PATH.'
    fi
    if [[ "$no_skills" == false ]]; then
        say 'Next: Start a fresh agent session to load the AJX skill.'
    fi
    if [[ "$update_existing" == false ]]; then
        say 'Try the demo: ajx console demo trials/console-demo'
        if [[ "$no_skills" == false ]]; then
            say 'Agent prompt: Use the AJX skill to review my project.'
        fi
    fi
    if [[ "$kiro_skill" == true ]]; then
        say 'Custom Kiro agents also need skill://~/.kiro/skills/ajx/SKILL.md in their resources list.'
    fi
    say 'Details: ajx version'
}

main "$@"
