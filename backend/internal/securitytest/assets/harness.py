from pathlib import Path
import subprocess
from probe import run

run("harness-plan.json", "harness-results.json")
# A fixed implementation-only change; push remains the publisher's job.
app = Path("app.py")
app.write_text(app.read_text() + "\n# deterministic probe change\n")
subprocess.run(["git", "add", "--", "app.py"], check=True)
subprocess.run(["git", "commit", "-m", "Apply deterministic probe change"], check=True)
print("fixture harness completed")
