#!/usr/bin/env python3
"""Reconcile Kaana DNS and retire its legacy API alias fail-closed."""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from typing import Any, Callable


API_BASE = "https://api.cloudflare.com/client/v4"
ZONE_NAME = "kaana.ai"
LEGACY_API_NAME = "api.kaana.ai"
OLD_SHARED_ALB_DNS = "oxy-alb-648111691.us-west-2.elb.amazonaws.com"


class ReconcileError(RuntimeError):
    """The live state cannot be reconciled safely."""


@dataclass(frozen=True)
class APIResponse:
    status: int
    payload: dict[str, Any]


@dataclass(frozen=True)
class Config:
    action: str
    validation_name: str
    validation_value: str
    alb_dns: str
    retire_legacy_api_dns: bool
    legacy_api_target: str


class CloudflareAPI:
    def __init__(self, token: str) -> None:
        self._token = token

    def call(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
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
            raise ReconcileError(
                f"Cloudflare API {method} {path} could not be reached: {error.reason}"
            ) from error

        if not 200 <= status < 300:
            raise ReconcileError(
                f"Cloudflare API {method} {path} returned HTTP {status}: "
                f"{payload.get('errors') or []}"
            )
        if not payload.get("success"):
            raise ReconcileError(
                f"Cloudflare API {method} {path} reported failure: "
                f"{payload.get('errors') or []}"
            )
        return APIResponse(status=status, payload=payload)


def _result(response: APIResponse, context: str) -> Any:
    if "result" not in response.payload:
        raise ReconcileError(f"Cloudflare returned no result while {context}")
    return response.payload["result"]


def _normalized_hostname(value: str) -> str:
    return value.strip().rstrip(".").lower()


def _retirement_target(value: str) -> str:
    if not value:
        raise ReconcileError("legacy retirement requires legacy_api_target")
    if any(character.isspace() for character in value):
        raise ReconcileError("legacy_api_target must not contain whitespace")
    if value != OLD_SHARED_ALB_DNS:
        raise ReconcileError(
            "legacy_api_target is not the reviewed old shared Oxy ALB target"
        )
    return value


def resolve_zone(api: CloudflareAPI) -> str:
    query = urllib.parse.urlencode({"name": ZONE_NAME, "status": "active", "match": "all"})
    response = api.call("GET", f"/zones?{query}")
    zones = _result(response, f"resolving the {ZONE_NAME} zone")
    if not isinstance(zones, list):
        raise ReconcileError(f"Cloudflare returned an invalid {ZONE_NAME} zone list")
    if (
        len(zones) != 1
        or not isinstance(zones[0], dict)
        or zones[0].get("name") != ZONE_NAME
    ):
        raise ReconcileError(f"expected one active {ZONE_NAME} zone, found {len(zones)}")
    zone_id = zones[0].get("id")
    if not isinstance(zone_id, str) or not zone_id:
        raise ReconcileError(f"the {ZONE_NAME} zone has no id")
    return zone_id


def _dns_records(
    api: CloudflareAPI,
    zone_id: str,
    name: str,
    record_type: str | None = None,
) -> list[dict[str, Any]]:
    parameters = {"name": name}
    if record_type is not None:
        parameters["type"] = record_type
    query = urllib.parse.urlencode(parameters)
    response = api.call("GET", f"/zones/{zone_id}/dns_records?{query}")
    records = _result(response, f"reading DNS records for {name}")
    if not isinstance(records, list) or any(not isinstance(record, dict) for record in records):
        raise ReconcileError(f"Cloudflare returned invalid DNS records for {name}")
    return records


def _wanted_records(config: Config) -> tuple[list[tuple[str, str, bool]], str]:
    validation_name = _normalized_hostname(config.validation_name)
    validation_value = _normalized_hostname(config.validation_value)
    alb_dns = _normalized_hostname(config.alb_dns)

    if config.retire_legacy_api_dns and config.action != "apply":
        raise ReconcileError("retire_legacy_api_dns requires action=apply")
    if config.retire_legacy_api_dns and not alb_dns:
        raise ReconcileError(
            "retire_legacy_api_dns requires the expected dedicated alb_dns"
        )
    if config.retire_legacy_api_dns:
        _retirement_target(config.legacy_api_target)

    wanted: list[tuple[str, str, bool]] = []
    if validation_name or validation_value:
        if not validation_name or not validation_value:
            raise ReconcileError(
                "validation_name and validation_value must be supplied together"
            )
        if not validation_name.endswith(".kaana.ai"):
            raise ReconcileError("validation_name is outside kaana.ai")
        if not validation_value.endswith(".acm-validations.aws"):
            raise ReconcileError("validation_value is not an ACM validation target")
        wanted.append((validation_name, validation_value, False))

    if alb_dns:
        if not alb_dns.endswith(".us-west-2.elb.amazonaws.com"):
            raise ReconcileError("alb_dns is not a us-west-2 ELB hostname")
        wanted.append((ZONE_NAME, alb_dns, True))

    if not wanted:
        raise ReconcileError("nothing to do")
    return wanted, alb_dns


def _reconcile_record(
    api: CloudflareAPI,
    zone_id: str,
    *,
    name: str,
    value: str,
    proxied: bool,
    apply: bool,
) -> bool:
    existing = _dns_records(api, zone_id, name, "CNAME")
    exact = [
        record
        for record in existing
        if str(record.get("content", "")).rstrip(".").lower() == value
        and bool(record.get("proxied")) == proxied
    ]
    if len(exact) == 1 and len(existing) == 1:
        print(f"OK {name} -> {value} proxied={proxied}")
        return True
    if existing:
        print(
            f"CONFLICT {name}: "
            f"{[(record.get('content'), record.get('proxied')) for record in existing]}"
        )
        return False
    if not apply:
        print(f"MISSING {name} -> {value} proxied={proxied}")
        return True

    api.call(
        "POST",
        f"/zones/{zone_id}/dns_records",
        {
            "type": "CNAME",
            "name": name,
            "content": value,
            "ttl": 120,
            "proxied": proxied,
        },
    )
    print(f"CREATED {name} -> {value} proxied={proxied}")

    verified = _dns_records(api, zone_id, name, "CNAME")
    exact_verified = [
        record
        for record in verified
        if str(record.get("content", "")).rstrip(".").lower() == value
        and bool(record.get("proxied")) == proxied
    ]
    if len(exact_verified) != 1 or len(verified) != 1:
        raise ReconcileError(f"read-back failed for {name}")
    return True


def _verify_apex(api: CloudflareAPI, zone_id: str, alb_dns: str) -> None:
    records = _dns_records(api, zone_id, ZONE_NAME, "CNAME")
    exact = [
        record
        for record in records
        if str(record.get("content", "")).rstrip(".").lower() == alb_dns
        and bool(record.get("proxied"))
    ]
    if len(records) != 1 or len(exact) != 1:
        raise ReconcileError(
            "refusing legacy DNS retirement: apex is not the exact proxied dedicated ALB record"
        )


def check_apex_liveness() -> None:
    request = urllib.request.Request(
        "https://kaana.ai/livez",
        headers={"Accept": "application/json", "User-Agent": "kaana-dns-retirement/1"},
    )
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            if response.status != 200:
                raise ReconcileError(
                    f"refusing legacy DNS retirement: apex livez returned {response.status}"
                )
            live = json.load(response)
    except (urllib.error.URLError, json.JSONDecodeError) as error:
        raise ReconcileError(
            f"refusing legacy DNS retirement: apex livez failed: {error}"
        ) from error
    if live.get("status") != "ok" or not live.get("contractVersion"):
        raise ReconcileError(
            "refusing legacy DNS retirement: apex livez response is not Kaana"
        )


def _legacy_record_id(records: list[dict[str, Any]], expected_target: str) -> str:
    if len(records) != 1:
        raise ReconcileError(
            f"refusing legacy DNS retirement: expected exactly one {LEGACY_API_NAME} record, "
            f"found {len(records)}"
        )
    record = records[0]
    if record.get("name") != LEGACY_API_NAME:
        raise ReconcileError(
            "refusing legacy DNS retirement: Cloudflare returned another record name"
        )
    if record.get("type") != "CNAME":
        raise ReconcileError(
            "refusing legacy DNS retirement: the exact legacy record is not a CNAME"
        )
    if record.get("proxied") is not False:
        raise ReconcileError(
            "refusing legacy DNS retirement: the exact legacy CNAME is not DNS-only"
        )
    if record.get("content") != expected_target:
        raise ReconcileError(
            "refusing legacy DNS retirement: the legacy CNAME target is not byte-exact"
        )
    record_id = record.get("id")
    if not isinstance(record_id, str) or not record_id:
        raise ReconcileError(
            "refusing legacy DNS retirement: the exact legacy record has no id"
        )
    return record_id


def retire_legacy_alias(api: CloudflareAPI, zone_id: str, expected_target: str) -> None:
    expected_target = _retirement_target(expected_target)
    records = _dns_records(api, zone_id, LEGACY_API_NAME)
    record_id = _legacy_record_id(records, expected_target)

    api.call("DELETE", f"/zones/{zone_id}/dns_records/{record_id}")
    if _dns_records(api, zone_id, LEGACY_API_NAME):
        raise ReconcileError(f"read-back failed after deleting {LEGACY_API_NAME}")
    print(
        f"DELETED {LEGACY_API_NAME} after exact apex, livez, and old-target verification"
    )


def reconcile(
    api: CloudflareAPI,
    config: Config,
    live_check: Callable[[], None] = check_apex_liveness,
) -> int:
    wanted, alb_dns = _wanted_records(config)
    zone_id = resolve_zone(api)
    apply = config.action == "apply"

    failed = False
    for name, value, proxied in wanted:
        if not _reconcile_record(
            api,
            zone_id,
            name=name,
            value=value,
            proxied=proxied,
            apply=apply,
        ):
            failed = True
    if failed:
        return 1

    if config.retire_legacy_api_dns:
        _verify_apex(api, zone_id, alb_dns)
        live_check()
        retire_legacy_alias(api, zone_id, config.legacy_api_target)
    return 0


def _bool(value: str) -> bool:
    normalized = value.lower()
    if normalized == "true":
        return True
    if normalized == "false":
        return False
    raise argparse.ArgumentTypeError("expected true or false")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--action", choices=("check", "apply"), required=True)
    parser.add_argument("--validation-name", default="")
    parser.add_argument("--validation-value", default="")
    parser.add_argument("--alb-dns", default="")
    parser.add_argument("--retire-legacy-api-dns", type=_bool, default=False)
    parser.add_argument("--legacy-api-target", default="")
    args = parser.parse_args()

    token = os.environ.get("CLOUDFLARE_API_TOKEN", "").strip()
    if not token:
        print("CLOUDFLARE_API_TOKEN is empty", file=sys.stderr)
        return 1
    config = Config(
        action=args.action,
        validation_name=args.validation_name,
        validation_value=args.validation_value,
        alb_dns=args.alb_dns,
        retire_legacy_api_dns=args.retire_legacy_api_dns,
        legacy_api_target=args.legacy_api_target,
    )
    try:
        return reconcile(CloudflareAPI(token), config)
    except ReconcileError as error:
        print(f"ERROR {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
