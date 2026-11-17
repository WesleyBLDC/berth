"""Proof 4: Touch ID, then a Keychain read from a signed helper, bytes only over a pipe."""

import hashlib
import os
import subprocess
import sys

helper = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "bin", "berth-native-spike")
expected = b"not-a-real-secret-berth-spike"
subprocess.run([helper, "add"], check=True)
try:
    r = subprocess.run(
        [helper, "read", "Berth spike: read the fake test item berth-spike-test"], capture_output=True, timeout=120
    )
    got = r.stdout
    print(f"exit {r.returncode}; stderr: {r.stderr.decode().strip() or 'none'}")
    print(
        f"bytes over pipe: {len(got)}, sha256 prefix {hashlib.sha256(got).hexdigest()[:12]}, matches fake value: {got == expected}"
    )
    ok = r.returncode == 0 and got == expected
finally:
    subprocess.run([helper, "delete"], check=True)
sys.exit(0 if ok else 1)
