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
        self._next_id = 100

    def _new_id(self) -> str:
        self._next_id += 1
        return f"rule-{self._next_id}"

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
                "rules": [{"id": self._new_id(), **copy.deepcopy(rule)} for rule in body["rules"]],
            }
            return rate_limit.APIResponse(200, {"success": True, "result": copy.deepcopy(self.ruleset)})
        if method == "POST" and path == "/zones/zone-1/rulesets/ruleset-1/rules":
            if body is None or self.ruleset is None:
                raise AssertionError("rule create had no body or ruleset")
            self.ruleset["rules"].append({"id": self._new_id(), **copy.deepcopy(body)})
            return rate_limit.APIResponse(200, {"success": True, "result": self.ruleset["rules"][-1]})
        prefix = "/zones/zone-1/rulesets/ruleset-1/rules/"
        if method == "PATCH" and path.startswith(prefix):
            rule_id = path[len(prefix):]
            if body is None or self.ruleset is None:
                raise AssertionError("rule update had no body or ruleset")
            if not any(rule.get("id") == rule_id for rule in self.ruleset["rules"]):
                raise AssertionError(f"PATCH of unknown rule {rule_id}")
            self.ruleset["rules"] = [
                {"id": rule_id, **copy.deepcopy(body)} if rule.get("id") == rule_id else rule
                for rule in self.ruleset["rules"]
            ]
            return rate_limit.APIResponse(200, {"success": True, "result": body})
        raise AssertionError(f"unexpected API call: {method} {path}")


RULE = rate_limit.KAANA_RULE

# What the live zone held on 2026-10-15: the /internal-only rule this one
# replaces in place, under the same ref.
TODAY: dict[str, Any] = {
    "ref": "kaana_public_inference_per_ip",
    "description": "Kaana public inference: 20 requests per 10 seconds per IP",
    "expression": '(http.request.uri.path eq "/internal/v1/inference")',
    "action": "block",
    "ratelimit": {
        "characteristics": ["cf.colo.id", "ip.src"],
        "period": 10,
        "requests_per_period": 20,
        "mitigation_timeout": 10,
    },
    "enabled": True,
}


def ruleset_with(*rules: dict[str, Any]) -> dict[str, Any]:
    return {
        "id": "ruleset-1",
        "kind": "zone",
        "phase": "http_ratelimit",
        "rules": [{"id": f"rule-{index}", **copy.deepcopy(rule)} for index, rule in enumerate(rules, 1)],
    }


def complete() -> dict[str, Any]:
    return ruleset_with(*rate_limit.DESIRED_RULES)


def methods(api: FakeAPI) -> list[str]:
    return [method for method, _, _ in api.calls]


def writes(api: FakeAPI) -> list[tuple[str, str]]:
    return [(method, path) for method, path, _ in api.calls if method != "GET"]


class DesiredRulesTests(unittest.TestCase):
    def test_the_zone_plan_holds_one_rule_so_exactly_one_is_owned(self) -> None:
        self.assertEqual(rate_limit.DESIRED_RULES, (RULE,))

    def test_the_rule_is_pinned(self) -> None:
        self.assertEqual(
            RULE,
            {
                "ref": "kaana_public_inference_per_ip",
                "description": "Kaana /internal/v1/inference and /v1: 125 requests per 10 seconds per IP",
                "expression": (
                    '(http.request.uri.path eq "/internal/v1/inference")'
                    ' or starts_with(http.request.uri.path, "/v1/")'
                ),
                "action": "block",
                "ratelimit": {
                    "characteristics": ["cf.colo.id", "ip.src"],
                    "period": 10,
                    "requests_per_period": 125,
                    "mitigation_timeout": 10,
                },
                "enabled": True,
            },
        )

    def test_the_budget_is_the_sum_of_the_per_path_budgets_it_replaces(self) -> None:
        # /internal/v1/inference 20 per 10 s; /v1 600 per 60 s; realtime 30 per 60 s.
        per_ten_seconds = 20 + 600 * 10 // 60 + 30 * 10 // 60
        self.assertEqual(RULE["ratelimit"]["requests_per_period"], per_ten_seconds)
        self.assertEqual(RULE["ratelimit"]["period"], 10)

    def test_it_keeps_the_live_ref_so_apply_updates_instead_of_adding(self) -> None:
        self.assertEqual(RULE["ref"], TODAY["ref"])

    def test_it_counts_the_lanes_and_nothing_else(self) -> None:
        expression = RULE["expression"]
        self.assertIn('"/internal/v1/inference"', expression)
        self.assertIn('starts_with(http.request.uri.path, "/v1/")', expression)
        self.assertNotIn("http.request.method", expression)
        self.assertNotIn("/livez", expression)
        self.assertNotIn('"/internal/"', expression)


class ReconcileTests(unittest.TestCase):
    def test_check_reports_todays_rule_as_drift_read_only(self) -> None:
        api = FakeAPI(ruleset_with(TODAY))
        self.assertEqual(rate_limit.reconcile(api, "check"), 2)
        self.assertEqual(methods(api), ["GET", "GET"])

    def test_apply_on_todays_state_patches_the_one_rule_in_place(self) -> None:
        api = FakeAPI(ruleset_with(TODAY))
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual(writes(api), [("PATCH", "/zones/zone-1/rulesets/ruleset-1/rules/rule-1")])
        self.assertEqual(methods(api)[-1], "GET")
        self.assertEqual(len(api.ruleset["rules"]), 1)

    def test_check_missing_entrypoint_is_read_only(self) -> None:
        api = FakeAPI(None)
        self.assertEqual(rate_limit.reconcile(api, "check"), 2)
        self.assertEqual(methods(api), ["GET", "GET"])

    def test_apply_creates_missing_entrypoint_with_the_rule_and_reads_it_back(self) -> None:
        api = FakeAPI(None)
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual(methods(api), ["GET", "GET", "POST", "GET"])
        self.assertEqual([rule["ref"] for rule in api.ruleset["rules"]], [RULE["ref"]])

    def test_check_exact_rule_is_read_only(self) -> None:
        api = FakeAPI(complete())
        self.assertEqual(rate_limit.reconcile(api, "check"), 0)
        self.assertEqual(methods(api), ["GET", "GET"])

    def test_apply_on_exact_rule_writes_nothing(self) -> None:
        api = FakeAPI(complete())
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual(writes(api), [])

    def test_apply_leaves_other_rules_alone(self) -> None:
        api = FakeAPI(ruleset_with(TODAY, {"ref": "somebody_else", "enabled": True}))
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual(writes(api), [("PATCH", "/zones/zone-1/rulesets/ruleset-1/rules/rule-1")])
        self.assertEqual(api.ruleset["rules"][1], {"id": "rule-2", "ref": "somebody_else", "enabled": True})

    def test_duplicate_owned_ref_is_never_mutated(self) -> None:
        ruleset = complete()
        ruleset["rules"].append({"id": "rule-9", **copy.deepcopy(RULE)})
        api = FakeAPI(ruleset)
        with self.assertRaises(rate_limit.ReconcileError):
            rate_limit.reconcile(api, "apply")
        self.assertEqual(methods(api), ["GET", "GET"])

    def test_description_without_owned_ref_is_never_duplicated(self) -> None:
        conflicting = copy.deepcopy(RULE)
        conflicting["ref"] = "manual_rule"
        api = FakeAPI(ruleset_with(conflicting))
        with self.assertRaises(rate_limit.ReconcileError):
            rate_limit.reconcile(api, "apply")
        self.assertEqual(methods(api), ["GET", "GET"])

    def test_read_back_failure_is_an_error(self) -> None:
        class SwallowingAPI(FakeAPI):
            def call(self, method, path, body=None, allowed_statuses=()):  # type: ignore[override]
                if method == "POST" and path.endswith("/rules"):
                    self.calls.append((method, path, copy.deepcopy(body)))
                    return rate_limit.APIResponse(200, {"success": True, "result": {}})
                return super().call(method, path, body, allowed_statuses)

        api = SwallowingAPI(ruleset_with({"ref": "somebody_else", "enabled": True}))
        with self.assertRaises(rate_limit.ReconcileError):
            rate_limit.reconcile(api, "apply")


if __name__ == "__main__":
    unittest.main()
