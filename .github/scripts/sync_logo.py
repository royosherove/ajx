"""Keep the standalone installer's logo synchronized with the canonical asset."""

import argparse
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
BEGIN = "    # BEGIN GENERATED LOGO (skills/ajx/assets/logo.txt)\n"
END = "    # END GENERATED LOGO\n"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="fail instead of rewriting a stale logo")
    args = parser.parse_args()
    logo = (ROOT / "skills/ajx/assets/logo.txt").read_text(encoding="utf-8")
    if not logo.endswith("\n") or "\nAJX_LOGO\n" in logo:
        parser.error("logo must end in a newline and cannot contain the here-doc delimiter")
    path = ROOT / "install.sh"
    source = path.read_text(encoding="utf-8")
    if source.count(BEGIN) != 1 or source.count(END) != 1:
        parser.error("installer must contain exactly one pair of generated-logo markers")
    before, rest = source.split(BEGIN)
    _, after = rest.split(END)
    generated = BEGIN + "    cat <<'AJX_LOGO'\n" + logo + "AJX_LOGO\n" + END
    updated = before + generated + after
    if updated == source:
        return
    if args.check:
        parser.error("installer logo is stale; run python3 -I .github/scripts/sync_logo.py")
    path.write_text(updated, encoding="utf-8")


if __name__ == "__main__":
    main()
