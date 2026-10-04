#!/usr/bin/env python3
"""Run one exhaustive package/test partition without changing test budgets."""

import argparse
import json
import os
import re
import subprocess
import sys
from pathlib import Path

SHARDS = 3
NAME_BUNDLE = 32
WINDOWS_COMMAND_BUDGET = 30000
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


def plan_shards(names, bundle_size=NAME_BUNDLE):
    # Small adjacent bundles share enough prefixes for Windows command lines,
    # while round-robin assignment spreads name families across every runner.
    # Capacity limits keep the top-level case counts balanced to one case.
    targets = [len(names) // SHARDS + (index < len(names) % SHARDS)
               for index in range(SHARDS)]
    assignments = [[] for _ in range(SHARDS)]
    index = 0
    for start in range(0, len(names), bundle_size):
        bundle = names[start:start + bundle_size]
        while bundle:
            count = min(len(bundle), targets[index] - len(assignments[index]))
            assignments[index].extend(bundle[:count])
            bundle = bundle[count:]
            index = (index + 1) % SHARDS
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


def plan_shard_commands(names, census_prefix, test_prefix, package, windows_api=None):
    # Keep existing membership when it fits. Larger adjacent bundles retain
    # exact coverage and balance while factoring more shared name prefixes.
    if windows_api is None:
        windows_api = os.name == "nt"
    bundle_size = NAME_BUNDLE
    while True:
        assignments, patterns = plan_shards(names, bundle_size)
        commands = [prefix + [flag, pattern, package]
                    for pattern in patterns
                    for prefix, flag in ((census_prefix, "-list"), (test_prefix, "-run"))]
        if not windows_api or all(command_units(command) <= WINDOWS_COMMAND_BUDGET
                                  for command in commands):
            return assignments, patterns, bundle_size
        if bundle_size >= len(names):
            raise ValueError("no balanced factored shard plan fits Windows budget: "
                             + str(max(map(command_units, commands))))
        bundle_size = min(bundle_size * 2, len(names))


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
    valid = {"remaining", "mcp"} | {f"{package}-{index}" for package in ("store", "indexer") for index in range(SHARDS)}
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
        assignments, patterns, bundle_size = plan_shard_commands(
            names, census_command[:-3], command, selected[0])
        # Verify the Python-generated expressions with Go's own test matcher.
        # The per-platform compiled census is the authority for all three.
        for pattern in patterns:
            guard_command(census_command[:-3] + ["-list", pattern, selected[0]])
            guard_command(command + ["-run", pattern, selected[0]])
        for index, pattern in enumerate(patterns):
            matched = test_names(capture(census_command[:-3] + ["-list", pattern, selected[0]]))
            if matched != assignments[index]:
                raise ValueError(f"Go matcher disagrees with shard {index}")
        index = int(args.partition.rsplit("-", 1)[1])
        command += ["-run", patterns[index]]
        manifest.update(census=names, shards=assignments, patterns=patterns,
                        name_bundle=bundle_size)
        print(f"{selected[0]}: {len(names)} compiled tests/examples/fuzz seeds; "
              f"shards={[len(shard) for shard in assignments]}; selected={index}", flush=True)
    command += selected
    command_units = guard_command(command)
    manifest["command"] = command
    manifest["command_utf16_units"] = command_units
    args.manifest.write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"Running {len(selected)} packages in {args.partition}; command units={command_units}", flush=True)
    if not args.dry_run:
        subprocess.run(command, check=True)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, subprocess.CalledProcessError) as error:
        print(f"CI test partition failed: {error}", file=sys.stderr)
        sys.exit(1)
