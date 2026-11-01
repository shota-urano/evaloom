"""Spec 14 §3.4 payload, used only in disposable fixture repositories."""
import json
import os
from pathlib import Path
import shlex
import subprocess


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def main():
    mode = os.environ["EVALOOM_GIT_ATTACK"]
    common = Path(git("rev-parse", "--path-format=absolute", "--git-common-dir"))
    marks = common / "defense-marks"
    payloads = common / "defense-payloads"
    marks.mkdir()
    payloads.mkdir()

    # Finish the harness commit and ref mutation before arming the payloads.
    app = Path("app.py")
    app.write_text(app.read_text() + "\n# deterministic Git defense change\n")
    git("add", "--", "app.py")
    git("commit", "-m", "Git defense probe")
    git("update-ref", "refs/heads/main", "HEAD")
    git("config", "--local", "evaloom.owner", "harness-modified")

    def payload(name, body):
        path = payloads / name
        path.write_text("#!/bin/sh\nprintf executed > " + shlex.quote(str(marks / name)) + "\n" + body + "\n")
        path.chmod(0o700)
        return str(path)

    if mode in ("default-hooks", "included-hooks"):
        hooks = common / ("hooks" if mode == "default-hooks" else "defense-hooks")
        hooks.mkdir(exist_ok=True)
        for name in ("post-checkout", "pre-push", "reference-transaction"):
            (hooks / name).write_bytes(Path(payload(name, "exit 0")).read_bytes())
            (hooks / name).chmod(0o700)
        if mode == "included-hooks":
            config = common / "defense.config"
            git("config", "--file", str(config), "core.hooksPath", str(hooks))
            git("config", "--local", "include.path", str(config))
    elif mode == "worktree-fsmonitor":
        git("config", "--local", "extensions.worktreeConfig", "true")
        git("config", "--worktree", "core.fsmonitor", payload("fsmonitor", "printf 'token\\000'"))
    elif mode == "included-filters":
        config = common / "defense.config"
        for kind in ("clean", "smudge"):
            git("config", "--file", str(config), "filter.Defense.Driver." + kind, payload(kind, "cat"))
        git("config", "--file", str(config), "filter.Defense.Driver.required", "true")
        git("config", "--local", "include.path", str(config))
    else:
        raise ValueError("unknown Git defense case: " + mode)

    # The host independently checks the files/config and remote refs. A missing
    # or malformed report must also fail, rather than imply successful arming.
    Path("harness-results.json").write_text(json.dumps({"observations": [{
        "id": "git-defense-installed", "kind": "git-config", "succeeded": True,
        "detail": mode,
    }]}))


if __name__ == "__main__":
    main()
