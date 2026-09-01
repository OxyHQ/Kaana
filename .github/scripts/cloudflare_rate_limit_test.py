#!/usr/bin/env python3

from __future__ import annotations

import copy
import importlib.util
import pathlib
import sys
import unittest
from typing import Any


MODULE_PATH = pathlib.Path(__file__).with_name("cloudflare_rate_limit.py")
SPEC = importlib.util.spec_from_file_location("cloudflare_rate_limit", MODULE_PATH)
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("could not load cloudflare_rate_limit.py")
rate_limit = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = rate_limit
SPEC.loader.exec_module(rate_limit)


class FakeAPI:
    def __init__(self, ruleset: dict[str, Any] | None) -> None:
        self.ruleset = copy.deepcopy(ruleset)
        self.calls: list[tuple[str, str, dict[str, Any] | None]] = []

    def call(
        self,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        allowed_statuses: tuple[int, ...] = (),
    ) -> Any:
        del allowed_statuses
        self.calls.append((method, path, copy.deepcopy(body)))
        if method == "GET" and path.startswith("/zones?"):
            return rate_limit.APIResponse(
                200,
                {"success": True, "result": [{"id": "zone-1", "name": "kaana.ai"}]},
            )
        if method == "GET" and path.endswith("/rulesets/phases/http_ratelimit/entrypoint"):
            if self.ruleset is None:
                return rate_limit.APIResponse(404, {"success": False, "result": None})
            return rate_limit.APIResponse(200, {"success": True, "result": copy.deepcopy(self.ruleset)})
        if method == "POST" and path == "/zones/zone-1/rulesets":
            if body is None:
                raise AssertionError("ruleset create had no body")
            self.ruleset = {
                "id": "ruleset-1",
                "kind": body["kind"],
                "phase": body["phase"],
                "rules": [{"id": "rule-1", **copy.deepcopy(body["rules"][0])}],
            }
            return rate_limit.APIResponse(200, {"success": True, "result": copy.deepcopy(self.ruleset)})
        if method == "POST" and path == "/zones/zone-1/rulesets/ruleset-1/rules":
            if body is None or self.ruleset is None:
                raise AssertionError("rule create had no body or ruleset")
            self.ruleset["rules"].append({"id": "rule-1", **copy.deepcopy(body)})
            return rate_limit.APIResponse(200, {"success": True, "result": self.ruleset["rules"][-1]})
        if method == "PATCH" and path == "/zones/zone-1/rulesets/ruleset-1/rules/rule-1":
            if body is None or self.ruleset is None:
                raise AssertionError("rule update had no body or ruleset")
            self.ruleset["rules"] = [
                {"id": "rule-1", **copy.deepcopy(body)}
                if rule.get("id") == "rule-1"
                else rule
                for rule in self.ruleset["rules"]
            ]
            return rate_limit.APIResponse(200, {"success": True, "result": self.ruleset["rules"][0]})
        raise AssertionError(f"unexpected API call: {method} {path}")


def ruleset_with(rule: dict[str, Any]) -> dict[str, Any]:
    return {
        "id": "ruleset-1",
        "kind": "zone",
        "phase": "http_ratelimit",
        "rules": [{"id": "rule-1", **copy.deepcopy(rule)}],
    }


class ReconcileTests(unittest.TestCase):
    def test_check_missing_entrypoint_is_read_only(self) -> None:
        api = FakeAPI(None)
        self.assertEqual(rate_limit.reconcile(api, "check"), 2)
        self.assertEqual([method for method, _, _ in api.calls], ["GET", "GET"])

    def test_apply_creates_missing_entrypoint_and_reads_it_back(self) -> None:
        api = FakeAPI(None)
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual([method for method, _, _ in api.calls], ["GET", "GET", "POST", "GET"])
        self.assertEqual(api.ruleset["rules"][0]["ref"], rate_limit.RULE_REF)

    def test_check_exact_rule_is_read_only(self) -> None:
        api = FakeAPI(ruleset_with(rate_limit.DESIRED_RULE))
        self.assertEqual(rate_limit.reconcile(api, "check"), 0)
        self.assertEqual([method for method, _, _ in api.calls], ["GET", "GET"])

    def test_apply_appends_to_existing_entrypoint_without_replacing_other_rules(self) -> None:
        ruleset = ruleset_with({"ref": "somebody_else", "enabled": True})
        api = FakeAPI(ruleset)
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual([method for method, _, _ in api.calls], ["GET", "GET", "POST", "GET"])
        self.assertEqual(api.ruleset["rules"][0]["ref"], "somebody_else")
        self.assertEqual(api.ruleset["rules"][1]["ref"], rate_limit.RULE_REF)

    def test_check_drift_is_read_only(self) -> None:
        rule = copy.deepcopy(rate_limit.DESIRED_RULE)
        rule["ratelimit"]["requests_per_period"] = 999
        api = FakeAPI(ruleset_with(rule))
        self.assertEqual(rate_limit.reconcile(api, "check"), 2)
        self.assertEqual([method for method, _, _ in api.calls], ["GET", "GET"])

    def test_apply_updates_only_owned_rule_and_reads_it_back(self) -> None:
        rule = copy.deepcopy(rate_limit.DESIRED_RULE)
        rule["enabled"] = False
        ruleset = ruleset_with(rule)
        ruleset["rules"].append({"id": "other", "ref": "somebody_else", "enabled": True})
        api = FakeAPI(ruleset)
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual([method for method, _, _ in api.calls], ["GET", "GET", "PATCH", "GET"])
        self.assertEqual(api.ruleset["rules"][1]["ref"], "somebody_else")

    def test_duplicate_owned_ref_is_never_mutated(self) -> None:
        ruleset = ruleset_with(rate_limit.DESIRED_RULE)
        ruleset["rules"].append({"id": "rule-2", **copy.deepcopy(rate_limit.DESIRED_RULE)})
        api = FakeAPI(ruleset)
        with self.assertRaises(rate_limit.ReconcileError):
            rate_limit.reconcile(api, "apply")
        self.assertEqual([method for method, _, _ in api.calls], ["GET", "GET"])

    def test_description_without_owned_ref_is_never_duplicated(self) -> None:
        conflicting = copy.deepcopy(rate_limit.DESIRED_RULE)
        conflicting["ref"] = "manual_rule"
        api = FakeAPI(ruleset_with(conflicting))
        with self.assertRaises(rate_limit.ReconcileError):
            rate_limit.reconcile(api, "apply")
        self.assertEqual([method for method, _, _ in api.calls], ["GET", "GET"])


if __name__ == "__main__":
    unittest.main()
