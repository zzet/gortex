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

    def test_family_counts_preserve_exhaustive_windows_and_race_plans(self):
        names = self.names(91) + ["ExampleFamily", "FuzzFamily"]
        names.sort()
        self.assertEqual(planner.SHARDS, {"store": 6, "indexer": 6, "mcp": 3})
        for family, count in planner.SHARDS.items():
            for windows in (False, True):
                with self.subTest(family=family, windows=windows):
                    prefix = ["go", "test"] + (["-v"] if windows else ["-race"])
                    with mock.patch.object(planner, "WINDOWS_COMMAND_BUDGET", 650):
                        assignments, chunks, _ = planner.plan_shard_commands(
                            names, prefix, prefix + ["-timeout=45m"], "package",
                            windows_api=windows, shard_count=count)
                    self.assertEqual(len(assignments), count)
                    self.assertEqual(sorted(name for shard in assignments for name in shard), names)
                    self.assertLessEqual(max(map(len, assignments)) - min(map(len, assignments)), 1)
                    for name in names:
                        self.assertEqual(sum(bool(re.fullmatch(pattern, name))
                                             for shard in chunks for pattern in shard), 1)
        workflow = (Path(__file__).parents[1] / ".github/workflows/ci.yml").read_text()
        for family, count in planner.SHARDS.items():
            for index in range(count):
                self.assertIn(f"{family}-{index}", workflow)
            self.assertNotIn(f"{family}-{count}", workflow)
        self.assertIn("timeout-minutes: 60", workflow)
        self.assertEqual(planner.TEST_EXECUTION_BUDGET_SECONDS, 2700)

    def test_plain_platform_layout_matches_the_workflow(self):
        workflow = (Path(__file__).parents[1] / ".github/workflows/ci.yml").read_text()
        rows = re.findall(r"(?m)^          - \{os: (\w+)-latest, go-version: '1.27', partition: ([\w-]+)\}$",
                          workflow)
        self.assertEqual({os_name for os_name, _ in rows}, {"macos", "windows"})
        # Linux runs every race shard; the plain rows reuse some of the names.
        linux = re.search(r"(?m)^        partition: \[(.*)\]$", workflow).group(1).split(", ")
        self.assertEqual(linux, ["remaining"] + [f"{family}-{index}" for family, count in planner.SHARDS.items()
                                                 for index in range(count)])
        self.assertEqual(set(planner.PLAIN_SHARDS), {"macos", "windows"})
        for platform, shards in planner.PLAIN_SHARDS.items():
            expected = {"remaining", "mcp"} | {f"{family}-{index}" for family, count in shards.items()
                                                for index in range(count)}
            with self.subTest(platform=platform):
                listed = [partition for os_name, partition in rows if os_name == platform]
                self.assertEqual(sorted(listed), sorted(expected))
        self.assertLessEqual(sum(os_name == "macos" for os_name, _ in rows), 5)
        self.assertIn("TEST_RACE: ${{ matrix.os == 'ubuntu-latest' }}", workflow)
        self.assertIn("os: [ubuntu-latest]\n", workflow)

    def run_dry(self, partition, environ, dry_run=True):
        module = "example.org/gortex"
        packages = [module + suffix for suffix in ("/cmd/gortex", "/internal/graph/store_sqlite",
                                                   "/internal/indexer", "/internal/mcp")]
        names = self.names(12)

        def capture(command):
            if command == ["go", "list", "-m"]:
                return module + "\n"
            if command == ["go", "list", "./..."]:
                return "\n".join(packages) + "\n"
            pattern = command[command.index("-list") + 1]
            return "\n".join(name for name in names if pattern == "." or re.fullmatch(pattern, name))

        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "selection.json"
            with mock.patch.object(planner, "capture", side_effect=capture) as captured, \
                    mock.patch.dict(planner.os.environ, environ, clear=True), \
                    mock.patch.object(planner.sys, "argv", ["partition", "--partition", partition,
                                                           "--manifest", str(path)] + ["--dry-run"] * dry_run), \
                    mock.patch.object(planner.subprocess, "run") as run:
                planner.main()
            if dry_run:
                run.assert_not_called()
                return json.loads(path.read_text()), captured.call_args_list
            return json.loads(path.read_text()), run.call_args_list

    def test_plain_platforms_run_without_race_or_coverage(self):
        for environ, partition, count in (({"TEST_RACE": "false"}, "indexer-2", 3),
                                          ({"TEST_WINDOWS": "true"}, "indexer-3", 4)):
            with self.subTest(environ=environ):
                manifest, calls = self.run_dry(partition, environ)
                self.assertEqual(manifest["shard_count"], count)
                for command in manifest["commands"]:
                    self.assertNotIn("-race", command)
                    self.assertNotIn("-coverprofile=coverage.out", command)
                    self.assertEqual("-v" in command, "TEST_WINDOWS" in environ)
                self.assertTrue(all("-race" not in call.args[0] for call in calls))
        for environ, verbose in (({"TEST_RACE": "false"}, []), ({"TEST_WINDOWS": "true"}, ["-v"])):
            with self.subTest(environ=environ):
                manifest, _ = self.run_dry("remaining", environ)
                self.assertEqual(manifest["selected_packages"], [
                    "example.org/gortex/cmd/gortex", "example.org/gortex/internal/graph/store_sqlite"])
                # store_sqlite runs alone, after the other packages.
                self.assertEqual(manifest["commands"], [
                    ["go", "test"] + verbose + ["-timeout=45m", "example.org/gortex/cmd/gortex"],
                    ["go", "test"] + verbose + ["-timeout=45m", "example.org/gortex/internal/graph/store_sqlite"]])
                self.assertEqual(manifest["aggregate_test_execution_budget_seconds"], 2700)
        manifest, _ = self.run_dry("remaining", {})
        self.assertEqual(manifest["command"], ["go", "test", "-race", "-timeout=45m", "-coverprofile=coverage.out",
                                               "example.org/gortex/cmd/gortex"])
        manifest, _ = self.run_dry("mcp", {"TEST_RACE": "false"})
        self.assertEqual(manifest["command"], ["go", "test", "-timeout=45m",
                                               "example.org/gortex/internal/mcp"])
        for environ in ({}, {"TEST_RACE": "true"}):
            with self.subTest(environ=environ):
                manifest, _ = self.run_dry("indexer-5", environ)
                self.assertIn("-race", manifest["command"])
                self.assertIn("-coverprofile=coverage.out", manifest["command"])
                self.assertEqual(manifest["shard_count"], 6)

    def test_plain_remaining_groups_share_the_budget_and_go_times_out_first(self):
        with mock.patch.object(planner.time, "monotonic", side_effect=[100, 100, 700]):
            manifest, runs = self.run_dry("remaining", {"TEST_RACE": "false"}, dry_run=False)
        self.assertEqual([call.args[0][-1] for call in runs], [command[-1] for command in manifest["commands"]])
        self.assertIn("-timeout=2700.000s", runs[0].args[0])
        self.assertIn("-timeout=2100.000s", runs[1].args[0])
        self.assertEqual([call.kwargs["timeout"] for call in runs], [3000, 2400])
        # A single command keeps Go's plain 45m and no process timeout.
        manifest, runs = self.run_dry("remaining", {}, dry_run=False)
        self.assertEqual([call.args for call in runs], [(manifest["command"],)])
        self.assertEqual(runs[0].kwargs, {"check": True})

    def test_package_group_failure_still_runs_later_groups(self):
        commands = [["go", "test", "-timeout=45m", name] for name in ("A", "B")]
        failure = subprocess.CalledProcessError(1, commands[0])
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "selection.json"
            with mock.patch.object(planner.time, "monotonic", side_effect=[0, 0, 60]), \
                    mock.patch.object(planner.subprocess, "run", side_effect=[failure, None]) as run:
                with self.assertRaises(subprocess.CalledProcessError) as raised:
                    planner.run_package_groups(commands, {}, path)
            self.assertIs(raised.exception, failure)
            self.assertEqual(run.call_count, 2)
            self.assertEqual(len(json.loads(path.read_text())["executed_commands"]), 2)
        # When the budget runs out after a failure, the failure is reported.
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(planner.time, "monotonic", side_effect=[0, 0, 2700]), \
                mock.patch.object(planner.subprocess, "run", side_effect=[failure]):
            with self.assertRaises(subprocess.CalledProcessError) as raised:
                planner.run_package_groups(commands, {}, Path(directory) / "selection.json")
            self.assertIs(raised.exception, failure)

    def test_partition_and_race_settings_are_checked(self):
        for environ, partition in (({"TEST_RACE": "false"}, "store-0"),
                                   ({"TEST_RACE": "false"}, "indexer-3"),
                                   ({"TEST_WINDOWS": "true"}, "indexer-4"),
                                   ({}, "mcp"),
                                   ({"TEST_RACE": "False"}, "remaining"),
                                   ({"TEST_WINDOWS": "true", "TEST_RACE": "true"}, "remaining")):
            with self.subTest(environ=environ, partition=partition), \
                    mock.patch.dict(planner.os.environ, environ, clear=True), \
                    mock.patch.object(planner.sys, "argv", ["partition", "--partition", partition]), \
                    mock.patch.object(planner, "capture") as capture, \
                    mock.patch("sys.stderr"), self.assertRaises(SystemExit):
                planner.main()
            capture.assert_not_called()

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
                                      lambda *args, **kwargs: original_plan(*args, windows_api=True, **kwargs)), \
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
        names = ["TestA", "TestB", "TestC", "TestD", "TestE", "TestF"]
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
