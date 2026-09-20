"""Record a command, its complete output, status, and measured wall time."""
from pathlib import Path
import os
import shlex
import subprocess
import sys
import time
log = Path(sys.argv[1])
command = sys.argv[2:]
env = dict(os.environ, CGO_CPPFLAGS="-I/opt/homebrew/opt/icu4c@78/include", CGO_LDFLAGS="-L/opt/homebrew/opt/icu4c@78/lib")
start = time.monotonic()
with log.open("w") as out:
    out.write("CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c@78/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c@78/lib " + shlex.join(command) + "\n")
    out.flush()
    result = subprocess.run(command, stdout=out, stderr=subprocess.STDOUT, env=env)
    out.write(f"\nExit: {result.returncode}; wall seconds: {time.monotonic() - start:.3f}\n")
print(f"{log}: exit {result.returncode}; wall seconds {time.monotonic() - start:.3f}")
sys.exit(result.returncode)
