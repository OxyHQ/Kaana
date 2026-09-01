#!/usr/bin/env python3
"""Reconcile Kaana's one owned Cloudflare rate-limiting rule."""

from __future__ import annotations

import argparse
import copy
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any


API_BASE = "https://api.cloudflare.com/client/v4"
ZONE_NAME = "kaana.ai"
PHASE = "http_ratelimit"
RULE_REF = "kaana_public_inference_per_ip"
RULE_DESCRIPTION = "Kaana public inference: 20 requests per 10 seconds per IP"
RULESET_NAME = "Kaana public inference rate limiting"

DESIRED_RULE: dict[str, Any] = {
    "ref": RULE_REF,
    "description": RULE_DESCRIPTION,
    # Path is available in rate-limiting expressions on every Cloudflare plan.
    # Method is deliberately absent because it is not available on Free/Pro.
    "expression": '(http.request.uri.path eq "/internal/v1/inference")',
    "action": "block",
    "ratelimit": {
        # cf.colo.id is mandatory for API-authored rate limits. ip.src is the
        # counting characteristic available on every Cloudflare plan.
        "characteristics": ["cf.colo.id", "ip.src"],
        "period": 10,
        "requests_per_period": 20,
        "mitigation_timeout": 10,
    },
    "enabled": True,
}


class ReconcileError(RuntimeError):
    """The live state cannot be reconciled safely."""


@dataclass(frozen=True)
class APIResponse:
    status: int
    payload: dict[str, Any]


class CloudflareAPI:
    def __init__(self, token: str) -> None:
        self._token = token

    def call(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        allowed_statuses: tuple[int, ...] = (),
    ) -> APIResponse:
        request = urllib.request.Request(
            f"{API_BASE}{path}",
            method=method,
            data=json.dumps(body).encode() if body is not None else None,
            headers={
                "Authorization": f"Bearer {self._token}",
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                payload = json.load(response)
                status = response.status
        except urllib.error.HTTPError as error:
            status = error.code
            try:
                payload = json.load(error)
            except (json.JSONDecodeError, UnicodeDecodeError):
                payload = {"success": False, "errors": [{"message": "non-JSON API error"}]}
        except urllib.error.URLError as error:
            raise ReconcileError(f"Cloudflare API {method} {path} could not be reached: {error.reason}") from error

        if status not in allowed_statuses and not 200 <= status < 300:
            errors = payload.get("errors") or []
            raise ReconcileError(f"Cloudflare API {method} {path} returned HTTP {status}: {errors}")
        if status != 404 and not payload.get("success"):
            raise ReconcileError(
                f"Cloudflare API {method} {path} reported failure: {payload.get('errors') or []}"
            )
        return APIResponse(status=status, payload=payload)


def _result(response: APIResponse, context: str) -> Any:
    if "result" not in response.payload:
        raise ReconcileError(f"Cloudflare returned no result while {context}")
    return response.payload["result"]


def resolve_zone(api: CloudflareAPI) -> str:
    query = urllib.parse.urlencode({"name": ZONE_NAME, "status": "active", "match": "all"})
    response = api.call("GET", f"/zones?{query}")
    zones = _result(response, f"resolving the {ZONE_NAME} zone") or []
    if len(zones) != 1 or zones[0].get("name") != ZONE_NAME:
        raise ReconcileError(f"expected one active {ZONE_NAME} zone, found {len(zones)}")
    zone_id = zones[0].get("id")
    if not isinstance(zone_id, str) or not zone_id:
        raise ReconcileError(f"the {ZONE_NAME} zone has no id")
    return zone_id


def get_entrypoint(api: CloudflareAPI, zone_id: str) -> dict[str, Any] | None:
    response = api.call(
        "GET",
        f"/zones/{zone_id}/rulesets/phases/{PHASE}/entrypoint",
        allowed_statuses=(404,),
    )
    if response.status == 404:
        return None
    ruleset = _result(response, f"reading the {PHASE} entry point")
    if not isinstance(ruleset, dict):
        raise ReconcileError(f"the {PHASE} entry point is not an object")
    if ruleset.get("phase") != PHASE or ruleset.get("kind") != "zone":
        raise ReconcileError(
            f"unexpected entry point shape: phase={ruleset.get('phase')!r} "
            f"kind={ruleset.get('kind')!r}"
        )
    return ruleset


def _managed_view(rule: dict[str, Any]) -> dict[str, Any]:
    return {key: rule.get(key) for key in DESIRED_RULE}


def rule_drift(rule: dict[str, Any]) -> list[str]:
    actual = _managed_view(rule)
    return [
        key
        for key, wanted in DESIRED_RULE.items()
        if actual.get(key) != wanted
    ]


def find_owned_rule(ruleset: dict[str, Any]) -> dict[str, Any] | None:
    rules = ruleset.get("rules") or []
    if not isinstance(rules, list):
        raise ReconcileError("the rate-limiting entry point has no rules list")

    owned = [rule for rule in rules if isinstance(rule, dict) and rule.get("ref") == RULE_REF]
    if len(owned) > 1:
        raise ReconcileError(f"found {len(owned)} rules with ref {RULE_REF}; refusing to choose one")

    description_conflicts = [
        rule
        for rule in rules
        if isinstance(rule, dict)
        and rule.get("description") == RULE_DESCRIPTION
        and rule.get("ref") != RULE_REF
    ]
    if description_conflicts:
        raise ReconcileError(
            f"a rule uses Kaana's managed description without ref {RULE_REF}; refusing to duplicate it"
        )
    return owned[0] if owned else None


def verify(api: CloudflareAPI, zone_id: str) -> None:
    ruleset = get_entrypoint(api, zone_id)
    if ruleset is None:
        raise ReconcileError("read-back found no rate-limiting entry point")
    rule = find_owned_rule(ruleset)
    if rule is None:
        raise ReconcileError(f"read-back found no rule with ref {RULE_REF}")
    drift = rule_drift(rule)
    if drift:
        raise ReconcileError(f"read-back still differs in: {', '.join(drift)}")
    print(f"OK {RULE_REF}: {RULE_DESCRIPTION}")


def reconcile(api: CloudflareAPI, action: str) -> int:
    apply = action == "apply"
    zone_id = resolve_zone(api)
    ruleset = get_entrypoint(api, zone_id)

    if ruleset is None:
        if not apply:
            print(f"MISSING {RULE_REF}: no {PHASE} entry point exists")
            return 2
        api.call(
            "POST",
            f"/zones/{zone_id}/rulesets",
            {
                "name": RULESET_NAME,
                "description": "Kaana-owned rules applied before the data plane reads a request body.",
                "kind": "zone",
                "phase": PHASE,
                "rules": [copy.deepcopy(DESIRED_RULE)],
            },
        )
        print(f"CREATED {PHASE} entry point with {RULE_REF}")
        verify(api, zone_id)
        return 0

    rule = find_owned_rule(ruleset)
    ruleset_id = ruleset.get("id")
    if not isinstance(ruleset_id, str) or not ruleset_id:
        raise ReconcileError(f"the {PHASE} entry point has no id")

    if rule is None:
        if not apply:
            print(f"MISSING {RULE_REF}: {PHASE} exists but Kaana's rule does not")
            return 2
        api.call(
            "POST",
            f"/zones/{zone_id}/rulesets/{ruleset_id}/rules",
            copy.deepcopy(DESIRED_RULE),
        )
        print(f"CREATED {RULE_REF}")
        verify(api, zone_id)
        return 0

    drift = rule_drift(rule)
    if not drift:
        print(f"OK {RULE_REF}: {RULE_DESCRIPTION}")
        return 0
    if not apply:
        print(f"DRIFT {RULE_REF}: {', '.join(drift)}")
        return 2

    rule_id = rule.get("id")
    if not isinstance(rule_id, str) or not rule_id:
        raise ReconcileError(f"the rule with ref {RULE_REF} has no id")
    api.call(
        "PATCH",
        f"/zones/{zone_id}/rulesets/{ruleset_id}/rules/{rule_id}",
        copy.deepcopy(DESIRED_RULE),
    )
    print(f"UPDATED {RULE_REF}: {', '.join(drift)}")
    verify(api, zone_id)
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--action", choices=("check", "apply"), required=True)
    args = parser.parse_args()

    token = os.environ.get("CLOUDFLARE_API_TOKEN", "").strip()
    if not token:
        print("CLOUDFLARE_API_TOKEN is empty", file=sys.stderr)
        return 1
    try:
        return reconcile(CloudflareAPI(token), args.action)
    except ReconcileError as error:
        print(f"ERROR {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
