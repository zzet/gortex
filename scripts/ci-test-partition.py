#!/usr/bin/env python3
"""Run one exhaustive package/test partition without changing test budgets."""

import argparse
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

SHARDS = {"store": 3, "indexer": 6}
NAME_BUNDLE = 32
WINDOWS_COMMAND_BUDGET = 30000
TEST_EXECUTION_BUDGET_SECONDS = 45 * 60
LARGE_PACKAGES = {
    "store": "/internal/graph/store_sqlite",
    "indexer": "/internal/indexer",
    "mcp": "/internal/mcp",
}


def capture(command):
    return subprocess.check_output(command, text=True, encoding="utf-8")


def test_names(output):
    names = []
    for line in output.splitlines():
        if line.startswith(("Test", "Example", "Fuzz")):
            if not line.isidentifier():
                raise ValueError(f"unexpected test census line: {line!r}")
            names.append(line)
    if not names or len(names) != len(set(names)):
        raise ValueError("compiled test census must be nonempty and unique")
    return sorted(names)


def exact_pattern(names):
    # Factoring shared prefixes keeps Windows' process command line below its
    # limit. Anchors apply only to top-level names; Go runs all their subtests.
    trie = {}
    for name in names:
        node = trie
        for char in name:
            node = node.setdefault(char, {})
        node[""] = {}

    def render(node):
        suffixes = {}
        for char, child in sorted(node.items()):
            if char:
                suffixes.setdefault(render(child), []).append(char)
        alternatives = []
        for suffix, chars in suffixes.items():
            prefix = (re.escape(chars[0]) if len(chars) == 1 else
                      "[" + "".join(re.escape(char) for char in chars) + "]")
            alternatives.append(prefix + suffix)
        if not alternatives:
            return ""
        body = alternatives[0] if len(alternatives) == 1 else "(?:" + "|".join(alternatives) + ")"
        return "(?:" + body + ")?" if "" in node else body

    return "^(?:" + render(trie) + ")$"


def plan_shards(names, bundle_size=NAME_BUNDLE, shard_count=3):
    # Small adjacent bundles share enough prefixes for Windows command lines,
    # while round-robin assignment spreads name families across every runner.
    # Capacity limits keep the top-level case counts balanced to one case.
    targets = [len(names) // shard_count + (index < len(names) % shard_count)
               for index in range(shard_count)]
    assignments = [[] for _ in range(shard_count)]
    index = 0
    for start in range(0, len(names), bundle_size):
        bundle = names[start:start + bundle_size]
        while bundle:
            count = min(len(bundle), targets[index] - len(assignments[index]))
            assignments[index].extend(bundle[:count])
            bundle = bundle[count:]
            index = (index + 1) % shard_count
    assignments = [sorted(assignment) for assignment in assignments]
    if any(not assignment for assignment in assignments):
        raise ValueError("every shard must have at least one compiled test")
    patterns = [exact_pattern(assignment) for assignment in assignments]
    # Fail closed on omissions, overlaps, or a regex that selects a different
    # shard. This also covers Example functions and Fuzz seed corpora.
    for name in names:
        matches = [index for index, pattern in enumerate(patterns)
                   if re.fullmatch(pattern, name)]
        expected = [index for index, assignment in enumerate(assignments)
                    if name in assignment]
        if len(matches) != 1 or matches != expected:
            raise ValueError(f"non-exhaustive or overlapping shard assignment: {name}")
    return assignments, patterns


def command_units(command):
    return len(subprocess.list2cmdline(command).encode("utf-16-le")) // 2


def planned_command_units(command):
    # Leave room for the longer remaining-budget flag used by split commands.
    return command_units(["-timeout=2700.000s" if arg == "-timeout=45m" else arg
                          for arg in command])


def plan_shard_commands(names, census_prefix, test_prefix, package, windows_api=None, shard_count=3):
    # Keep existing membership when it fits. Larger adjacent bundles retain
    # exact coverage and balance while factoring more shared name prefixes.
    if windows_api is None:
        windows_api = os.name == "nt"
    bundle_size = NAME_BUNDLE
    while True:
        assignments, patterns = plan_shards(names, bundle_size, shard_count)
        commands = [prefix + [flag, pattern, package]
                    for pattern in patterns
                    for prefix, flag in ((census_prefix, "-list"), (test_prefix, "-run"))]
        if not windows_api or all(planned_command_units(command) <= WINDOWS_COMMAND_BUDGET
                                  for command in commands):
            return assignments, [[pattern] for pattern in patterns], bundle_size
        if bundle_size >= len(names):
            # Preserve this balanced assignment. Splitting invocations does not
            # move tests between runners or increase their execution budget.
            chunks = [bounded_patterns(assignment, census_prefix, test_prefix, package)
                      for assignment in assignments]
            return assignments, chunks, bundle_size
        bundle_size = min(bundle_size * 2, len(names))


def bounded_patterns(names, census_prefix, test_prefix, package):
    pattern = exact_pattern(names)
    commands = [prefix + [flag, pattern, package]
                for prefix, flag in ((census_prefix, "-list"), (test_prefix, "-run"))]
    if all(planned_command_units(command) <= WINDOWS_COMMAND_BUDGET for command in commands):
        return [pattern]
    if len(names) == 1:
        raise ValueError("one compiled test cannot fit Windows command budget: " + names[0])
    middle = len(names) // 2
    return (bounded_patterns(names[:middle], census_prefix, test_prefix, package)
            + bounded_patterns(names[middle:], census_prefix, test_prefix, package))


def write_manifest(path, manifest):
    path.write_text(json.dumps(manifest, indent=2) + "\n")


def run_commands(commands, manifest, manifest_path):
    # Only oversized Windows shards have multiple commands. One deadline covers
    # all chunks, including Go startup between them; no chunk receives a new 45m.
    deadline = time.monotonic() + TEST_EXECUTION_BUDGET_SECONDS
    manifest["executed_commands"] = []
    try:
        for command in commands:
            actual = list(command)
            remaining = deadline - time.monotonic()
            if len(commands) > 1:
                if remaining <= 0:
                    raise ValueError("aggregate test execution budget exhausted")
                actual[actual.index("-timeout=45m")] = f"-timeout={remaining:.3f}s"
            units = guard_command(actual)
            manifest["executed_commands"].append({"command": actual, "utf16_units": units})
            write_manifest(manifest_path, manifest)
            if len(commands) > 1:
                subprocess.run(actual, check=True, timeout=remaining)
            else:
                subprocess.run(actual, check=True)
    finally:
        write_manifest(manifest_path, manifest)


def guard_command(command):
    # Reserve room for Go's own test-binary flags and path below CreateProcess's
    # 32,767 UTF-16 code-unit limit on Windows. Other platforms do not use
    # CreateProcess. Never drop names to make an argument fit.
    units = command_units(command)
    if os.name == "nt" and units > WINDOWS_COMMAND_BUDGET:
        raise ValueError(f"factored test command exceeds Windows budget: {units}")
    return units


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--partition", default=os.environ.get("TEST_PARTITION"), required=False)
    parser.add_argument("--dry-run", action="store_true")
    parser.add_argument("--manifest", type=Path, default=Path("test-selection.json"))
    args = parser.parse_args()
    valid = {"remaining", "mcp"} | {f"{package}-{index}" for package, count in SHARDS.items() for index in range(count)}
    if args.partition not in valid:
        parser.error(f"partition must be one of {sorted(valid)}")
    windows = os.environ.get("TEST_WINDOWS", "false") == "true"
    module = capture(["go", "list", "-m"]).strip()
    packages = capture(["go", "list", "./..."]).splitlines()
    if not packages or len(packages) != len(set(packages)):
        raise ValueError("package census must be nonempty and unique")
    package_partitions = {package: next((name for name, suffix in LARGE_PACKAGES.items()
                                        if package == module + suffix), "remaining")
                          for package in packages}
    family = args.partition.split("-", 1)[0]
    selected = [package for package, partition in package_partitions.items() if partition == family]
    if not selected:
        raise ValueError(f"no packages selected for {args.partition}")
    manifest = {"partition": args.partition, "packages": package_partitions, "selected_packages": selected}
    command = ["go", "test"]
    if windows:
        command += ["-v", "-timeout=45m"]
    else:
        command += ["-race", "-timeout=45m", "-coverprofile=coverage.out"]
    if family in ("store", "indexer"):
        if len(selected) != 1:
            raise ValueError("a test shard must own exactly one package")
        census_command = ["go", "test"] + ([] if windows else ["-race"]) + ["-list", ".", selected[0]]
        names = test_names(capture(census_command))
        assignments, pattern_chunks, bundle_size = plan_shard_commands(
            names, census_command[:-3], command, selected[0], shard_count=SHARDS[family])
        # Verify the Python-generated expressions with Go's own test matcher.
        # The per-platform compiled census is the authority for every shard.
        for index, chunks in enumerate(pattern_chunks):
            matched = []
            for pattern in chunks:
                guard_command(census_command[:-3] + ["-list", pattern, selected[0]])
                guard_command(command + ["-run", pattern, selected[0]])
                matched.extend(test_names(capture(census_command[:-3] + ["-list", pattern, selected[0]])))
            if sorted(matched) != assignments[index] or len(matched) != len(set(matched)):
                raise ValueError(f"Go matcher disagrees with shard {index}")
        index = int(args.partition.rsplit("-", 1)[1])
        commands = [command + ["-run", pattern] + selected for pattern in pattern_chunks[index]]
        manifest.update(census=names, shards=assignments, pattern_chunks=pattern_chunks,
                        name_bundle=bundle_size, shard_count=SHARDS[family])
        if all(len(chunks) == 1 for chunks in pattern_chunks):
            manifest["patterns"] = [chunks[0] for chunks in pattern_chunks]
        print(f"{selected[0]}: {len(names)} compiled tests/examples/fuzz seeds; "
              f"shards={[len(shard) for shard in assignments]}; selected={index}", flush=True)
    else:
        commands = [command + selected]
    units = [guard_command(command) for command in commands]
    manifest["commands"] = commands
    manifest["commands_utf16_units"] = units
    if len(commands) > 1:
        manifest["aggregate_test_execution_budget_seconds"] = TEST_EXECUTION_BUDGET_SECONDS
    if len(commands) == 1:
        manifest["command"] = commands[0]
        manifest["command_utf16_units"] = units[0]
    write_manifest(args.manifest, manifest)
    print(f"Running {len(selected)} packages in {args.partition}; command units={units}", flush=True)
    if not args.dry_run:
        run_commands(commands, manifest, args.manifest)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        print(f"CI test partition failed: {error}", file=sys.stderr)
        sys.exit(1)
