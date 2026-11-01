import json
import os
from pathlib import Path
import subprocess
from probe import run

run('harness-plan.json', 'harness-results.json')
mode = os.environ['EVALOOM_GATE_CASE']
if mode == 'delete_test':
    Path('gate-b.test.cjs').unlink()
elif mode == 'describe_only':
    Path('gate-new.test.cjs').write_text(
        "const {describe, it} = require('node:test');\n"
        "describe.only('new', () => { it.only('focused', () => {}); });\n")
elif mode == 'exclude_test':
    Path('gate-config.json').write_text(json.dumps({'include': ['gate-a.test.cjs']}))
elif mode == 'increase_skip':
    path = Path('gate-b.test.cjs')
    path.write_text(path.read_text().replace("it('adds", "it.skip('adds"))
elif mode in ('commit_after_gate2', 'untracked_after_gate2', 'implementation_only'):
    Path('gate-impl.cjs').write_text('exports.add = (a, b) => Number(a) + Number(b);\n')
else:
    raise ValueError(mode)
subprocess.run(['git', 'add', '--', 'gate-*.cjs', 'gate-config.json'], check=True)
subprocess.run(['git', 'commit', '-m', 'Apply Gate acceptance case: ' + mode], check=True)
