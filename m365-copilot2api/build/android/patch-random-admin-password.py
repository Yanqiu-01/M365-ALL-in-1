#!/usr/bin/env python3
"""Replace the APK's embedded admin password with a build-specific random secret.

The generated secret is written outside the APK work tree with mode 0600. This
prevents the user's Microsoft account password from being packaged in Android
resources while preserving the local gateway's existing admin authentication.
"""
from pathlib import Path
import secrets
import stat
import sys
import xml.etree.ElementTree as ET

if len(sys.argv) != 3:
    raise SystemExit("usage: patch-random-admin-password.py <apktool-work-dir> <secret-output-file>")

work = Path(sys.argv[1])
secret_file = Path(sys.argv[2])
strings_file = work / "res/values/strings.xml"

if not strings_file.is_file():
    raise SystemExit(f"Android string resources not found: {strings_file}")

tree = ET.parse(strings_file)
root = tree.getroot()
target = None
for node in root.findall("string"):
    if node.get("name") == "default_admin_password":
        target = node
        break
if target is None:
    raise SystemExit("default_admin_password resource not found")

secret = "***REMOVED-CREDENTIAL***"
target.text = secret
tree.write(strings_file, encoding="utf-8", xml_declaration=True)

secret_file.parent.mkdir(parents=True, exist_ok=True)
secret_file.write_text(secret + "\n", encoding="utf-8")
secret_file.chmod(stat.S_IRUSR | stat.S_IWUSR)

print(f"randomized local admin password; recovery file: {secret_file}")
