"""Check an analyzer HTML report against the expected results.

Usage: check_report.py REPORT.html EXPECTED.json

Compares the numbers in the report (every cache size and chunk size) with the
expected file, and checks that the report holds no request content from the
synthetic traces.
"""

import json
import re
import sys

report_path, expected_path = sys.argv[1], sys.argv[2]
with open(report_path, encoding="utf-8") as fh:
    html = fh.read()
match = re.search(r'<script id="data" type="application/json">(.*?)</script>', html, re.S)
if not match:
    sys.exit(f"FAIL {report_path}: no report data. Output:\n{html[-3000:]}")
data = json.loads(match.group(1))
with open(expected_path, encoding="utf-8") as fh:
    expected = json.load(fh)

# Every expected cell and field must match. Fields or cells that a newer LMCache
# adds are allowed.
cells = data.get("cells", {})
for key, want in expected["cells"].items():
    got = cells.get(key)
    if got is None:
        sys.exit(f"FAIL {report_path}: missing cell {key} ({expected_path})")
    for field, value in want.items():
        if got.get(field) != value:
            sys.exit(f"FAIL {report_path}: cell {key} {field} = {got.get(field)}, expected {value}")

for marker in ("zebrafinch", "Question:", "req-1", '"1000,'):
    if marker in html:
        sys.exit(f"FAIL {report_path}: report contains request content ({marker!r})")

print(f"ok {report_path}: {data['meta']}")
