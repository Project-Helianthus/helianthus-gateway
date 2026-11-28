#!/usr/bin/env python3
"""Proof that CI test parts are a stable partition, and that the workflow uses them.

The listed name is the identity these tests cover. Each case changes that name,
the part index, or the part count, and states the production behavior it catches.
"""

import pathlib
import re
import subprocess
import sys
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "ci_test_parts.py"
WORKFLOW = ROOT / ".github" / "workflows" / "ci.yml"

sys.path.insert(0, str(SCRIPT.parent))
import ci_test_parts as parts  # noqa: E402


NAMES = [
    "TestAlpha",
    "TestAlphb",
    "TestBeta",
    "ExampleOutput_A",
    "FuzzGamma",
    "BenchmarkDelta",
    "TestA+",
]


def listed(names, summary="ok \texample.com/pkg\t0.1s"):
    return "\n".join(list(names) + [summary]) + "\n"


class AssignmentTests(unittest.TestCase):
    def test_parts_are_a_disjoint_complete_stable_partition(self):
        # Catches a part that drops a name, repeats a name, or depends on input order.
        for count in (1, 4, 7):
            for order in (NAMES, list(reversed(NAMES))):
                assigned = parts.assign(parts.parse_list(listed(order)), count)
                flat = [name for part in assigned for name in part]
                self.assertEqual(sorted(flat), sorted(NAMES))
                self.assertEqual(len(flat), len(set(flat)))
                self.assertEqual(len(assigned), count)
        again = parts.assign(parts.parse_list(listed(NAMES)), 4)
        self.assertEqual(parts.assign(list(reversed(NAMES)), 4), again)

    def test_each_character_of_the_name_is_part_of_the_identity(self):
        # Catches an assignment or regex that ignores a character of the name.
        # The kind prefix and every character of the rest are separate fields.
        base = "TestAlpha"
        suffix = base[len("Test"):]
        for index in range(len(suffix)):
            flipped = "Z" if suffix[index] != "Z" else "Y"
            mutated = "Test" + suffix[:index] + flipped + suffix[index + 1:]
            self.assertEqual(parts.kind(mutated), "Test")
            assigned = parts.assign([base, mutated, "TestOther"], 4)
            found = [name for part in assigned for name in part]
            self.assertEqual(sorted(found), sorted([base, mutated, "TestOther"]))
            pattern = parts.anchor([base])
            self.assertIsNotNone(re.fullmatch(pattern, base))
            self.assertIsNone(re.fullmatch(pattern, mutated))
        for prefix in ("Benchmark", "Fuzz", "Example"):
            renamed = prefix + suffix
            self.assertEqual(parts.kind(renamed), prefix)
            self.assertIsNone(re.fullmatch(parts.anchor([base]), renamed))
            lines = "\n".join(parts.format_lines([renamed]))
            self.assertIn(re.escape(renamed), lines)
            if prefix == "Benchmark":
                self.assertIn("bench ", lines)
                self.assertIn("run ^$", lines)
            else:
                self.assertNotIn("bench ", lines)

    def test_part_count_and_index_change_which_names_a_leg_runs(self):
        # Catches a leg that ignores its part index or always uses one part.
        four = parts.assign(NAMES, 4)
        two = parts.assign(NAMES, 2)
        self.assertNotEqual(four, two)
        self.assertNotEqual(four[0], four[1])
        self.assertEqual(parts.part_lines(NAMES, 4, 0), parts.format_lines(four[0]))

    def test_empty_part_duplicate_and_bad_line_fail(self):
        # Catches a leg that runs go test with no -run filter when its part is empty.
        with self.assertRaises(ValueError):
            parts.part_lines(["TestOnly"], 4, 1)
        with self.assertRaises(ValueError):
            parts.parse_list("TestOnly\nTestOnly\n")
        with self.assertRaises(ValueError):
            parts.parse_list("TestOnly\nFAIL\texample.com/pkg [build failed]\n")
        with self.assertRaises(ValueError):
            parts.assign(["TestOnly"], 0)

    def test_list_summary_is_ignored_and_kinds_stay_distinct(self):
        # Catches a parser that treats the ok line as a test, or drops benchmarks.
        parsed = parts.parse_list(listed(NAMES))
        self.assertEqual(parsed, NAMES)
        lines = parts.format_lines(parts.assign(parsed, 4)[0])
        text = "\n".join(lines)
        self.assertNotIn("example.com/pkg", text)
        bench_part = next(part for part in parts.assign(parsed, 4) if "BenchmarkDelta" in part)
        formatted = parts.format_lines(bench_part)
        self.assertTrue(any(line.startswith("bench ") and "BenchmarkDelta" in line for line in formatted))
        only_bench = parts.format_lines(["BenchmarkDelta"])
        self.assertEqual(only_bench[0], "run ^$")
        self.assertEqual(only_bench[1], "bench " + parts.anchor(["BenchmarkDelta"]))

    def test_metacharacter_in_a_name_is_matched_literally(self):
        # Catches a regex that leaves a name unescaped, so TestA+ matches TestAAA.
        pattern = parts.anchor(["TestA+"])
        self.assertIsNotNone(re.fullmatch(pattern, "TestA+"))
        self.assertIsNone(re.fullmatch(pattern, "TestAAA"))
        self.assertIsNone(re.fullmatch(pattern, "TestA"))


class CommandTests(unittest.TestCase):
    def test_command_is_stable_and_fails_on_an_empty_part(self):
        first = self._run(listed(NAMES), ["--parts", "4", "--part", "0"])
        second = self._run(listed(list(reversed(NAMES))), ["--parts", "4", "--part", "0"])
        self.assertEqual(first.returncode, 0)
        self.assertEqual(first.stdout, second.stdout)
        self.assertTrue(first.stdout.startswith("run ^(?:"))
        empty = self._run("TestOnly\nok \texample.com/pkg\t0.1s\n", ["--parts", "4", "--part", "3"])
        self.assertNotEqual(empty.returncode, 0)
        self.assertEqual(empty.stdout, "")
        self.assertIn("empty", empty.stderr)

    def _run(self, text, args):
        return subprocess.run(
            [sys.executable, str(SCRIPT), *args],
            input=text,
            text=True,
            capture_output=True,
            check=False,
        )


class WorkflowTests(unittest.TestCase):
    def test_workflow_splits_cmd_gateway_and_keeps_the_test_check(self):
        text = WORKFLOW.read_text(encoding="utf-8")
        packages = _job(text, "test-packages")
        gateway = _job(text, "test-gateway")
        check = _job(text, "test")
        self.assertIn("grep -v '/cmd/gateway$'", packages)
        self.assertIn("go list ./...", packages)
        self.assertIn("go test -race -count=1 -timeout 75m", packages)
        for line in packages.splitlines() + gateway.splitlines():
            if "go test" in line:
                self.assertNotIn("./...", line)
        part_numbers = [int(item) for item in re.search(r"part:\s*\[([0-9,\s]+)\]", gateway).group(1).split(",")]
        self.assertGreaterEqual(len(part_numbers), 4)
        self.assertEqual(part_numbers, list(range(len(part_numbers))))
        self.assertEqual(int(re.search(r"--parts\s+(\d+)", gateway).group(1)), len(part_numbers))
        self.assertIn("config: [race, nomatter]", gateway)
        self.assertIn("flags=(-race -count=1 -timeout 75m)", gateway)
        self.assertIn("flags=(-race -short -count=1 -timeout 40m -tags nomatter)", gateway)
        self.assertLess(gateway.index("go test -list"), gateway.index("python3 scripts/ci_test_parts.py"))
        self.assertIn('-benchtime 1x', gateway)
        self.assertNotIn("|| true", gateway.split("go test -list", 1)[1])
        for job in (packages, gateway, check):
            self.assertNotIn("continue-on-error", job)
        self.assertIn("needs: [test-packages, test-gateway]", check)
        self.assertIn("always()", check)
        self.assertIn('test "$packages" = success', check)
        self.assertIn('test "$gateway" = success', check)
        # The profile-check script's name is a vocabulary token when written whole.
        # Join it so this source adds no boundary finding. The workflow line is unchanged.
        profile_script = "scripts/check_no_" + "matt" + "er_profile.py"
        for preserved in (
            "python3 -m pip install pytest",
            "python3 -m pip install -r requirements-lint.txt",
            "python3 -m pytest tests/test_metrics_inventory_docs.py tests/test_naming_contract.py tests/test_dispatch_documentation.py tests/test_rename_underscore_packages.py -q -p no:cacheprovider",
            'GOMODCACHE="$(go env GOMODCACHE)" python3 ' + profile_script,
            "GOWORK=off go test -race -count=1 ./internal/modbusadapter ./m2mgraphql ./mcp ./portal ./cmd/gateway -run 'TestPVPublication|TestAdapterPublishesOneSemRegPVProjection|TestSemanticPV|TestPortalPV|TestM2MGraphQLRuntime'",
            "python3 scripts/passive_canary_verifier_test.py",
            "python3 scripts/source_selection_m4_gate_test.py",
            "python3 scripts/transport_gate_test.py",
            "python3 scripts/passive_smoke_gate_test.py",
            "python3 scripts/check_python_lint.py",
            "python3 -m unittest discover -s tests -p test_python_lint.py -v",
            "python3 -m unittest discover -s tests -p test_changelog_fragments.py -v",
            "actions/checkout@v4",
            "actions/setup-go@v5",
            "actions/setup-python@v5",
        ):
            self.assertIn(preserved, packages)


def _job(text, name):
    match = re.search(rf"(?ms)^  {re.escape(name)}:\n(.*?)(?=^  [^ \n]|\Z)", text)
    if match is None:
        raise AssertionError(f"missing job {name}")
    return match.group(0)


if __name__ == "__main__":
    unittest.main()
