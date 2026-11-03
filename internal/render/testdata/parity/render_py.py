"""Render a parity fixture with the Python implementation (no agent or model call).

Usage: python3 -I render_py.py SKILL_LIB DIR [--clusters]
"""
import json
import sys
from pathlib import Path
from types import SimpleNamespace

sys.path.insert(0, sys.argv[1])
from ajx import render  # noqa: E402

out = Path(sys.argv[2])
data = json.loads((out / "cases.json").read_text(encoding="utf-8"))
spec = data["spec"]
spec["trial"]["output_dir"] = str(out)
for case in data["cases"]:
    rd = out / "runs" / case["dir"]
    state = json.loads((rd / "state.json").read_text(encoding="utf-8"))
    harness = SimpleNamespace(name=case["harness"], clean_supported=case["clean_supported"])
    render.write_run_json(spec, case["cell"], case["rep"], rd, state, harness, None, None)
    render.run_report(rd)
clusters = json.loads((out / "clusters.json").read_text(encoding="utf-8"))
render.synthesize_clusters = lambda *args: clusters
render.matrix_report(spec, synthesize="--clusters" in sys.argv, log=lambda message: None)
