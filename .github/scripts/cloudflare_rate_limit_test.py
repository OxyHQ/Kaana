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


INTERNAL = rate_limit.INTERNAL_INFERENCE_RULE
PUBLIC = rate_limit.PUBLIC_API_RULE
REALTIME = rate_limit.PUBLIC_REALTIME_RULE


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
    def test_the_internal_rule_is_unchanged_until_phase_5(self) -> None:
        self.assertEqual(
            INTERNAL,
            {
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
            },
        )

    def test_the_public_api_rule_is_pinned(self) -> None:
        self.assertEqual(PUBLIC["ref"], "kaana_public_api_per_ip")
        self.assertEqual(
            PUBLIC["expression"],
            '(starts_with(http.request.uri.path, "/v1/") and http.request.uri.path ne "/v1/realtime")',
        )
        self.assertEqual(
            PUBLIC["ratelimit"],
            {
                "characteristics": ["cf.colo.id", "ip.src"],
                "period": 60,
                "requests_per_period": 600,
                "mitigation_timeout": 60,
            },
        )
        self.assertEqual((PUBLIC["action"], PUBLIC["enabled"]), ("block", True))

    def test_realtime_upgrades_have_their_own_stricter_counter(self) -> None:
        self.assertEqual(REALTIME["ref"], "kaana_public_realtime_per_ip")
        self.assertEqual(REALTIME["expression"], '(http.request.uri.path eq "/v1/realtime")')
        self.assertEqual(
            REALTIME["ratelimit"],
            {
                "characteristics": ["cf.colo.id", "ip.src"],
                "period": 60,
                "requests_per_period": 30,
                "mitigation_timeout": 60,
            },
        )
        # Negative control: the general /v1 rule excludes exactly the realtime
        # path, so one upgrade is never counted twice.
        self.assertIn('http.request.uri.path ne "/v1/realtime"', PUBLIC["expression"])
        self.assertLess(
            REALTIME["ratelimit"]["requests_per_period"], PUBLIC["ratelimit"]["requests_per_period"]
        )

    def test_no_owned_rule_names_a_method_or_an_internal_path_it_does_not_own(self) -> None:
        for rule in (PUBLIC, REALTIME):
            self.assertNotIn("http.request.method", rule["expression"])
            self.assertNotIn("/internal/", rule["expression"])
            self.assertNotIn("/livez", rule["expression"])
        self.assertNotIn("/v1/", INTERNAL["expression"].replace("/internal/v1/", ""))

    def test_refs_and_descriptions_are_unique(self) -> None:
        refs = [rule["ref"] for rule in rate_limit.DESIRED_RULES]
        descriptions = [rule["description"] for rule in rate_limit.DESIRED_RULES]
        self.assertEqual(len(set(refs)), len(refs))
        self.assertEqual(len(set(descriptions)), len(descriptions))


class ReconcileTests(unittest.TestCase):
    def test_check_missing_entrypoint_is_read_only(self) -> None:
        api = FakeAPI(None)
        self.assertEqual(rate_limit.reconcile(api, "check"), 2)
        self.assertEqual(methods(api), ["GET", "GET"])

    def test_apply_creates_missing_entrypoint_with_every_rule_and_reads_it_back(self) -> None:
        api = FakeAPI(None)
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual(methods(api), ["GET", "GET", "POST", "GET"])
        self.assertEqual(
            [rule["ref"] for rule in api.ruleset["rules"]],
            [rule["ref"] for rule in rate_limit.DESIRED_RULES],
        )

    def test_check_exact_rules_is_read_only(self) -> None:
        api = FakeAPI(complete())
        self.assertEqual(rate_limit.reconcile(api, "check"), 0)
        self.assertEqual(methods(api), ["GET", "GET"])

    def test_apply_on_exact_rules_writes_nothing(self) -> None:
        api = FakeAPI(complete())
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual(writes(api), [])

    def test_check_reports_the_public_rules_missing_from_todays_state(self) -> None:
        # Production today holds only the internal rule.
        api = FakeAPI(ruleset_with(INTERNAL))
        self.assertEqual(rate_limit.reconcile(api, "check"), 2)
        self.assertEqual(methods(api), ["GET", "GET"])

    def test_apply_appends_the_public_rules_after_existing_ones(self) -> None:
        api = FakeAPI(ruleset_with(INTERNAL, {"ref": "somebody_else", "enabled": True}))
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual(
            writes(api),
            [
                ("POST", "/zones/zone-1/rulesets/ruleset-1/rules"),
                ("POST", "/zones/zone-1/rulesets/ruleset-1/rules"),
            ],
        )
        self.assertEqual(methods(api)[-1], "GET")
        self.assertEqual(
            [rule["ref"] for rule in api.ruleset["rules"]],
            [INTERNAL["ref"], "somebody_else", PUBLIC["ref"], REALTIME["ref"]],
        )
        self.assertEqual(api.ruleset["rules"][1], {"id": "rule-2", "ref": "somebody_else", "enabled": True})

    def test_check_drift_is_read_only(self) -> None:
        drifted = copy.deepcopy(PUBLIC)
        drifted["ratelimit"]["requests_per_period"] = 999
        api = FakeAPI(ruleset_with(INTERNAL, drifted, REALTIME))
        self.assertEqual(rate_limit.reconcile(api, "check"), 2)
        self.assertEqual(methods(api), ["GET", "GET"])

    def test_apply_updates_only_the_drifted_rule_and_reads_it_back(self) -> None:
        drifted = copy.deepcopy(REALTIME)
        drifted["enabled"] = False
        ruleset = ruleset_with(INTERNAL, PUBLIC, drifted)
        ruleset["rules"].append({"id": "other", "ref": "somebody_else", "enabled": True})
        api = FakeAPI(ruleset)
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual(writes(api), [("PATCH", "/zones/zone-1/rulesets/ruleset-1/rules/rule-3")])
        self.assertEqual(methods(api)[-1], "GET")
        self.assertEqual(api.ruleset["rules"][3], {"id": "other", "ref": "somebody_else", "enabled": True})

    def test_legacy_rule_drift_is_still_corrected(self) -> None:
        drifted = copy.deepcopy(INTERNAL)
        drifted["ratelimit"]["requests_per_period"] = 999
        api = FakeAPI(ruleset_with(drifted, PUBLIC, REALTIME))
        self.assertEqual(rate_limit.reconcile(api, "apply"), 0)
        self.assertEqual(writes(api), [("PATCH", "/zones/zone-1/rulesets/ruleset-1/rules/rule-1")])

    def test_duplicate_owned_ref_is_never_mutated(self) -> None:
        ruleset = complete()
        ruleset["rules"].append({"id": "rule-9", **copy.deepcopy(PUBLIC)})
        api = FakeAPI(ruleset)
        with self.assertRaises(rate_limit.ReconcileError):
            rate_limit.reconcile(api, "apply")
        self.assertEqual(methods(api), ["GET", "GET"])

    def test_a_conflict_on_one_rule_refuses_every_write(self) -> None:
        # The realtime description is taken by a manual rule while the public
        # API rule is missing: nothing is created, not even the healthy one.
        conflicting = copy.deepcopy(REALTIME)
        conflicting["ref"] = "manual_rule"
        api = FakeAPI(ruleset_with(INTERNAL, conflicting))
        with self.assertRaises(rate_limit.ReconcileError):
            rate_limit.reconcile(api, "apply")
        self.assertEqual(methods(api), ["GET", "GET"])

    def test_description_without_owned_ref_is_never_duplicated(self) -> None:
        conflicting = copy.deepcopy(INTERNAL)
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

        api = SwallowingAPI(ruleset_with(INTERNAL))
        with self.assertRaises(rate_limit.ReconcileError):
            rate_limit.reconcile(api, "apply")


if __name__ == "__main__":
    unittest.main()
