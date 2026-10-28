"""Access attempts only. The controller, not this file, decides pass/fail."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import urllib.error
import urllib.request


def attempt(item):
    kind = item["kind"]
    if kind == "read":
        # Do not copy secrets into the untrusted result file.
        return {"bytes": len(Path(item["path"]).read_bytes())}
    if kind == "write":
        # Only disposable test-owned paths may be supplied by the controller.
        with open(item["path"], "ab") as target:
            target.write(item["value"].encode())
        return {}
    if kind == "exists":
        if not Path(item["path"]).exists():
            raise FileNotFoundError(item["path"])
        return {}
    if kind == "http":
        # Ignore proxy settings, so loopback probes actually attempt loopback.
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(item["url"], timeout=item["timeoutSec"]) as response:
                return {"status": response.status}
        except urllib.error.HTTPError as response:
            # A 401/403/404 is still a successful connection to the API.
            return {"status": response.code}
    if kind == "env":
        allowed = set(item["allowlist"])
        values = list(os.environ.values())
        return {"names": sorted(os.environ),
                "unexpected": sorted(set(os.environ) - allowed),
                "secretPresent": any(secret in value
                                     for secret in item["secrets"] for value in values)}
    if kind == "git_push":
        result = subprocess.run(
            ["git", "push", item["remote"], item["refspec"]],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            timeout=item["timeoutSec"], check=False)
        if result.returncode:
            raise RuntimeError("git push exited " + str(result.returncode))
        return {}
    if kind == "command":
        result = subprocess.run(item["argv"], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                timeout=item["timeoutSec"], check=False)
        if result.returncode:
            raise RuntimeError("command exited " + str(result.returncode))
        return {"stdoutBytes": len(result.stdout), "stderrBytes": len(result.stderr)}
    raise ValueError("unknown probe kind: " + kind)


def run(plan_path, result_path):
    output = Path(result_path)
    # Never retain an old result when the plan or an attempt cannot execute.
    output.unlink(missing_ok=True)
    plan = json.loads(Path(plan_path).read_text())
    if not isinstance(plan, list) or not plan:
        raise ValueError("a nonempty probe plan is required")
    observations = []
    ids = set()
    for item in plan:
        if not item["id"] or item["id"] in ids:
            raise ValueError("missing or duplicate probe id")
        ids.add(item["id"])
        # Configuration errors are not evidence of denied access.
        required = {"read": ["path"], "write": ["path", "value"],
                    "exists": ["path"], "http": ["url", "timeoutSec"],
                    "env": ["allowlist", "secrets"],
                    "git_push": ["remote", "refspec", "timeoutSec"],
                    "command": ["argv", "timeoutSec"]}
        for field in required[item["kind"]]:
            item[field]
        if item["kind"] in ("git_push", "command"):
            program = "git" if item["kind"] == "git_push" else item["argv"][0]
            if shutil.which(program) is None:
                raise RuntimeError("probe prerequisite unavailable: " + program)
        try:
            detail = attempt(item)
            succeeded = True
        except (OSError, urllib.error.URLError, subprocess.SubprocessError, RuntimeError) as error:
            detail = {"error": type(error).__name__}
            succeeded = False
        observations.append({"id": item["id"], "kind": item["kind"],
                             "succeeded": succeeded, "detail": detail})
    output.write_text(json.dumps({"observations": observations}, sort_keys=True) + "\n")


if __name__ == "__main__":
    run(sys.argv[1], sys.argv[2])
