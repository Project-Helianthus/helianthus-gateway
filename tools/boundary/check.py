#!/usr/bin/env python3
"""Protocol-boundary check for the gateway (rule 1).

Scans the working-directory tree for protocol imports, protocol or vendor
names, register tables and vendor identifiers. Every finding must match a
line of the debt ledger (ledger.tsv) or an entry of exceptions.json. The
ledger and exceptions are read from this file's own directory. When
GITHUB_EVENT_PATH names an event payload, the ledger committed at the base
revision of that event is read with git and no ledger occurrence count may
rise against it (shrink-only).
"""

import ast
import bisect
import json
import os
import re
import subprocess
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
MODULE = "github.com/Project-Helianthus/helianthus-gateway"
HELIANTHUS = "github.com/Project-Helianthus/"
REGISTRY = "internal/promotioncapture/contracts/leaf-promotion-captured-multi-leaf-registry-v1.json"
PY_TABLES = ("RENAMED", "QUALIFIER_ONLY")
UI_DIRS = ("portal/web/src/", "ui/static/")

DRIVER_MODULES = (
    ("helianthus-ebusgo", "GG08"),
    ("helianthus-ebusreg", "GG08"),
    ("helianthus-modbus", "GG09"),
    ("helianthus-modbusreg", "GG09"),
    ("helianthus-canbus", "GG07"),
    ("helianthus-canbusreg", "GG07"),
    ("helianthus-eebusreg", "GG10"),
    ("helianthus-eebus-go", "GG10"),
    ("helianthus-ship-go", "GG10"),
    ("helianthus-spine-go", "GG10"),
    ("helianthus-matterreg", "GG11"),
    ("helianthus-matter-binding", "GG11"),
)
GATEWAY_PROTOCOL_PACKAGES = (
    ("internal/adaptermux", "GG08"),
    ("internal/vaillant", "GG08"),
    ("internal/modbusadapter", "GG09"),
    ("internal/canbusadapter", "GG07"),
    ("internal/eebusadapter", "GG10"),
    ("internal/transport", "GG08"),
)
# Owner rows whose deletion lists take these paths regardless of the protocol.
PATH_OWNERS = (
    ("scripts/capture_m8_source_window.sh", "GG10"),
    ("internal/m8sourcecapture/", "GG10"),
    ("internal/promotioncapture/", "GG10"),
    ("cmd/gateway/leaf_promotion_live_source.go", "GG10"),
    ("eebus_promoted_semantic.go", "GG10"),
    ("eebus_inbound_values.go", "GG10"),
    ("scripts/adopt_modbusreg_packages.py", "GG09"),
)

PROSE = {
    "go": re.compile(r'//[^\n]*|/\*.*?\*/|"(?:\\.|[^"\\\n])*"|`[^`]*`|\'(?:\\.|[^\'\\\n])*\'', re.S),
    "py": re.compile(r'"""(?:.|\n)*?"""|\'\'\'(?:.|\n)*?\'\'\'|#[^\n]*|"(?:\\.|[^"\\\n])*"|\'(?:\\.|[^\'\\\n])*\'', re.S),
    "sh": re.compile(r'(?:(?<=\s)|^)#[^\n]*|"(?:\\.|[^"\\\n])*"|\'[^\'\n]*\'', re.M),
    "js": re.compile(r'//[^\n]*|/\*.*?\*/|"(?:\\.|[^"\\\n])*"|\'(?:\\.|[^\'\\\n])*\'|`(?:\\.|[^`\\])*`', re.S),
    "css": re.compile(r'/\*.*?\*/|"(?:\\.|[^"\\\n])*"|\'(?:\\.|[^\'\\\n])*\'', re.S),
    "html": re.compile(r'<!--.*?-->', re.S),
}
IMPORT_BLOCK = re.compile(r'^import\s*\((.*?)^\)', re.M | re.S)
IMPORT_SINGLE = re.compile(r'^import\s+(?:[A-Za-z_.][\w.]*\s+)?(?:"([^"\n]+)"|`([^`\n]+)`)', re.M)
IMPORT_SPEC = re.compile(r'^[ \t]*(?:[A-Za-z_.][\w.]*[ \t]+)?(?:"([^"\n]+)"|`([^`\n]+)`)', re.M)
BUILD = re.compile(r'^//go:build[ \t]+(.+)$', re.M)
VENDOR_ID = re.compile(
    r'\b([A-Za-z_]*(?:manufacturer|vendor|device_?type|product_?id|product_?type)[A-Za-z_]*)\b'
    r'(?:[ \t]+[\w.\[\]*]+)?\s*(?::=|==|!=|=|:|,|\()\s*(0x[0-9a-fA-F]+|\d+)\b', re.I)
MANUF_ACCESS = r'[A-Za-z_$][\w$]*\.manufacturer\b'
SWITCH_MANUF = re.compile(r'switch\s*\(\s*' + MANUF_ACCESS + r'\s*\)\s*\{')
LITERAL = r'(["\'])((?:\\.|(?!\1)[^\\\n])*)\1'
MANUF_RIGHT = re.compile(MANUF_ACCESS + r'\s*(?:===?|!==?)\s*' + LITERAL)
MANUF_LEFT = re.compile(LITERAL + r'\s*(?:===?|!==?)\s*' + MANUF_ACCESS)
CASE_LITERAL = re.compile(r'\bcase\s+' + LITERAL + r'\s*:')
GO_REGISTER_ENTRY = re.compile(
    r'(?:"(?:\\.|[^"\\\n])*"|0x[0-9a-fA-F]+|\b\d+)\s*:\s*\{[^{}]*?'
    r'\b\w*(?:Register|Addr|Selector|Cluster|Attribute)\w*\s*:\s*(?:0x[0-9a-fA-F]+|\d+)\b[^{}]*\}')
CAN_TOKENS = frozenset({"can", "canbus", "socketcan"})
CAN_CONTEXT = re.compile(r'\s+(bus|frames?|ids?|identifiers?)\b', re.I)


def load_vocabulary(here=HERE):
    entries = json.loads((here / "vocabulary.json").read_text(encoding="utf-8"))
    return {entry["token"]: entry for entry in entries}


def load_exceptions(here=HERE):
    return json.loads((here / "exceptions.json").read_text(encoding="utf-8"))["entries"]


def load_guard_operands(here=HERE):
    raw = json.loads((here / "guard_operands.json").read_text(encoding="utf-8"))
    return {path: set(lines) for path, lines in raw.items()}


def canon(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def owner_of(path, seed, protocol):
    for suffix, owner in PATH_OWNERS:
        if suffix.endswith("/"):
            if path.startswith(suffix):
                return owner
        elif path == suffix or path.endswith("/" + suffix):
            return owner
    if path.startswith("graphql/") and (seed == "GG08" or (protocol or "").startswith("eBUS")):
        return "GG13"
    return seed


def tokens_of(run):
    """Exact run, camel segments, and their uppercase-acronym subdivisions."""
    pieces = {run}
    for segment in re.split(r"(?<=[a-z])(?=[A-Z])", run):
        pieces.add(segment)
        pieces.update(re.split(r"(?<=[A-Z])(?=[A-Z][a-z])", segment))
    return {piece.lower() for piece in pieces}


def line_starts(text):
    return [0] + [m.end() for m in re.finditer("\n", text)]


def line_at(starts, pos):
    return bisect.bisect_right(starts, pos)


def build_of(text):
    match = BUILD.search(text)
    return match.group(1).strip() if match else None


def finding(path, line, kind, item, seed, protocol, text, where="content"):
    return {"path": path, "line": line, "kind": kind, "item": item,
            "owner": owner_of(path, seed, protocol), "text": text, "where": where}


def go_imports(path, text, starts, lines, vocab):
    """Import specs of a Go file: (findings, string-literal spans to skip for name scanning)."""
    found, skip = [], []
    specs = []
    for match in IMPORT_BLOCK.finditer(text):
        for spec in IMPORT_SPEC.finditer(text, match.start(1), match.end(1)):
            specs.append(import_literal(spec))
    for match in IMPORT_SINGLE.finditer(text):
        specs.append(import_literal(match))
    for literal_start, literal_end, imported in specs:
        owner, protocol = import_owner(imported, vocab)
        if owner is None:
            # An unclassified path stays visible to the name scan.
            continue
        skip.append((literal_start, literal_end))
        line = line_at(starts, literal_start)
        found.append(finding(path, line, "import", imported, owner, protocol, lines[line - 1].strip()))
    return found, skip


def import_literal(match):
    """Span of an import path literal, quotes included, and its value (double or raw-string)."""
    if match.group(1) is not None:
        return match.start(1) - 1, match.end(1) + 1, match.group(1)
    return match.start(2) - 1, match.end(2) + 1, match.group(2)


def import_owner(imported, vocab):
    for name, owner in DRIVER_MODULES:
        full = HELIANTHUS + name
        if imported == full or imported.startswith(full + "/"):
            return owner, "driver"
    if "go-fabric" in imported.split("/"):
        return "GG11", "Matter"
    if imported.startswith(MODULE + "/"):
        return gateway_owner(imported[len(MODULE) + 1:], vocab)
    return None, None


def check_package_owners():
    for name, owner in GATEWAY_PROTOCOL_PACKAGES:
        if owner is None:
            raise ValueError(f"classified gateway package has no deletion owner: {name}")


def gateway_owner(rel, vocab):
    for name, owner in GATEWAY_PROTOCOL_PACKAGES:
        if rel == name or rel.startswith(name + "/"):
            return owner, "gateway"
    for element in rel.split("/"):
        for token, entry in vocab.items():
            lowered = element.lower()
            if lowered == token or (len(token) >= 4 and lowered.startswith(token)):
                return entry["owner"], entry["protocol"]
    return None, None


def can_in_protocol_context(text, start, end, span):
    body = text[span[0]:span[1]].strip().strip("\"'`#/*").strip()
    if body.lower() == "can":
        return True
    return bool(CAN_CONTEXT.match(text, end, span[1]))


def name_findings(path, text, lang, lines, starts, skip, vocab):
    found = []
    prose = [(m.start(), m.end()) for m in PROSE[lang].finditer(text)] if lang in PROSE else []
    prose_starts = [s for s, _ in prose]
    for match in re.finditer(r"[A-Za-z]+", text):
        start, end = match.span()
        if any(lo <= start < hi for lo, hi in skip):
            continue
        run = match.group()
        index = bisect.bisect_right(prose_starts, start) - 1
        in_prose = index >= 0 and prose[index][0] <= start < prose[index][1]
        for token in sorted(tokens_of(run) & set(vocab)):
            if in_prose and token == "can" and not can_in_protocol_context(text, start, end, prose[index]):
                continue
            line = line_at(starts, start)
            found.append(finding(path, line, "name", token, *protocol_seed(token, path, vocab),
                                 lines[line - 1].strip()))
    return found


def path_tokens(path, vocab):
    found = set()
    parts = path.split("/")
    for position, part in enumerate(parts):
        stem = os.path.splitext(part)[0] if position == len(parts) - 1 else part
        for match in re.finditer(r"[A-Za-z]+", stem):
            found |= tokens_of(match.group()) & set(vocab)
    return found


def protocol_seed(token, path, vocab):
    """Owner of a vocabulary token; Growatt files that also name CAN are CAN (GG07)."""
    if token == "growatt" and CAN_TOKENS & path_tokens(path, vocab):
        return "GG07", "CAN"
    return vocab[token]["owner"], vocab[token]["protocol"]


def path_findings(path, vocab):
    found = []
    parts = path.split("/")
    for position, part in enumerate(parts):
        stem = os.path.splitext(part)[0] if position == len(parts) - 1 else part
        for match in re.finditer(r"[A-Za-z]+", stem):
            for token in sorted(tokens_of(match.group()) & set(vocab)):
                found.append(finding(path, 0, "name", token, *protocol_seed(token, path, vocab), path,
                                     where="path"))
    return found


def path_owner(path, vocab):
    """Owner of the protocol named by the path: the first sorted vocabulary token."""
    tokens = sorted(path_tokens(path, vocab))
    return protocol_seed(tokens[0], path, vocab) if tokens else (None, None)


def vendor_findings(path, text, lines, starts, vocab):
    found = []
    seed, protocol = path_owner(path, vocab)
    for match in VENDOR_ID.finditer(text):
        item = f"{match.group(1).lower()}={match.group(2).lower()}"
        line = line_at(starts, match.start())
        found.append(finding(path, line, "vendor", item, seed, protocol, lines[line - 1].strip()))
    return found


def manufacturer_findings(path, text, lines, starts, vocab):
    found = []
    literals = [(m.start(), m.group(2)) for m in MANUF_RIGHT.finditer(text)]
    literals += [(m.start(), m.group(2)) for m in MANUF_LEFT.finditer(text)]
    for switch in SWITCH_MANUF.finditer(text):
        depth, index = 0, switch.end() - 1
        while index < len(text):
            if text[index] == "{":
                depth += 1
            elif text[index] == "}":
                depth -= 1
                if depth == 0:
                    break
            index += 1
        for case in CASE_LITERAL.finditer(text, switch.end(), index):
            literals.append((case.start(), case.group(2)))
    for pos, literal in literals:
        line = line_at(starts, pos)
        seed = vocab[literal.lower()]["owner"] if literal.lower() in vocab else None
        protocol = vocab[literal.lower()]["protocol"] if literal.lower() in vocab else None
        found.append(finding(path, line, "name", literal, seed, protocol, lines[line - 1].strip()))
    return found


def registry_findings(path, text):
    found = []
    for entry in json.loads(text)["candidate_catalog"]:
        body = {key: value for key, value in entry.items() if key != "fact_hash"}
        for slot in ("ebus_selector", "ebus_fallback", "eebus_source"):
            if entry.get(slot) is not None:
                item = f"{slot}={canon(body)}"
                found.append(finding(path, 0, "register_table", item, "GG10", "eeBUS", entry["candidate_id"]))
    return found


def go_register_findings(path, text, lines, starts, vocab):
    """Keyed Go composite entries that map a name or number to a protocol address (register_table)."""
    seed, protocol = path_owner(path, vocab)
    found = []
    for match in GO_REGISTER_ENTRY.finditer(text):
        item = re.sub(r"\s+", "", match.group(0))
        line = line_at(starts, match.start())
        found.append(finding(path, line, "register_table", item, seed, protocol, lines[line - 1].strip()))
    return found


def python_table_findings(path, text):
    found = []
    tree = ast.parse(text)
    for node in tree.body:
        target = node.targets[0] if isinstance(node, ast.Assign) else getattr(node, "target", None)
        if not isinstance(target, ast.Name) or target.id not in PY_TABLES or not isinstance(node.value, ast.Dict):
            continue
        for key, value in zip(node.value.keys, node.value.values):
            if not isinstance(key, ast.Constant) or not isinstance(key.value, str):
                continue
            found.append(finding(path, key.lineno, "name", key.value, None, None, f"{target.id}[{key.value!r}]",
                                 where="content"))
            if isinstance(value, (ast.Tuple, ast.List)):
                for element in value.elts:
                    item = f"{target.id}[{key.value!r}]:{ast.unparse(element)}"
                    found.append(finding(path, element.lineno, "register_table", item, None, None,
                                         ast.unparse(element)))
    return found


def language(path):
    if path == REGISTRY:
        return "registry"
    if path.endswith(".go"):
        return "go"
    if path.endswith(".sh"):
        return "sh"
    if path.endswith(".py"):
        return "py"
    if path.startswith(UI_DIRS):
        for suffix, lang in ((".js", "js"), (".html", "html"), (".css", "css")):
            if path.endswith(suffix):
                return lang
    return None


def scanned_files(root):
    for dirpath, dirnames, filenames in os.walk(root):
        relative_dir = Path(dirpath).relative_to(root).as_posix()
        prefix = "" if relative_dir == "." else relative_dir + "/"
        dirnames[:] = sorted(
            d for d in dirnames
            if not d.startswith(".") and d != "node_modules" and prefix + d != "tools/boundary"
        )
        for name in sorted(filenames):
            relative = prefix + name
            lang = language(relative)
            if lang is not None:
                yield relative, lang


def admitted(item, exceptions, build):
    for entry in exceptions:
        if entry["path"] != item["path"]:
            continue
        if item["kind"] == "import" and item["item"] in entry["imports"] and build == entry["build"]:
            return True
        if item["where"] == "path" and item["kind"] == "name" and item["item"] in entry.get("path_tokens", []):
            return True
    return False


def scan(root, here=HERE):
    check_package_owners()
    vocab = load_vocabulary(here)
    guards = load_guard_operands(here)
    exceptions = load_exceptions(here)
    found = []
    for path, lang in scanned_files(root):
        text = (root / path).read_text(encoding="utf-8", errors="replace")
        lines = text.split("\n")
        starts = line_starts(text)
        build = build_of(text) if lang == "go" else None
        items = path_findings(path, vocab)
        if lang == "registry":
            items += registry_findings(path, text)
        else:
            skip = []
            if lang == "go":
                imports, skip = go_imports(path, text, starts, lines, vocab)
                items += imports + go_register_findings(path, text, lines, starts, vocab)
            items += name_findings(path, text, lang, lines, starts, skip, vocab)
            if lang in ("go", "py", "sh", "js"):
                items += vendor_findings(path, text, lines, starts, vocab)
            if lang == "py":
                items += python_table_findings(path, text)
            if lang == "js" and path.startswith(UI_DIRS):
                items += manufacturer_findings(path, text, lines, starts, vocab)
        for item in items:
            if item["text"] in guards.get(path, ()):
                continue
            if admitted(item, exceptions, build):
                continue
            found.append(item)
    return found


def read_ledger_text(text):
    rows = []
    for number, raw in enumerate(text.split("\n"), 1):
        if not raw:
            continue
        fields = raw.split("\t")
        if len(fields) != 4:
            raise ValueError(f"ledger line {number} does not have four fields")
        rows.append(tuple(fields))
    return rows


def ledger_key(path, kind, item):
    return (path, kind, item)


def ratchet(findings, ledger_rows):
    errors = []
    found_counts, ledger_counts = {}, {}
    for item in findings:
        found_counts.setdefault(ledger_key(item["path"], item["kind"], item["item"]), []).append(item)
    for path, kind, item, owner in ledger_rows:
        ledger_counts[ledger_key(path, kind, item)] = ledger_counts.get(ledger_key(path, kind, item), 0) + 1
    for key, occurrences in sorted(found_counts.items()):
        unledgered = sorted(occurrences, key=lambda o: o["line"])[ledger_counts.get(key, 0):]
        for occurrence in unledgered:
            owner = occurrence["owner"] or "UNMAPPED"
            errors.append(f"{occurrence['path']}:{occurrence['line']}: {occurrence['kind']} {occurrence['item']!r} "
                          f"is not in the ledger and not admitted (owner {owner}): {occurrence['text'][:160]}")
    for key, count in sorted(ledger_counts.items()):
        surplus = count - len(found_counts.get(key, []))
        for _ in range(max(surplus, 0)):
            errors.append(f"ledger line no longer matches a finding: {key[0]}\t{key[1]}\t{key[2]}")
    return errors


def base_ratchet(ledger_rows, base_rows):
    base_counts = {}
    for row in base_rows:
        base_counts[row] = base_counts.get(row, 0) + 1
    proposed = {}
    for row in ledger_rows:
        proposed[row] = proposed.get(row, 0) + 1
    errors = []
    for row, count in sorted(proposed.items()):
        for _ in range(max(count - base_counts.get(row, 0), 0)):
            errors.append(f"ledger line exceeds the base revision: {row[0]}\t{row[1]}\t{row[2]}\towner {row[3]}")
    return errors


def ledger_at_base(root, base):
    if subprocess.run(["git", "-C", str(root), "cat-file", "-e", f"{base}^{{commit}}"],
                      capture_output=True).returncode != 0:
        raise ValueError(f"base revision is not a commit in this repository: {base}")
    shown = subprocess.run(["git", "-C", str(root), "show", f"{base}:tools/boundary/ledger.tsv"],
                           capture_output=True, text=True)
    if shown.returncode != 0:
        return None
    return read_ledger_text(shown.stdout)


def base_revision(event_path):
    event = json.loads(Path(event_path).read_text(encoding="utf-8"))
    if "pull_request" in event:
        return event["pull_request"]["base"]["sha"]
    if "before" in event:
        return event["before"]
    raise ValueError("event payload names no base revision")


def build_ledger(root, here=HERE):
    """Seed rows for the ledger from the current tree (used once, when the ledger is written)."""
    rows = []
    for item in scan(root, here):
        if item["owner"] is None:
            raise ValueError(f"finding with no owner row: {item['path']}:{item['line']} {item['item']!r}")
        rows.append((item["path"], item["kind"], item["item"], item["owner"]))
    return sorted(rows)


def main():
    root = Path.cwd()
    try:
        findings = scan(root)
        ledger_rows = read_ledger_text((HERE / "ledger.tsv").read_text(encoding="utf-8"))
        errors = ratchet(findings, [(p, k, i, o) for p, k, i, o in ledger_rows])
        event_path = os.environ.get("GITHUB_EVENT_PATH")
        seeding = False
        if event_path:
            base_rows = ledger_at_base(root, base_revision(event_path))
            if base_rows is None:
                seeding = True
            else:
                errors += base_ratchet(ledger_rows, base_rows)
    except (OSError, ValueError, KeyError, subprocess.SubprocessError, SyntaxError) as error:
        print(f"protocol boundary check failed: {error}", file=sys.stderr)
        return 1
    if errors:
        print("\n".join(errors), file=sys.stderr)
        return 1
    note = " (initial seeding: no ledger at the base revision)" if seeding else ""
    print(f"Protocol boundary OK: {len(findings)} findings matched {len(ledger_rows)} ledger lines{note}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
