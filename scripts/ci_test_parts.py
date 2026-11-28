#!/usr/bin/env python3
"""Assign each top-level Go test to one stable CI part.

Reads the stdout of `go test -list '.*'` on stdin. That command prints one
top-level name per line (tests, examples that check output, fuzz targets, and
benchmarks) and then a summary line such as `ok <package> <time>`.

Names are sorted, then dealt round-robin: sorted index i belongs to part
i mod N. The same list and part count always produce the same parts. The parts
are disjoint and their union is the list. An empty part is an error, so a leg
does not run `go test` with its default of every test.

Stdout, on success:
    run <regex>
    bench <regex>    # only when this part owns a benchmark

The run regex is the part's tests, examples, and fuzz targets, anchored so a
name cannot match a prefix of another. The bench regex is the part's
benchmarks. A part that has only benchmarks prints `run ^$`.
"""

import argparse
import re
import sys

_PREFIXES = ("Test", "Benchmark", "Fuzz", "Example")
_SUMMARY = re.compile(r"^(?:ok|\?)\s+\S.*")


def kind(name):
    """Return which top-level Go test kind name is, or None."""
    for prefix in _PREFIXES:
        if name.startswith(prefix):
            rest = name[len(prefix):]
            if rest == "" or not rest[0].islower():
                return prefix
    return None


def parse_list(text):
    """Return top-level names from go test -list stdout, in listed order.

    A summary line is ignored. Any other non-name fails closed, including a
    FAIL line from a list this caller did not already reject by exit status.
    """
    names = []
    for raw in text.splitlines():
        line = raw.strip()
        if line == "" or _SUMMARY.fullmatch(line):
            continue
        if kind(line) is None:
            raise ValueError("not a top-level test name: " + line)
        names.append(line)
    if len(names) != len(set(names)):
        raise ValueError("duplicate test name")
    return names


def assign(names, part_count):
    """Deal sorted names round-robin into part_count parts.

    Each part keeps sorted order. The result is a partition of names.
    """
    if part_count < 1:
        raise ValueError("part count must be at least 1")
    ordered = sorted(names)
    parts = [[] for _ in range(part_count)]
    for index, name in enumerate(ordered):
        parts[index % part_count].append(name)
    require_partition(ordered, parts)
    return parts


def require_partition(names, parts):
    """Raise unless parts are disjoint and their union equals names."""
    found = [name for part in parts for name in part]
    if len(found) != len(set(found)):
        raise ValueError("parts overlap")
    if sorted(found) != sorted(names):
        raise ValueError("parts do not match the list")


def anchor(names):
    """A full-match regex for these names, in the order given."""
    body = "|".join(re.escape(name) for name in names)
    return "^(?:" + body + ")$"


def format_lines(names):
    """Return the run line and, when this part owns benchmarks, the bench line."""
    benchmarks = [name for name in names if kind(name) == "Benchmark"]
    others = [name for name in names if kind(name) != "Benchmark"]
    lines = ["run " + (anchor(others) if others else "^$")]
    if benchmarks:
        lines.append("bench " + anchor(benchmarks))
    return lines


def part_lines(names, part_count, part_index):
    """Return the stdout lines for one part, or raise if that part is empty."""
    if part_index < 0 or part_index >= part_count:
        raise ValueError("part index out of range")
    chosen = assign(names, part_count)[part_index]
    if not chosen:
        raise ValueError(f"part {part_index} of {part_count} is empty")
    return format_lines(chosen)


def main(argv):
    parser = argparse.ArgumentParser(description="Assign go test -list names to one CI part.")
    parser.add_argument("--parts", type=int, required=True)
    parser.add_argument("--part", type=int, required=True)
    args = parser.parse_args(argv)
    try:
        lines = part_lines(parse_list(sys.stdin.read()), args.parts, args.part)
    except (ValueError, IndexError) as exc:
        print(exc, file=sys.stderr)
        return 1
    print("\n".join(lines))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
