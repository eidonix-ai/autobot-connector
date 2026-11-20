import json
import re
import sys
from pathlib import Path

import yaml

source = yaml.safe_load(Path("/run/cdi/nvidia.yaml").read_text())
common = source["containerEdits"]
shared_paths = {"/dev/nvidiactl", "/dev/nvidia-uvm", "/dev/nvidia-uvm-tools"}
shared = [node for node in common["deviceNodes"] if node["path"] in shared_paths]
assert {node["path"] for node in shared} == shared_paths
devices = []
for entry in source["devices"]:
    if entry["name"].startswith("GPU-"):
        nodes = [node for node in entry["containerEdits"]["deviceNodes"]
                 if re.fullmatch(r"/dev/nvidia[0-9]+", node["path"])]
        assert len(nodes) == 1, entry
        devices.append({"name": entry["name"], "containerEdits": {"deviceNodes": nodes}})
assert len(devices) == int(sys.argv[2])

mounts = []
libraries = {"libcuda", "libnvidia-ml", "libnvidia-gpucomp", "libnvidia-ptxjitcompiler"}
for original in common["mounts"]:
    path = Path(original["containerPath"])
    lib = path.name.partition(".so.")[0]
    if path.name != "nvidia-smi" and lib not in libraries:
        continue
    assert "ro" in original["options"]
    mount = dict(original)
    if lib in {"libcuda", "libnvidia-ml"}:
        mount["containerPath"] = str(path.with_name(lib + ".so.1"))
    mounts.append(mount)
assert len(mounts) == 5
all_nodes = [node for device in devices for node in device["containerEdits"]["deviceNodes"]]
assert len({node["path"] for node in all_nodes}) == len(devices)
spec = {
    "cdiVersion": source["cdiVersion"],
    "kind": "flare.local/gpu",
    "devices": devices + [{"name": "all", "containerEdits": {"deviceNodes": all_nodes}}],
    "containerEdits": {"mounts": mounts, "deviceNodes": shared},
}
Path(sys.argv[1]).write_text(json.dumps(spec, indent=2, sort_keys=True) + "\n")
