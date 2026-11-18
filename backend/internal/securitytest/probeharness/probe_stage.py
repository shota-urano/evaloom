#!/usr/bin/env python3
"""Prepare, check and publish program of the probe special harness (spec 14 3.0).

It follows harness contract v1 using only the request, the result path and the
channel Evaloom gives it. It never calls an AI. The test case decides, through
files under probe/ in the committed Profile repository:

  probe/<stage>-plan.json       accesses to attempt (probe_access.py plan);
                                observations go to out/probe-<stage>.json
  probe/<stage>-result.json     values merged into the result JSON
  probe/publish-authorize.json  {"operation", "destination"[, "contentId"]
                                [, "attempts"]}: ask authorize right before
                                the operation and send it to the destination
                                only when allowed; "attempts" (default 1)
                                repeats ask-then-operate with the same
                                approval, recording every reply in
                                out/authorizations.json

Strings may contain {{contentId}} (for publish-authorize.json taken after
the plan ran, right before authorize), {{workspace}}, {{out}}, {{inputs}},
{{taskId}}. A result value "{{omit}}" removes that key.
"""
import sys

sys.dont_write_bytecode = True

import hashlib
import json
import os
import socket
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from probe_access import run_plan  # noqa: E402

CONFIG = Path(__file__).resolve().parent.parent / "probe"


def host_path(path):
    """Maps a guest path when the test runtime runs this program on the host
    without a mount namespace. Inside a real container the variable is absent."""
    try:
        mounts = json.loads(os.environ.get("EVALOOM_FAKE_MOUNTS", ""))
    except ValueError:
        return path
    for target in sorted(mounts, key=len, reverse=True):
        if path == target or path.startswith(target + "/"):
            return mounts[target] + path[len(target):]
    return path


def load(name):
    path = CONFIG / name
    if not path.exists():
        return None
    return json.loads(path.read_text())


def substitute(value, variables):
    if isinstance(value, str):
        for key, replacement in variables.items():
            value = value.replace("{{" + key + "}}", replacement)
        return value
    if isinstance(value, list):
        return [substitute(item, variables) for item in value]
    if isinstance(value, dict):
        return {key: substitute(item, variables) for key, item in value.items()}
    return value


def content_id(workspace):
    marker = Path(workspace) / "probe-prepared.txt"
    data = marker.read_bytes() if marker.exists() else b""
    return hashlib.sha256(data).hexdigest()


def authorize(request, ask):
    approval_id = None
    for approval in request.get("approvals") or []:
        if approval.get("operation") == ask["operation"]:
            approval_id = approval.get("id")
    message = {"op": "authorize", "operation": ask["operation"],
               "approvalId": approval_id, "contentId": ask["contentId"]}
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as conn:
        conn.connect(os.environ["EVALOOM_CHANNEL"])
        conn.sendall((json.dumps(message) + "\n").encode())
        with conn.makefile("rb") as stream:
            line = stream.readline()
    return line


def send_operation(ask, out):
    body = json.dumps({"operation": ask["operation"], "contentId": ask["contentId"]}).encode()
    request = urllib.request.Request(ask["destination"], data=body, method="POST",
                                     headers={"Content-Type": "application/json"})
    # The default opener honours the Sandbox's proxy settings.
    with urllib.request.urlopen(request, timeout=10) as response:
        status = response.status
    (Path(out) / "operation.json").write_text(json.dumps({"status": status}) + "\n")
    return status


def main():
    if len(sys.argv) != 2 or sys.argv[1] not in ("prepare", "check", "publish"):
        raise SystemExit("usage: probe prepare|check|publish")
    stage = sys.argv[1]
    request = json.loads(Path(os.environ["EVALOOM_REQUEST"]).read_text())
    if request["stage"] != stage:
        raise SystemExit("request stage %s, command stage %s" % (request["stage"], stage))
    paths = {key: host_path(request["paths"][key]) for key in ("workspace", "out", "inputs")}
    if stage == "prepare":
        Path(paths["workspace"], "probe-prepared.txt").write_text(
            "prepared for " + request["task"]["id"] + "\n")
    variables = dict(paths, taskId=request["task"]["id"], contentId=content_id(paths["workspace"]))
    plan = load(stage + "-plan.json")
    if plan is not None:
        run_plan(substitute(plan, variables), str(Path(paths["out"], "probe-" + stage + ".json")))
    result = {"contractVersion": "1", "stage": stage, "status": "succeeded",
              "message": "probe " + stage}
    if stage == "check":
        result["contentId"] = variables["contentId"]
    if stage == "publish":
        ask = load("publish-authorize.json")
        if ask is not None:
            # The identifier is taken after the plan ran, right before
            # authorize: what the stage would publish is the workspace as it
            # is now, not as it was when the stage started.
            variables["contentId"] = content_id(paths["workspace"])
            ask = substitute(ask, variables)
            ask.setdefault("contentId", variables["contentId"])
            attempts = int(ask.pop("attempts", 1))
            replies = []
            for _ in range(attempts):
                line = authorize(request, ask)
                if not replies:
                    (Path(paths["out"]) / "authorization.json").write_bytes(line)
                reply = json.loads(line)
                replies.append(reply)
                if reply.get("allowed") is True:
                    send_operation(ask, paths["out"])
                else:
                    result.update(status="failed", errorClass="publish_failed",
                                  message="not allowed: " + str(reply.get("reason")))
            (Path(paths["out"]) / "authorizations.json").write_text(json.dumps(replies) + "\n")
    override = load(stage + "-result.json")
    if override is not None:
        for key, value in substitute(override, variables).items():
            if value == "{{omit}}":
                result.pop(key, None)
            else:
                result[key] = value
    Path(os.environ["EVALOOM_RESULT"]).write_text(json.dumps(result) + "\n")
    print("probe finished " + stage)


if __name__ == "__main__":
    main()
