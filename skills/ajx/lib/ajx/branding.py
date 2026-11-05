"""Human-facing branding shared by the Python CLI's entry points."""

from . import __version__
from .util import SKILL_ROOT


def banner():
    logo = (SKILL_ROOT / "assets/logo.txt").read_text(encoding="utf-8")
    return f"{logo}v{__version__}\n\n"
