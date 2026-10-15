"""Reconcile Kaana's owned Cloudflare rate-limiting rules."""

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
RULESET_NAME = "Kaana public inference rate limiting"

# cf.colo.id is mandatory for API-authored rate limits; ip.src is the counting
# characteristic available on every Cloudflare plan. Method is deliberately
# absent from every expression because it is not available on Free/Pro.
_PER_IP = ["cf.colo.id", "ip.src"]

# The signed /internal lane. Removed with the envelope in Kaana ADR 0002
# phase 5, not before.
INTERNAL_INFERENCE_RULE: dict[str, Any] = {
    "ref": "kaana_public_inference_per_ip",
    "description": "Kaana public inference: 20 requests per 10 seconds per IP",
    "expression": '(http.request.uri.path eq "/internal/v1/inference")',
    "action": "block",
    "ratelimit": {
        "characteristics": _PER_IP,
        "period": 10,
        "requests_per_period": 20,
        "mitigation_timeout": 10,
    },
    "enabled": True,
}

# The public API, https://kaana.ai/v1 (Kaana ADR 0002 phase 3). An
# origin-abuse ceiling, not a customer quota: per-credential and per-app rate
# limits are Oxy's, applied when Kaana asks for a grant. It is generous on
# purpose because many legitimate SDK callers share one IP behind a NAT; it
# exists to bound what an unauthenticated stranger can make Kaana read before
# Oxy has said whose request it is (a transcription body is up to ~28 MiB).
# Realtime upgrades are counted by their own rule, not here.
PUBLIC_API_RULE: dict[str, Any] = {
    "ref": "kaana_public_api_per_ip",
    "description": "Kaana public API /v1: 600 requests per 60 seconds per IP",
    "expression": (
        '(starts_with(http.request.uri.path, "/v1/")'
        ' and http.request.uri.path ne "/v1/realtime")'
    ),
    "action": "block",
    "ratelimit": {
        "characteristics": _PER_IP,
        "period": 60,
        "requests_per_period": 600,
        "mitigation_timeout": 60,
    },
    "enabled": True,
}

# Realtime WebSocket upgrades. Each one opens a session that can hold a task's
# memory and an upstream session for up to an hour, so opening them is counted
# separately and far more strictly than ordinary requests. A resume is an
# upgrade too; 30 a minute leaves room for reconnect storms on one NAT.
PUBLIC_REALTIME_RULE: dict[str, Any] = {
    "ref": "kaana_public_realtime_per_ip",
    "description": "Kaana public realtime /v1/realtime: 30 upgrades per 60 seconds per IP",
    "expression": '(http.request.uri.path eq "/v1/realtime")',
    "action": "block",
    "ratelimit": {
        "characteristics": _PER_IP,
        "period": 60,
        "requests_per_period": 30,
        "mitigation_timeout": 60,
    },
    "enabled": True,
}

# Every rule this reconciler owns, in the order a fresh entry point gets them.
# Each is identified by its stable ref; nothing else in the phase is touched.
DESIRED_RULES: tuple[dict[str, Any], ...] = (
    INTERNAL_INFERENCE_RULE,
    PUBLIC_API_RULE,
    PUBLIC_REALTIME_RULE,
)


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


def _managed_view(rule: dict[str, Any], desired: dict[str, Any]) -> dict[str, Any]:
    return {key: rule.get(key) for key in desired}


def rule_drift(rule: dict[str, Any], desired: dict[str, Any]) -> list[str]:
    actual = _managed_view(rule, desired)
    return [key for key, wanted in desired.items() if actual.get(key) != wanted]


def _rules_of(ruleset: dict[str, Any]) -> list[Any]:
    rules = ruleset.get("rules") or []
    if not isinstance(rules, list):
        raise ReconcileError("the rate-limiting entry point has no rules list")
    return rules


def find_owned_rule(ruleset: dict[str, Any], desired: dict[str, Any]) -> dict[str, Any] | None:
    ref = desired["ref"]
    rules = _rules_of(ruleset)
    owned = [rule for rule in rules if isinstance(rule, dict) and rule.get("ref") == ref]
    if len(owned) > 1:
        raise ReconcileError(f"found {len(owned)} rules with ref {ref}; refusing to choose one")

    description_conflicts = [
        rule
        for rule in rules
        if isinstance(rule, dict)
        and rule.get("description") == desired["description"]
        and rule.get("ref") != ref
    ]
    if description_conflicts:
        raise ReconcileError(
            f"a rule uses Kaana's managed description without ref {ref}; refusing to duplicate it"
        )
    return owned[0] if owned else None


def _check_all_owned(ruleset: dict[str, Any]) -> dict[str, dict[str, Any] | None]:
    """Resolve every owned rule BEFORE any write, so a conflict on one rule
    refuses the whole reconciliation instead of leaving it half applied."""
    return {desired["ref"]: find_owned_rule(ruleset, desired) for desired in DESIRED_RULES}


def verify(api: CloudflareAPI, zone_id: str) -> None:
    ruleset = get_entrypoint(api, zone_id)
    if ruleset is None:
        raise ReconcileError("read-back found no rate-limiting entry point")
    owned = _check_all_owned(ruleset)
    for desired in DESIRED_RULES:
        rule = owned[desired["ref"]]
        if rule is None:
            raise ReconcileError(f"read-back found no rule with ref {desired['ref']}")
        drift = rule_drift(rule, desired)
        if drift:
            raise ReconcileError(f"read-back of {desired['ref']} still differs in: {', '.join(drift)}")
        print(f"OK {desired['ref']}: {desired['description']}")


def reconcile(api: CloudflareAPI, action: str) -> int:
    apply = action == "apply"
    zone_id = resolve_zone(api)
    ruleset = get_entrypoint(api, zone_id)

    if ruleset is None:
        if not apply:
            for desired in DESIRED_RULES:
                print(f"MISSING {desired['ref']}: no {PHASE} entry point exists")
            return 2
        api.call(
            "POST",
            f"/zones/{zone_id}/rulesets",
            {
                "name": RULESET_NAME,
                "description": "Kaana-owned rules applied before the data plane reads a request body.",
                "kind": "zone",
                "phase": PHASE,
                "rules": [copy.deepcopy(desired) for desired in DESIRED_RULES],
            },
        )
        print(f"CREATED {PHASE} entry point with {', '.join(d['ref'] for d in DESIRED_RULES)}")
        verify(api, zone_id)
        return 0

    owned = _check_all_owned(ruleset)
    ruleset_id = ruleset.get("id")
    if not isinstance(ruleset_id, str) or not ruleset_id:
        raise ReconcileError(f"the {PHASE} entry point has no id")

    pending = False
    wrote = False
    for desired in DESIRED_RULES:
        ref = desired["ref"]
        rule = owned[ref]
        if rule is None:
            if not apply:
                print(f"MISSING {ref}: {PHASE} exists but Kaana's rule does not")
                pending = True
                continue
            # Cloudflare requires a new rate-limiting rule at the END of the
            # phase; POST to .../rules appends.
            api.call("POST", f"/zones/{zone_id}/rulesets/{ruleset_id}/rules", copy.deepcopy(desired))
            print(f"CREATED {ref}")
            wrote = True
            continue

        drift = rule_drift(rule, desired)
        if not drift:
            print(f"OK {ref}: {desired['description']}")
            continue
        if not apply:
            print(f"DRIFT {ref}: {', '.join(drift)}")
            pending = True
            continue
        rule_id = rule.get("id")
        if not isinstance(rule_id, str) or not rule_id:
            raise ReconcileError(f"the rule with ref {ref} has no id")
        api.call(
            "PATCH",
            f"/zones/{zone_id}/rulesets/{ruleset_id}/rules/{rule_id}",
            copy.deepcopy(desired),
        )
        print(f"UPDATED {ref}: {', '.join(drift)}")
        wrote = True

    if not apply:
        return 2 if pending else 0
    if wrote:
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
