#!/usr/bin/env python3

from __future__ import annotations

import copy
import importlib.util
import pathlib
import sys
import unittest
import urllib.parse
from typing import Any


MODULE_PATH = pathlib.Path(__file__).with_name("cloudflare_dns.py")
WORKFLOW_PATH = MODULE_PATH.parents[1] / "workflows" / "cloudflare-dns.yml"
SPEC = importlib.util.spec_from_file_location("cloudflare_dns", MODULE_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("could not load cloudflare_dns.py")
dns = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = dns
SPEC.loader.exec_module(dns)


def legacy_record(**changes: Any) -> dict[str, Any]:
    record = {
        "id": "legacy-record-1",
        "name": dns.LEGACY_API_NAME,
        "type": "CNAME",
        "content": dns.OLD_SHARED_ALB_DNS,
        "proxied": False,
    }
    record.update(changes)
    return record


class LegacyAPI:
    def __init__(self, records: list[dict[str, Any]]) -> None:
        self.records = copy.deepcopy(records)
        self.calls: list[tuple[str, str, dict[str, Any] | None]] = []

    def call(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
    ) -> Any:
        self.calls.append((method, path, copy.deepcopy(body)))
        if method == "GET" and path.startswith("/zones/zone-1/dns_records?"):
            query = urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)
            if query != {"name": [dns.LEGACY_API_NAME]}:
                raise AssertionError(f"legacy lookup was not exact: {query}")
            return dns.APIResponse(
                200,
                {"success": True, "result": copy.deepcopy(self.records)},
            )
        if method == "DELETE" and path == "/zones/zone-1/dns_records/legacy-record-1":
            self.records = []
            return dns.APIResponse(200, {"success": True, "result": {"id": "legacy-record-1"}})
        raise AssertionError(f"unexpected API call: {method} {path}")


class FullAPI:
    def __init__(self, apex: list[dict[str, Any]], legacy: list[dict[str, Any]]) -> None:
        self.apex = copy.deepcopy(apex)
        self.legacy = copy.deepcopy(legacy)
        self.calls: list[tuple[str, str, dict[str, Any] | None]] = []

    def call(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
    ) -> Any:
        self.calls.append((method, path, copy.deepcopy(body)))
        if method == "GET" and path.startswith("/zones?"):
            return dns.APIResponse(
                200,
                {"success": True, "result": [{"id": "zone-1", "name": dns.ZONE_NAME}]},
            )
        if method == "GET" and path.startswith("/zones/zone-1/dns_records?"):
            query = urllib.parse.parse_qs(urllib.parse.urlsplit(path).query)
            if query.get("name") == [dns.ZONE_NAME]:
                return dns.APIResponse(200, {"success": True, "result": copy.deepcopy(self.apex)})
            if query == {"name": [dns.LEGACY_API_NAME]}:
                return dns.APIResponse(200, {"success": True, "result": copy.deepcopy(self.legacy)})
        if method == "DELETE" and path == "/zones/zone-1/dns_records/legacy-record-1":
            self.legacy = []
            return dns.APIResponse(200, {"success": True, "result": {"id": "legacy-record-1"}})
        raise AssertionError(f"unexpected API call: {method} {path}")


def delete_calls(api: LegacyAPI | FullAPI) -> list[str]:
    return [path for method, path, _ in api.calls if method == "DELETE"]


def retirement_config(alb_dns: str = "kaana-alb.us-west-2.elb.amazonaws.com") -> Any:
    return dns.Config(
        action="apply",
        validation_name="",
        validation_value="",
        alb_dns=alb_dns,
        retire_legacy_api_dns=True,
        legacy_api_target=dns.OLD_SHARED_ALB_DNS,
    )


def workflow_contract_violations(text: str) -> list[str]:
    required = {
        "main-only job": "if: github.ref == 'refs/heads/main'",
        "default-off retirement": "retire_legacy_api_dns:\n        description:",
        "default false": "default: false",
        "explicit old target input": "legacy_api_target:",
        "safe checkout": "persist-credentials: false",
        "reconciler test": "python3 .github/scripts/cloudflare_dns_test.py",
        "reviewed reconciler": "python3 .github/scripts/cloudflare_dns.py",
        "target input forwarding": "LEGACY_API_TARGET: ${{ inputs.legacy_api_target }}",
        "target forwarding": "--legacy-api-target \"$LEGACY_API_TARGET\"",
    }
    return [label for label, fragment in required.items() if fragment not in text]


class LegacyRecordTests(unittest.TestCase):
    def test_exact_dns_only_cname_is_deleted_and_absence_is_read_back(self) -> None:
        api = LegacyAPI([legacy_record()])
        dns.retire_legacy_alias(api, "zone-1", dns.OLD_SHARED_ALB_DNS)
        self.assertEqual(
            [method for method, _, _ in api.calls],
            ["GET", "DELETE", "GET"],
        )
        self.assertEqual(api.records, [])

    def test_record_mutations_fail_before_delete(self) -> None:
        mutations = {
            "wrong name": {"name": "API.kaana.ai"},
            "wrong type": {"type": "A"},
            "proxied": {"proxied": True},
            "missing proxy state": {"proxied": None},
            "wrong target": {"content": "other.us-west-2.elb.amazonaws.com"},
            "leading record whitespace": {"content": f" {dns.OLD_SHARED_ALB_DNS}"},
            "trailing record whitespace": {"content": f"{dns.OLD_SHARED_ALB_DNS} "},
            "missing id": {"id": ""},
        }
        for label, change in mutations.items():
            with self.subTest(label=label):
                api = LegacyAPI([legacy_record(**change)])
                with self.assertRaises(dns.ReconcileError):
                    dns.retire_legacy_alias(api, "zone-1", dns.OLD_SHARED_ALB_DNS)
                self.assertEqual([method for method, _, _ in api.calls], ["GET"])
                self.assertEqual(delete_calls(api), [])

    def test_missing_or_duplicate_name_fails_before_delete(self) -> None:
        for records in ([], [legacy_record(), legacy_record(id="legacy-record-2")]):
            with self.subTest(count=len(records)):
                api = LegacyAPI(records)
                with self.assertRaises(dns.ReconcileError):
                    dns.retire_legacy_alias(api, "zone-1", dns.OLD_SHARED_ALB_DNS)
                self.assertEqual(delete_calls(api), [])

    def test_target_input_requires_exact_reviewed_value_without_whitespace(self) -> None:
        rejected = (
            "",
            f" {dns.OLD_SHARED_ALB_DNS}",
            f"{dns.OLD_SHARED_ALB_DNS} ",
            f"oxy-alb-648111691.\tus-west-2.elb.amazonaws.com",
            "other.us-west-2.elb.amazonaws.com",
            dns.OLD_SHARED_ALB_DNS.upper(),
            f"{dns.OLD_SHARED_ALB_DNS}.",
        )
        for target in rejected:
            with self.subTest(target=target):
                api = LegacyAPI([legacy_record()])
                with self.assertRaises(dns.ReconcileError):
                    dns.retire_legacy_alias(api, "zone-1", target)
                self.assertEqual(api.calls, [])


class RetirementSequenceTests(unittest.TestCase):
    def test_exact_apex_and_liveness_precede_legacy_delete(self) -> None:
        alb_dns = "kaana-alb.us-west-2.elb.amazonaws.com"
        api = FullAPI(
            apex=[
                {
                    "name": dns.ZONE_NAME,
                    "type": "CNAME",
                    "content": alb_dns,
                    "proxied": True,
                }
            ],
            legacy=[legacy_record()],
        )
        live_call_count = 0

        def pass_liveness() -> None:
            nonlocal live_call_count
            live_call_count += 1

        self.assertEqual(dns.reconcile(api, retirement_config(alb_dns), pass_liveness), 0)
        self.assertEqual(live_call_count, 1)
        self.assertEqual(delete_calls(api), ["/zones/zone-1/dns_records/legacy-record-1"])
        self.assertEqual(api.legacy, [])

    def test_apex_drift_stops_before_liveness_and_delete(self) -> None:
        api = FullAPI(
            apex=[
                {
                    "name": dns.ZONE_NAME,
                    "type": "CNAME",
                    "content": "another-alb.us-west-2.elb.amazonaws.com",
                    "proxied": True,
                }
            ],
            legacy=[legacy_record()],
        )
        live_checks: list[bool] = []
        self.assertEqual(
            dns.reconcile(api, retirement_config(), lambda: live_checks.append(True)),
            1,
        )
        self.assertEqual(live_checks, [])
        self.assertEqual(delete_calls(api), [])

    def test_failed_apex_liveness_stops_before_delete(self) -> None:
        alb_dns = "kaana-alb.us-west-2.elb.amazonaws.com"
        api = FullAPI(
            apex=[
                {
                    "name": dns.ZONE_NAME,
                    "type": "CNAME",
                    "content": alb_dns,
                    "proxied": True,
                }
            ],
            legacy=[legacy_record()],
        )

        def fail_liveness() -> None:
            raise dns.ReconcileError("unhealthy")

        with self.assertRaises(dns.ReconcileError):
            dns.reconcile(api, retirement_config(alb_dns), fail_liveness)
        self.assertEqual(delete_calls(api), [])


class WorkflowContractTests(unittest.TestCase):
    def test_workflow_wires_the_tested_reconciler_behind_main_and_default_off(self) -> None:
        text = WORKFLOW_PATH.read_text()
        self.assertEqual(workflow_contract_violations(text), [])

    def test_contract_gate_detects_each_load_bearing_workflow_mutation(self) -> None:
        text = WORKFLOW_PATH.read_text()
        fragments = (
            "if: github.ref == 'refs/heads/main'",
            "default: false",
            "legacy_api_target:",
            "persist-credentials: false",
            "python3 .github/scripts/cloudflare_dns_test.py",
            "python3 .github/scripts/cloudflare_dns.py",
            "LEGACY_API_TARGET: ${{ inputs.legacy_api_target }}",
            "--legacy-api-target \"$LEGACY_API_TARGET\"",
        )
        for fragment in fragments:
            with self.subTest(fragment=fragment):
                mutated = text.replace(fragment, "MUTATED", 1)
                self.assertNotEqual(mutated, text, "mutation did not apply")
                self.assertTrue(workflow_contract_violations(mutated))


if __name__ == "__main__":
    unittest.main()
