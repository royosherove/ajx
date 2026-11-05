#!/bin/sh
# Shared by both hooks; keep this file compatible with POSIX sh.

init_security_scan() {
    root=$(git rev-parse --show-toplevel)
    cd "$root"
    scanner=$(git config --local --get ajx.gitleaksPath || command -v gitleaks || true)
    if [ -z "$scanner" ] || [ ! -x "$scanner" ]; then
        echo "Install Gitleaks and follow CONTRIBUTING.md before $1." >&2
        exit 1
    fi
}

security_scan() {
    "$scanner" "$@" --redact --no-banner --ignore-gitleaks-allow \
        --max-decode-depth 2 --max-archive-depth 2 \
        --config "$root/.gitleaks.toml"
}
