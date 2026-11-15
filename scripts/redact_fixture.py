#!/usr/bin/env python3
"""Make a commit-safe copy of a 9Router backup.

Every secret field (keys, tokens, passwords, cookies, JWTs) is replaced with a
unique placeholder "REDACTED-<field>-<n>", and personal identifiers (emails,
user/machine/project ids, tunnel URL, error text) are replaced too.
Usage: redact_fixture.py <backup.json> <out.json>
"""
import json, re, sys

SECRET = re.compile(r"(apikey|^key$|token|password|secret|cookie|jwt|credential|session)", re.I)
PII = {"email", "userid", "username", "machineid", "projectid", "chatgptaccountid",
       "tunnelurl", "lasterror", "displayname"}
counter = 0

def scrub(v, key=""):
    global counter
    if isinstance(v, dict):
        return {k: scrub(x, k) for k, x in v.items()}
    if isinstance(v, list):
        return [scrub(x, key) for x in v]
    if v is None or isinstance(v, bool):
        return v
    lk = key.lower()
    if SECRET.search(key) and not lk.startswith("auth"):
        counter += 1
        return f"REDACTED-{key}-{counter}"
    if lk in PII:
        return f"redacted-{key}"
    if lk == "name" and isinstance(v, str) and ("@" in v or "|" in v):
        counter += 1
        return f"account-{counter}"
    return v

data = json.load(open(sys.argv[1]))
json.dump(scrub(data), open(sys.argv[2], "w"), indent=2)
