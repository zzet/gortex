#!/usr/bin/env python3
"""Run one CI test job: whole package groups, or one shard of a large package.

TEST_PARTITION names the job's work as "+"-joined groups, each optionally one
shard of n: "indexer:1/2" or "mcp+store". A large package is compiled once,
and its top-level tests run in batches on one worker process per CPU. The
"rest" group runs every other package under one `go test -p <cpus>`.

Every compiled top-level test, example and fuzz seed of a large package must
report a result, or the job fails. A batch or package that fails while other
workers share the machine is rerun once, whole and alone; only if that rerun
passes completely does the job pass, and the failure is reported as flaky. A
data race, a panic (a test timeout is one) or a crash is never rerun.
"""

import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import queue
import re
import subprocess
import sys
import tempfile
import threading
import time

GROUPS = {
    "indexer": "/internal/indexer",
    "mcp": "/internal/mcp",
    "store": "/internal/graph/store_sqlite",
}
REST = "rest"
# Instrumenting the SQLite C that modernc transpiled to Go, and its libc,
# makes store tests about 8x slower under -race. The driver's Go layer stays
# instrumented, so a buffer gortex binds is still checked; races inside
# SQLite's own memory, such as the VFS tables gortex installs, are not.
RACE_FLAGS = ["-race", "-gcflags=modernc.org/sqlite/lib=-race=false",
              "-gcflags=modernc.org/libc/...=-race=false"]
COVER_FLAGS = ["-cover", "-covermode=atomic"]
# macOS runs without -race, but its FSEvents and kqueue watcher backends start
# goroutines no other platform compiles. The indexer's watcher tests drive
# them, so macOS race-checks those tests.
DARWIN_RACE_TESTS = re.compile(r"Watch|Storm|Realtime|Poller")
TEST_BUDGET_SECONDS = 35 * 60
BATCHES_PER_WORKER = 8
RESULT = re.compile(r"^--- (PASS|FAIL|SKIP): (\S+) \(([\d.]+)s\)")
RACE_REPORT = "WARNING: DATA RACE"
TIMEOUT_REPORT = "panic: test timed out"
CRASH = re.compile(r"(?m)^(panic: |fatal error: |SIG[A-Z]+: |Exception 0x[0-9a-fA-F]+ )"
                   r"|signal arrived during")


def capture(command, **kwargs):
    return subprocess.check_output(command, text=True, encoding="utf-8", **kwargs)


def hard_failure(output):
    """A data race, panic (a test timeout is one) or crash is a bug, not a flake."""
    return RACE_REPORT in output or bool(CRASH.search(output))


def parse_partition(spec):
    """Return [(group, shard, count)] with 0-based shards; reject anything else."""
    items = []
    for item in spec.split("+"):
        match = re.fullmatch(r"([a-z]+)(?::([1-9]\d*)/([1-9]\d*))?", item)
        if not match:
            raise ValueError(f"bad partition item {item!r}: want group or group:i/n")
        group, shard, count = match.group(1), int(match.group(2) or 1), int(match.group(3) or 1)
        if group != REST and group not in GROUPS:
            raise ValueError(f"unknown group {group!r}")
        if shard > count or (group == REST and count != 1):
            raise ValueError(f"bad shard in {item!r}")
        items.append((group, shard - 1, count))
    if len({group for group, _, _ in items}) != len(items):
        raise ValueError(f"partition {spec!r} names a group twice")
    return items


def check_layout(rows):
    """Every OS must run every group exactly once: all n shards of one count."""
    by_os = {}
    for os_name, spec in rows:
        for group, shard, count in parse_partition(spec):
            by_os.setdefault(os_name, {}).setdefault(group, []).append((shard, count))
    expected = set(GROUPS) | {REST}
    for os_name, groups in by_os.items():
        if set(groups) != expected:
            raise ValueError(f"{os_name} runs groups {sorted(groups)}, want {sorted(expected)}")
        for group, shards in groups.items():
            counts = {count for _, count in shards}
            if len(counts) != 1 or sorted(shard for shard, _ in shards) != list(range(counts.pop())):
                raise ValueError(f"{os_name} {group} shards {sorted(shards)} are not exactly 1..n of one n")
    return by_os


def workflow_rows(text):
    """(os, partition) jobs of ci.yml's test matrix: os x partition lists, plus includes."""
    if re.search(r"(?m)^\s+exclude:", text):
        raise ValueError("the test matrix must not use exclude:, which this check does not model")

    def values(key):
        match = re.search(rf"(?m)^\s+{key}:\s*\[([^\]]*)\]", text)
        return [value.strip().strip("'\"") for value in match.group(1).split(",")] if match else []

    rows = [(os_name, partition) for os_name in values("os") for partition in values("partition")]
    for body in re.findall(r"\{([^{}]*\bpartition:[^{}]*)\}", text):
        fields = dict(part.split(":", 1) for part in body.split(",") if ":" in part)
        fields = {key.strip(): value.strip().strip("'\"") for key, value in fields.items()}
        rows.append((fields["os"], fields["partition"]))
    if not rows:
        raise ValueError("found no test matrix rows (os/partition) in the workflow")
    return rows


def exact_pattern(names):
    # Factoring shared prefixes keeps Windows' process command line short.
    # Anchors apply only to top-level names; Go runs all their subtests.
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


def plan_batches(names, shard, count, batches):
    # Round-robin twice: name families (TestDirtyChain*, TestKindParity*) are
    # often slow together, so spreading them evens out shards and batches.
    selected = names[shard::count]
    if not selected:
        raise ValueError("every shard must have at least one compiled test")
    batches = min(batches, len(selected))
    return [selected[index::batches] for index in range(batches)]


def parse_results(output):
    results = {}
    for line in output.splitlines():
        match = RESULT.match(line)
        if match and "/" not in match.group(2):
            results[match.group(2)] = (match.group(1), float(match.group(3)))
    return results


def test_output(output, name):
    """The log lines of one top-level test, or all of them if it cannot tell."""
    lines = output.splitlines()
    start = next((index for index, line in enumerate(lines) if line == f"=== RUN   {name}"), None)
    end = next((index for index, line in enumerate(lines)
                if line.startswith(f"--- FAIL: {name} (")), None)
    if start is None or end is None or end < start:
        return output
    # Panics print after the FAIL line; keep everything up to the next test.
    tail = next((index for index in range(end + 1, len(lines))
                 if lines[index].startswith("=== RUN   ") and "/" not in lines[index]), len(lines))
    return "\n".join(lines[start:tail])


def go_test_events(output):
    """Index `go test -json` output by package and top-level test."""
    tests, packages, test_out, package_out = {}, {}, {}, {}
    for line in output.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not isinstance(event, dict):
            continue
        package, name, action = event.get("Package"), event.get("Test"), event.get("Action")
        if action == "output":
            package_out[package] = package_out.get(package, "") + event.get("Output", "")
            if name and "/" not in name:
                test_out[(package, name)] = test_out.get((package, name), "") + event.get("Output", "")
        elif action in ("pass", "fail", "skip"):
            if name is None:
                packages[package] = (action, event.get("Elapsed", 0.0))
            elif "/" not in name:
                tests[(package, name)] = (action, event.get("Elapsed", 0.0))
        elif action == "build-output":
            path = event.get("ImportPath")
            package_out[path] = package_out.get(path, "") + event.get("Output", "")
    return {"tests": tests, "packages": packages, "output": test_out, "package_output": package_out}


def merge_coverage(paths, destination):
    mode, blocks = None, {}
    for path in paths:
        if not path.exists():
            continue
        for line in path.read_text().splitlines():
            if line.startswith("mode: "):
                if mode not in (None, line):
                    raise ValueError(f"coverage modes differ: {mode} and {line}")
                mode = line
            elif line:
                block, count = line.rsplit(" ", 1)
                blocks[block] = blocks.get(block, 0) + int(count)
    if mode is None:
        return False
    with destination.open("w") as out:
        out.write(mode + "\n")
        out.writelines(f"{block} {count}\n" for block, count in blocks.items())
    return True


class Job:
    def __init__(self, race, windows, workers, scratch, deadline):
        self.race, self.windows, self.workers = race, windows, workers
        self.scratch, self.deadline = scratch, deadline
        self.durations = {}
        self.failures = []  # (title, output)
        self.flaky = []  # (title, output of the failed first run)
        self.coverage = []
        self.slots = queue.Queue()
        for slot in range(workers):
            self.slots.put(slot)
        self.lock = threading.Lock()
        self.base_env = dict(os.environ)
        for key in ("GOCACHE", "GOMODCACHE", "GOPATH", "GOENV"):
            self.base_env[key] = capture(["go", "env", key]).strip()

    def remaining(self):
        left = self.deadline - time.monotonic()
        if left <= 0:
            raise ValueError("test execution budget exhausted")
        return left

    def env(self, slot):
        # Each worker gets its own home and temp directories, so tests of one
        # package that touch the user's config or cache never share them.
        root = self.scratch / f"w{slot}"
        home, temp = root / "h", root / "t"
        temp.mkdir(parents=True, exist_ok=True)
        home.mkdir(parents=True, exist_ok=True)
        env = dict(self.base_env, HOME=str(home), TMPDIR=str(temp), TMP=str(temp), TEMP=str(temp))
        if self.windows:
            env.update(USERPROFILE=str(home), APPDATA=str(home / "AppData" / "Roaming"),
                       LOCALAPPDATA=str(home / "AppData" / "Local"))
        else:
            env.update(XDG_CONFIG_HOME=str(home / ".config"), XDG_CACHE_HOME=str(home / ".cache"),
                       XDG_DATA_HOME=str(home / ".local" / "share"),
                       XDG_STATE_HOME=str(home / ".local" / "state"))
        return env

    def run(self, command, slot, log, **kwargs):
        left = self.remaining()
        with log.open("w", encoding="utf-8") as out:
            try:
                code = subprocess.call(command, env=self.env(slot), stdout=out,
                                       stderr=subprocess.STDOUT, timeout=left + 120, **kwargs)
            except subprocess.TimeoutExpired:
                out.write(f"\n{TIMEOUT_REPORT}: the test process outlived the job budget\n")
                code = -1
        return code, log.read_text(encoding="utf-8", errors="replace")

    def fail(self, title, output):
        with self.lock:
            self.failures.append((title, output))

    def record(self, label, results):
        with self.lock:
            for name, (_, seconds) in results.items():
                self.durations[f"{label} {name}"] = seconds

    # Large packages: one compiled binary, batches spread across workers.

    def run_package(self, group, package, shard, count, directory, race=None, select=None):
        race = self.race if race is None else race
        suffix = "-race" if race and not self.race else ""
        binary = self.scratch / (group + suffix + (".test.exe" if self.windows else ".test"))
        flags = (RACE_FLAGS if race else []) + (COVER_FLAGS if self.race else [])
        started = time.monotonic()
        subprocess.run(["go", "test", "-c", "-o", str(binary)] + flags + [package], check=True)
        names = test_names(capture([str(binary), "-test.list", "."], cwd=directory))
        if select:
            names = [name for name in names if select.search(name)]
        batches = plan_batches(names, shard, count, BATCHES_PER_WORKER * self.workers)
        label = f"{group}{suffix}:{shard + 1}/{count}"
        print(f"{label}: {package} runs {sum(map(len, batches))} tests in {len(batches)} batches "
              f"on {self.workers} workers (built in {time.monotonic() - started:.0f}s)", flush=True)

        def batch(index, names):
            slot = self.slots.get()
            try:
                return self.run_batch(label, binary, directory, names, slot, f"{group}{suffix}-{index}")
            finally:
                self.slots.put(slot)

        with ThreadPoolExecutor(self.workers) as pool:
            reruns = [item for item in pool.map(batch, range(len(batches)), batches) if item]
        self.rerun_batches(label, binary, directory, reruns)
        print(f"{label}: done in {time.monotonic() - started:.0f}s", flush=True)

    def rerun_batches(self, label, binary, directory, reruns):
        for names, tag, first in reruns:
            # Alone, whole and in the same order: a flake caused by sharing
            # the machine passes; an order dependence or a crash does not.
            code, output, results = self.execute(binary, directory, names, 0, f"{tag}-rerun")
            if code == 0 and all(name in results for name in names):
                failed = [name for name, (result, _) in parse_results(first).items() if result == "FAIL"]
                self.flaky.append((f"{label} {' '.join(failed) or tag}", first))
            else:
                self.batch_failure(label, tag, names, results, output, code)

    def execute(self, binary, directory, names, slot, tag):
        cover = self.scratch / "coverage" / f"{tag}.out"
        command = [str(binary), "-test.run", exact_pattern(names), "-test.v",
                   "-test.paniconexit0", f"-test.timeout={self.remaining():.0f}s"]
        if self.race:
            command += ["-test.coverprofile", str(cover)]
            with self.lock:
                self.coverage.append(cover)
        code, output = self.run(command, slot, self.scratch / "logs" / f"{tag}.log", cwd=directory)
        return code, output, parse_results(output)

    def run_batch(self, label, binary, directory, names, slot, tag):
        started = time.monotonic()
        code, output, results = self.execute(binary, directory, names, slot, tag)
        self.record(label, results)
        complete = all(name in results for name in names)
        failed = any(result == "FAIL" for result, _ in results.values())
        print(f"{label} [{tag}] {len(names)} tests, exit {code}, {time.monotonic() - started:.0f}s", flush=True)
        if code == 0 and complete and not failed:
            return None
        if code == 0 or hard_failure(output):
            # A clean exit that skipped tests, or a race, panic or crash.
            self.batch_failure(label, tag, names, results, output, code)
            return None
        return names, tag, output

    def batch_failure(self, label, tag, names, results, output, code):
        failed = [name for name in names if results.get(name, ("",))[0] == "FAIL"]
        for name in failed:
            self.fail(f"{label} {name}", test_output(output, name))
        missing = [name for name in names if name not in results]
        if missing:
            self.fail(f"{label} [{tag}]: {len(missing)} tests never reported a result", output)
        elif not failed:
            self.fail(f"{label} [{tag}]: exit {code} outside any test", output)

    # Every other package: one go test over all of them.

    def go_test(self, packages, parallel, tag):
        command = ["go", "test", "-json", "-count=1", f"-p={parallel}", f"-timeout={self.remaining():.0f}s"]
        if self.race:
            cover = self.scratch / "coverage" / f"{tag}.out"
            self.coverage.append(cover)
            command += RACE_FLAGS + COVER_FLAGS + ["-coverprofile", str(cover)]
        code, output = self.run(command + packages, 0, self.scratch / "logs" / f"{tag}.log")
        return code, output, go_test_events(output)

    def run_rest(self, packages):
        started = time.monotonic()
        code, output, events = self.go_test(packages, self.workers, "rest")
        for (package, name), (_, seconds) in events["tests"].items():
            self.durations[f"rest {package} {name}"] = seconds
        reruns = []
        for package in packages:
            action = events["packages"].get(package, ("missing", 0.0))
            print(f"{action[0]:7} {package} {action[1]:.1f}s", flush=True)
            failed = [name for (pkg, name), (result, _) in events["tests"].items()
                      if pkg == package and result == "fail"]
            if action[0] in ("pass", "skip") and not failed:
                continue
            text = events["package_output"].get(package, "")
            if action[0] == "fail" and not hard_failure(text):
                reruns.append((package, failed, text))
            else:
                self.package_failure(package, events, text or output)
        if code != 0 and not reruns and not self.failures:
            self.fail("rest", output)
        for package, failed, first in reruns:
            # The whole package, alone: tests a failure kept from running, and
            # checks in TestMain, get their run too.
            code, output, events = self.go_test([package], 1, "rerun-" + package.rsplit("/", 1)[-1])
            passed = events["packages"].get(package, ("missing",))[0] == "pass" and not any(
                result == "fail" for (pkg, _), (result, _) in events["tests"].items() if pkg == package)
            if code == 0 and passed:
                self.flaky.append((f"rest {package} {' '.join(failed) or ''}".rstrip(), first))
            else:
                self.package_failure(package, events, events["package_output"].get(package, "") or output)
        print(f"rest: done in {time.monotonic() - started:.0f}s", flush=True)

    def package_failure(self, package, events, text):
        failed = sorted(name for (pkg, name), (result, _) in events["tests"].items()
                        if pkg == package and result == "fail")
        for name in failed:
            self.fail(f"rest {package} {name}", events["output"].get((package, name), text))
        if not failed or events["packages"].get(package, ("missing",))[0] != "fail" or hard_failure(text):
            self.fail(f"rest {package}", text)

    def report(self):
        slowest = sorted(self.durations.items(), key=lambda item: -item[1])[:20]
        lines = ["### Slowest tests", "", "| seconds | test |", "| ---: | --- |"]
        lines += [f"| {seconds:.1f} | `{name}` |" for name, seconds in slowest]
        print("\nSlowest tests:", flush=True)
        for name, seconds in slowest:
            print(f"  {seconds:8.1f}s {name}")
        for title, output in self.flaky:
            print(f"\n::group::FLAKY {title} (first run's output)\n{output.rstrip()}\n::endgroup::")
            print(f"::warning title=Flaky test::{title} failed beside other tests and passed when rerun alone")
        if self.flaky:
            lines += ["", "### Flaky: failed beside other tests, passed rerun alone", ""]
            lines += [f"- `{title}`" for title, _ in self.flaky]
        for title, output in self.failures:
            print(f"\n::group::FAIL {title}\n{output.rstrip()}\n::endgroup::")
            print(f"::error title=Test failed::{title}")
        if self.failures:
            lines += ["", "### Failed", ""] + [f"- `{title}`" for title, _ in self.failures]
        summary = os.environ.get("GITHUB_STEP_SUMMARY")
        if summary:
            with open(summary, "a", encoding="utf-8") as out:
                out.write("\n".join(lines) + "\n")


def main():
    # Test logs carry arrows and other non-ASCII text; Windows' console
    # encoding would raise on them and lose the failure report.
    for stream in (sys.stdout, sys.stderr):
        stream.reconfigure(encoding="utf-8", errors="replace")
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--partition", default=os.environ.get("TEST_PARTITION"), required=False)
    parser.add_argument("--check-workflow", type=Path, help="validate this workflow's test layout and exit")
    args = parser.parse_args()
    if args.check_workflow:
        layout = check_layout(workflow_rows(args.check_workflow.read_text()))
        print(f"{args.check_workflow}: test layout covers every group on {sorted(layout)}")
        return
    if not args.partition:
        parser.error("set --partition or TEST_PARTITION")
    items = parse_partition(args.partition)
    windows = os.environ.get("TEST_WINDOWS", "true" if os.name == "nt" else "false") == "true"
    race_setting = os.environ.get("TEST_RACE", "false" if windows else "true")
    if race_setting not in ("true", "false") or (windows and race_setting == "true"):
        parser.error("TEST_RACE must be true or false, and false on Windows")
    race = race_setting == "true"
    workers = int(os.environ.get("TEST_WORKERS") or os.cpu_count() or 1)
    module = capture(["go", "list", "-m"]).strip()
    packages = capture(["go", "list", "./..."]).splitlines()
    if not packages or len(packages) != len(set(packages)):
        raise ValueError("package census must be nonempty and unique")
    large = {group: module + suffix for group, suffix in GROUPS.items()}
    if not set(large.values()) <= set(packages):
        raise ValueError(f"missing large packages: {sorted(set(large.values()) - set(packages))}")
    # Short paths: tests bind Unix sockets under the worker's temp directory,
    # and macOS caps a socket path at 104 bytes.
    base = os.environ.get("RUNNER_TEMP") or ("/tmp" if os.path.isdir("/tmp") else tempfile.gettempdir())
    root = Path(base) / "gt"
    root.mkdir(parents=True, exist_ok=True)
    scratch = Path(tempfile.mkdtemp(prefix="j", dir=root))
    for name in ("logs", "coverage"):
        (scratch / name).mkdir()
    if windows:
        # Keep go's build scratch beside the tests', off the system drive.
        os.environ["GOTMPDIR"] = str(root)
    job = Job(race, windows, workers, scratch, time.monotonic() + TEST_BUDGET_SECONDS)
    print(f"partition {args.partition}: race={race} workers={workers} scratch={scratch}", flush=True)

    def directory(group):
        return capture(["go", "list", "-f", "{{.Dir}}", large[group]]).strip()

    try:
        for group, shard, count in items:
            if group == REST:
                job.run_rest([package for package in packages if package not in large.values()])
                if sys.platform == "darwin" and not race:
                    job.run_package("indexer", large["indexer"], 0, 1, directory("indexer"),
                                    race=True, select=DARWIN_RACE_TESTS)
            else:
                job.run_package(group, large[group], shard, count, directory(group))
    finally:
        job.report()
        if race:
            merge_coverage(job.coverage, Path("coverage.out"))
        print(f"\nlogs: {scratch / 'logs'}", flush=True)
    if job.failures:
        sys.exit(1)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, subprocess.CalledProcessError) as error:
        print(f"CI test partition failed: {error}", file=sys.stderr)
        sys.exit(1)
