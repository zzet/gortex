import importlib.util
import json
from pathlib import Path
import random
import re
import subprocess
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "ci_test_partition", Path(__file__).with_name("ci-test-partition.py"))
planner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(planner)


class PartitionTests(unittest.TestCase):
    def names(self, count=90):
        randomizer = random.Random(806)
        return sorted("Test" + "".join(randomizer.choices("ABCDEFGHIJKLMNOPQRSTUVWXYZ", k=40))
                      for _ in range(count))

    def test_oversized_windows_chunks_preserve_exact_balanced_assignment(self):
        names = self.names() + ["ExampleWindow", "FuzzWindow"]
        names.sort()
        census = ["C:\\Program Files\\Go 😀\\go.exe", "test"]
        tests = census + ["-v", "-timeout=45m"]
        package = "example.org/組織/internal/indexer"
        with mock.patch.object(planner, "WINDOWS_COMMAND_BUDGET", 650):
            assignments, chunks, bundle = planner.plan_shard_commands(
                names, census, tests, package, windows_api=True)
            original, _ = planner.plan_shards(names, bundle)
            self.assertEqual(assignments, original)
            self.assertTrue(any(len(shard) > 1 for shard in chunks))
            self.assertLessEqual(max(map(len, assignments)) - min(map(len, assignments)), 1)
            for index, shard in enumerate(chunks):
                selected = [name for pattern in shard for name in names
                            if re.fullmatch(pattern, name)]
                self.assertEqual(sorted(selected), assignments[index])
                self.assertEqual(len(selected), len(set(selected)))
                for pattern in shard:
                    for prefix, flag in ((census, "-list"), (tests, "-run")):
                        self.assertLessEqual(planner.planned_command_units(
                            prefix + [flag, pattern, package]), 650)
            for name in names:
                self.assertEqual(sum(bool(re.fullmatch(pattern, name))
                                     for shard in chunks for pattern in shard), 1)

    def test_non_windows_keeps_one_command_and_original_membership(self):
        names = self.names()
        prefix = ["go", "test", "-race", "-timeout=45m", "-coverprofile=coverage.out"]
        with mock.patch.object(planner, "WINDOWS_COMMAND_BUDGET", 1):
            assignments, chunks, bundle = planner.plan_shard_commands(
                names, ["go", "test", "-race"], prefix, "package", windows_api=False)
        original, patterns = planner.plan_shards(names)
        self.assertEqual(assignments, original)
        self.assertEqual(chunks, [[pattern] for pattern in patterns])
        self.assertEqual(bundle, planner.NAME_BUNDLE)

    def test_single_test_overflow_fails_without_dropping_it(self):
        with mock.patch.object(planner, "WINDOWS_COMMAND_BUDGET", 10):
            with self.assertRaisesRegex(ValueError, "one compiled test cannot fit"):
                planner.bounded_patterns(["TestOne"], ["go", "test"], ["go", "test"], "package")

    def test_units_count_quoted_utf16_not_characters(self):
        command = ["C:\\Go 😀\\go.exe", "test", "組織"]
        serialized = subprocess.list2cmdline(command)
        self.assertEqual(planner.command_units(command), len(serialized.encode("utf-16-le")) // 2)
        self.assertGreater(planner.command_units(command), len(serialized))

    def test_execution_chunks_share_deadline_and_record_effective_commands(self):
        commands = [["go", "test", "-timeout=45m", "-run", pattern, "package"]
                    for pattern in ("TestA", "TestB")]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "selection.json"
            manifest = {"commands": commands}
            with mock.patch.object(planner.time, "monotonic", side_effect=[100, 100, 700]), \
                    mock.patch.object(planner.subprocess, "run") as run:
                planner.run_commands(commands, manifest, path)
            self.assertEqual(run.call_args_list[0].kwargs["timeout"], 2700)
            self.assertEqual(run.call_args_list[1].kwargs["timeout"], 2100)
            self.assertIn("-timeout=2100.000s", run.call_args_list[1].args[0])
            written = json.loads(path.read_text())
            self.assertEqual(len(written["executed_commands"]), 2)
            self.assertEqual(commands[1][2], "-timeout=45m")

    def test_exhausted_deadline_does_not_start_another_chunk(self):
        commands = [["go", "test", "-timeout=45m", name] for name in ("A", "B")]
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.object(planner.time, "monotonic", side_effect=[0, 0, 2700]), \
                    mock.patch.object(planner.subprocess, "run") as run:
                with self.assertRaisesRegex(ValueError, "aggregate test execution budget"):
                    planner.run_commands(commands, {}, Path(directory) / "selection.json")
            self.assertEqual(run.call_count, 1)

    def test_chunk_failure_does_not_run_later_chunks(self):
        commands = [["go", "test", "-timeout=45m", name] for name in ("A", "B")]
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.object(planner.subprocess, "run", side_effect=subprocess.TimeoutExpired("go", 1)) as run:
                with self.assertRaises(subprocess.TimeoutExpired):
                    planner.run_commands(commands, {}, Path(directory) / "selection.json")
            self.assertEqual(run.call_count, 1)

    def test_single_command_preserves_non_windows_coverage_invocation(self):
        command = ["go", "test", "-race", "-timeout=45m", "-coverprofile=coverage.out", "package"]
        with tempfile.TemporaryDirectory() as directory:
            with mock.patch.object(planner.subprocess, "run") as run:
                planner.run_commands([command], {}, Path(directory) / "selection.json")
            run.assert_called_once_with(command, check=True)

    def test_main_verifies_each_go_chunk_and_manifests_all_commands(self):
        names = self.names()
        package = "example.org/gortex/internal/indexer"
        census_calls = []

        def capture(command):
            if command == ["go", "list", "-m"]:
                return "example.org/gortex\n"
            if command == ["go", "list", "./..."]:
                return package + "\n"
            self.assertIn("-list", command)
            pattern = command[command.index("-list") + 1]
            census_calls.append(pattern)
            matched = names if pattern == "." else [name for name in names if re.fullmatch(pattern, name)]
            return "\n".join(matched) + "\nok\t" + package

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "selection.json"
            original_plan = planner.plan_shard_commands
            with mock.patch.object(planner, "capture", side_effect=capture), \
                    mock.patch.object(planner, "WINDOWS_COMMAND_BUDGET", 650), \
                    mock.patch.object(planner, "plan_shard_commands", side_effect=
                                      lambda *args: original_plan(*args, windows_api=True)), \
                    mock.patch.dict(planner.os.environ, {"TEST_WINDOWS": "true"}), \
                    mock.patch.object(planner.sys, "argv", ["partition", "--partition", "indexer-1",
                                                           "--dry-run", "--manifest", str(path)]), \
                    mock.patch.object(planner.subprocess, "run") as run:
                planner.main()
            manifest = json.loads(path.read_text())
            chunks = manifest["pattern_chunks"]
            self.assertEqual(len(census_calls), 1 + sum(map(len, chunks)))
            self.assertEqual(len(manifest["commands"]), len(chunks[1]))
            self.assertGreater(len(manifest["commands"]), 1)
            self.assertTrue(all(units <= 650 for units in manifest["commands_utf16_units"]))
            self.assertEqual(manifest["aggregate_test_execution_budget_seconds"], 2700)
            run.assert_not_called()

    def test_main_refuses_duplicate_go_matcher_results_before_execution(self):
        names = ["TestA", "TestB", "TestC"]
        package = "example.org/gortex/internal/indexer"

        def capture(command):
            if command == ["go", "list", "-m"]:
                return "example.org/gortex"
            if command == ["go", "list", "./..."]:
                return package
            pattern = command[command.index("-list") + 1]
            if pattern == ".":
                return "\n".join(names)
            return "TestA\nTestA"

        with mock.patch.object(planner, "capture", side_effect=capture), \
                mock.patch.object(planner.sys, "argv", ["partition", "--partition", "indexer-0"]), \
                mock.patch.object(planner.subprocess, "run") as run:
            with self.assertRaisesRegex(ValueError, "nonempty and unique"):
                planner.main()
        run.assert_not_called()


if __name__ == "__main__":
    unittest.main()
