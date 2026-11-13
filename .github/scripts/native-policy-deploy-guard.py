#!/usr/bin/env python3
"""Validate native policy continuity before ECS registration and service update."""
import argparse
import hashlib
import json
from pathlib import Path
import re
import sys

PATH = "KAANA_NATIVE_POLICY_PATH"
SHA = "KAANA_NATIVE_POLICY_SHA256"


def policy_pair(task, container):
    rows = [row for row in task.get("containerDefinitions", []) if row.get("name") == container]
    if len(rows) != 1:
        raise ValueError("exact serving container required")
    row = rows[0]
    if any(secret.get("name") in (PATH, SHA) for secret in row.get("secrets", [])):
        raise ValueError("native policy must be explicit non-secret environment")
    found = {}
    for env in row.get("environment", []):
        name = env.get("name")
        if name in (PATH, SHA):
            if name in found or not isinstance(env.get("value"), str) or not env["value"]:
                raise ValueError("duplicate or empty native policy variable")
            found[name] = env["value"]
    if found and set(found) != {PATH, SHA}:
        raise ValueError("partial native policy configuration")
    return found


def validate(live, candidate, source_root, container="kaana", expected=None):
    current = policy_pair(live, container)
    next_pair = policy_pair(candidate, container)
    if current and not next_pair:
        raise ValueError("cannot remove configured native authority")
    if expected is not None and next_pair != policy_pair(expected, container):
        raise ValueError("registered native policy differs from reviewed candidate")
    if next_pair:
        match = re.fullmatch(r"/etc/kaana-native/([A-Za-z0-9_-][A-Za-z0-9_.-]*\.json)", next_pair[PATH])
        if not match or not re.fullmatch(r"[0-9a-f]{64}", next_pair[SHA]):
            raise ValueError("invalid native policy path or SHA256")
        root = (source_root / "configs/native-admission").resolve()
        source = (root / match.group(1)).resolve()
        if source.parent != root or not source.is_file():
            raise ValueError("candidate image source does not contain native policy")
        if hashlib.sha256(source.read_bytes()).hexdigest() != next_pair[SHA]:
            raise ValueError("candidate policy SHA256 differs from image source")
    return bool(next_pair)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--live", type=Path, required=True)
    parser.add_argument("--candidate", type=Path, required=True)
    parser.add_argument("--expected", type=Path)
    parser.add_argument("--source-root", type=Path, default=Path("."))
    parser.add_argument("--container", default="kaana")
    args = parser.parse_args()
    try:
        configured = validate(json.loads(args.live.read_text()), json.loads(args.candidate.read_text()), args.source_root, args.container, json.loads(args.expected.read_text()) if args.expected else None)
    except (OSError, ValueError, TypeError, KeyError, AttributeError):
        # Never dump a task definition, environment, source bytes or raw error.
        print("native policy deployment guard refused candidate", file=sys.stderr)
        return 1
    print(json.dumps({"nativePolicyVerified": True, "configured": configured}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
