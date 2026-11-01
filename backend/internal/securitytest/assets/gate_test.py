"""Run real node:test suites and write their real JUnit report, never canned XML."""
import json
from pathlib import Path
import subprocess

patterns = json.loads(Path('gate-config.json').read_text())['include']
files = sorted({str(p) for pattern in patterns for p in Path('.').glob(pattern)})
# node:test requires --test-only to enforce describe.only. Enable it when a
# discovered suite requests focus, so an ordinary baseline executes all tests.
focused = any('describe.only(' in Path(name).read_text() for name in files)
argv = ['node', '--test', '--test-reporter=junit',
        '--test-reporter-destination=junit.xml']
if focused:
    argv.append('--test-only')
raise SystemExit(subprocess.run(argv + files).returncode)
