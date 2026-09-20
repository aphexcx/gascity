"""Run the real reaper trim/conversion/selection code in each requested Bash."""

from pathlib import Path
import subprocess
import sys

root = Path(__file__).resolve().parents[3]
source = (root / "internal/bootstrap/packs/core/assets/scripts/reaper.sh").read_text()
trim = "\n".join(line for line in source.splitlines() if line.startswith('STALE_ISSUE_AGE="${STALE_ISSUE_AGE'))
conversion = source[source.index("duration_to_hours() {"):].split("\n}", 1)[0] + "\n}"
arm = source[source.index("STALE_CLOSE_DISABLED=0"):].split("\nesac", 1)[0] + "\nesac"
program = 'set -euo pipefail\nSTALE_ISSUE_AGE="$1"\n' + trim + "\n" + conversion + "\n" + arm + '\nprintf "%s|%s\\n" "$STALE_CLOSE_DISABLED" "${STALE_AGE_H-}"\n'

# The original 27 inputs, with round-2 expectations, followed by new forms.
# The third field indicates whether the single invalid-input warning is due.
cases = [
    ("off", "1|", False),
    (" NeVeR\t", "1|", False),
    ("0", "1|", False),
    ("0h", "1|", False),
    ("0m", "1|", False),
    ("0s", "1|", False),
    ("00", "1|", False),
    ("+0", "1|", False),
    ("+00H", "1|", True),
    (" \t+000M\r\n", "1|", True),
    ("+000s", "1|", False),
    ("0" * 128, "1|", False),
    ("1h", "0|1", False),
    ("720h", "0|720", False),
    ("48h", "0|48", False),
    ("10h", "0|10", False),
    ("01h", "0|01", False),
    ("+01h", "0|01", False),
    ("0.0h", "1|", False),
    ("-0h", "1|", False),
    ("+", "1|", True),
    ("h", "1|", True),
    ("++0", "1|", True),
    ("0hh", "1|", True),
    ("0ms", "1|", False),
    ("0 h", "1|", True),
    ("", "1|", True),
    ("-0", "1|", False),
    ("0h0m", "1|", False),
    ("0us", "1|", False),
    ("0ns", "1|", False),
    ("+0.0h", "1|", False),
    ("-1h", "1|", True),
    ("0.5h", "1|", True),
    ("30m", "1|", True),
    ("1h30m", "1|", True),
    ("0H", "1|", True),
    ("abc", "1|", True),
    ("1d", "1|", True),
    ("+48h", "0|48", False),
    ("48", "0|48", False),
    ("0.000s", "1|", False),
    ("0h0m0s", "1|", False),
    ("0µs", "1|", True),
    ("0μs", "1|", True),
    ("48H", "1|", True),
    ("876000h", "0|876000", False),
    (" \t-0h\r\n", "1|", False),
    (" \t+48h\r\n", "0|48", False),
    (" \tabc\r\n", "1|", True),
    (".0h", "1|", True),
    ("0.h", "1|", True),
]

for bash in sys.argv[1:]:
    version = subprocess.check_output([bash, "--version"], text=True).splitlines()[0]
    print(f"Interpreter: {bash}; {version}")
    for value, expected, invalid in cases:
        result = subprocess.run([bash, "-c", program, "reaper-parser", value], capture_output=True, text=True)
        warning = (
            f"reaper: GC_REAPER_STALE_ISSUE_AGE={value.strip()} is not off, never, a zero, "
            "or a positive whole number of hours (Nh or N); age-based issue closes are disabled for this run\n"
        ) if invalid else ""
        assert (result.returncode, result.stdout, result.stderr) == (0, expected + "\n", warning), (value, result)
        print(f"PASS {value!r}: {expected}; stderr lines={int(invalid)}")
    print(f"PASS: {len(cases)}/{len(cases)} cases")
