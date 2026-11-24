"""Synthetic import mutations exercise syntax, source placement and graph edges."""

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import shutil
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
SPEC = importlib.util.spec_from_file_location("semantic_imports", ROOT / "scripts/check_semantic_imports.py")
guard = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(guard)
NATIVE = "github.com/Project-Helianthus/helianthus-ebusgo/protocol"


class SemanticImportsTest(unittest.TestCase):
    def setUp(self):
        scratch = ROOT / ".tmp"
        scratch.mkdir(parents=True, exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(dir=scratch)
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        for owner in guard.PROTECTED:
            self.write(f"{owner}/base.go", "package neutral\n")

    def write(self, path, text):
        target = self.root / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text)

    def errors(self):
        return guard.violations(guard.parse_sources(self.root))

    def test_direct_import_syntax_and_each_owner(self):
        for owner in guard.PROTECTED:
            for alias in ("", "renamed ", "_ ", ". "):
                with self.subTest(owner=owner, alias=alias):
                    self.write(f"{owner}/base.go", f'package neutral\nimport {alias}"{NATIVE}"\n')
                    errors = "\n".join(self.errors())
                    self.assertIn(f"{guard.MODULE}/{owner}", errors)
                    self.assertIn(NATIVE, errors)
                    self.write(f"{owner}/base.go", "package neutral\n")

    def test_all_native_families_and_gateway_implementation(self):
        for native in guard.FORBIDDEN_PREFIXES:
            with self.subTest(native=native):
                self.write("internal/semhub/base.go", f'package neutral\nimport "{native}/example"\n')
                self.assertIn("forbidden native dependency", "\n".join(self.errors()))

    def test_transitive_helper_and_cycle(self):
        helper = f"{guard.MODULE}/internal/helper"
        self.write("internal/semhub/base.go", f'package neutral\nimport "{helper}"\n')
        self.write("internal/helper/helper.go", f'package helper\nimport ("{NATIVE}"; "{guard.MODULE}/internal/semhub")\n')
        errors = "\n".join(self.errors())
        self.assertIn(f"{guard.MODULE}/internal/semhub -> {helper} -> {NATIVE}", errors)
        self.assertIn("dependency requires reviewed rule", errors)

    def test_transitive_allowed_semantic_owner(self):
        self.write("internal/semhub/base.go", f'package neutral\nimport "{guard.MODULE}/internal/semanticview"\n')
        self.write("internal/semanticview/base.go", f'package neutral\nimport "{NATIVE}"\n')
        self.assertIn(f"internal/semhub -> {guard.MODULE}/internal/semanticview -> {NATIVE}", "\n".join(self.errors()))

    def test_platform_build_tags_and_package_names_do_not_exempt_production(self):
        for filename, package, header in (
            ("native_windows.go", "neutral", ""),
            ("native_linux_arm64.go", "neutral", "//go:build linux && arm64\n\n"),
            ("native.go", "neutral_test", "//go:build custom_boundary_test\n\n"),
            ("testdata/native.go", "fixture_test", "//go:build ignore\n\n"),
        ):
            with self.subTest(filename=filename, package=package, header=header):
                self.write(f"internal/semhub/{filename}", f'{header}package {package}\nimport "{NATIVE}"\n')
                self.assertIn(NATIVE, "\n".join(self.errors()))
                (self.root / "internal/semhub" / filename).unlink()

    def test_test_imports_and_transitive_test_dependency(self):
        helper = f"{guard.MODULE}/internal/helper"
        for package in ("neutral", "neutral_test"):
            with self.subTest(package=package):
                self.write("internal/semhub/native_test.go", f'package {package}\nimport "{helper}"\n')
                self.write("internal/helper/base.go", f'package helper\nimport "{NATIVE}"\n')
                self.assertIn(f"{helper} -> {NATIVE}", "\n".join(self.errors()))
        self.write("internal/semhub/native_test.go", f'package neutral_test\nimport "{NATIVE}"\n')
        self.assertIn(NATIVE, "\n".join(self.errors()))

    def test_allowed_semreg_standard_and_semantic_owners(self):
        imports = ["fmt", "syscall/js", *guard.ALLOWED_LOCAL,
                   guard.ALLOWED_EXTERNAL[0] + "/semreg/v1"]
        self.write("internal/semhub/base.go", 'package neutral\nimport (\n' +
                   "\n".join(f'"{imp}"' for imp in imports) + '\n)\n')
        self.assertEqual([], self.errors())

    def test_platform_syscalls_allowed_only_for_semanticmetrics(self):
        self.write("internal/semanticmetrics/base.go",
                   'package neutral\nimport "golang.org/x/sys/unix"\n')
        self.assertEqual([], self.errors())

    def test_platform_syscalls_rejected_for_other_semantic_packages(self):
        for owner in (*guard.PROTECTED, "internal/semanticmetrics/helper"):
            if owner == "internal/semanticmetrics":
                continue
            with self.subTest(owner=owner):
                self.write(f"{owner}/base.go",
                           'package neutral\nimport "golang.org/x/sys/unix"\n')
                errors = "\n".join(self.errors())
                self.assertIn("dependency requires reviewed rule", errors)
                self.assertIn(f"{guard.MODULE}/{owner} -> golang.org/x/sys/unix", errors)
                self.write(f"{owner}/base.go", "package neutral\n")

    def test_semreg_replacements_fail_closed(self):
        semreg = guard.ALLOWED_EXTERNAL[0]
        self.write("internal/semhub/base.go", f'package neutral\nimport "{semreg}/semreg/v1"\n')
        self.write("replacement/native.go", f'package replacement\nimport "{NATIVE}"\n')
        for directive in (
            f"replace {semreg} => ./replacement",
            f"replace (\n {semreg} v0.0.0 => ./replacement\n)",
            f'replace "{semreg}" => "./replacement"',
            f"replace {semreg} => example.org/replacement v0.0.0",
            f"replace {semreg}/submodule => ./replacement",
        ):
            with self.subTest(directive=directive):
                self.write("go.mod", f"module {guard.MODULE}\n\n{directive}\n")
                with self.assertRaisesRegex(ValueError, "SemReg replacement is not allowed"):
                    self.errors()
        result = subprocess.run([sys.executable, str(ROOT / "scripts/check_semantic_imports.py"),
                                 "--root", str(self.root)], capture_output=True, text=True, timeout=160)
        self.assertEqual(1, result.returncode)
        self.assertIn("SemReg replacement is not allowed", result.stderr)

    def test_unrelated_module_replacement_is_allowed(self):
        self.write("go.mod", f"module {guard.MODULE}\n\nreplace example.org/helper => ./helper\n")
        self.assertEqual([], self.errors())

    def test_buildable_semreg_overrides_fail_closed(self):
        semreg = guard.ALLOWED_EXTERNAL[0]
        ancestor = semreg.rsplit("/", 1)[0]
        native_module = NATIVE.rsplit("/", 1)[0]
        cases_root = self.root
        for mode, supplied in (
            ("module-replace", ancestor),
            ("root-replace", semreg),
            ("root-use", semreg),
            ("ancestor-replace", semreg),
            ("ancestor-use", semreg),
            ("root-replace", ancestor),
            ("ancestor-replace", ancestor),
            ("root-use", ancestor),
            ("ancestor-use", ancestor),
        ):
            with self.subTest(mode=mode, supplied=supplied):
                # Each case discovers only its own root or ancestor workspace.
                self.root = cases_root / f"{mode}-{supplied.rsplit('/', 1)[-1]}"
                clone = self.workspace_clone()
                self.write("native/go.mod", f"module {native_module}\ngo 1.22.0\n")
                self.write("native/protocol/base.go", "package protocol\n")
                self.write("gateway/internal/semhub/base.go",
                           f'package neutral\nimport _ "{semreg}/semreg/v1"\n')
                relative = semreg.removeprefix(supplied).lstrip("/")
                candidate = "candidate"
                self.write(f"{candidate}/go.mod", f"module {supplied}\ngo 1.22.0\n")
                self.write(f"{candidate}/{relative}/semreg/v1/base.go",
                           f'package neutral\nimport _ "{NATIVE}"\n')
                module = (f"module {guard.MODULE}\ngo 1.22.0\n"
                          f"require {supplied} v0.0.0\n")
                if mode == "module-replace":
                    module += (f"require {native_module} v0.0.0\n"
                               f"replace {supplied} => ../{candidate}\n"
                               f"replace {native_module} => ../native\n")
                    selected = "off"
                else:
                    workspace = clone / "go.work" if mode.startswith("root-") else self.root / "go.work"
                    gateway_path = "." if mode.startswith("root-") else "./gateway"
                    prefix = "../" if mode.startswith("root-") else "./"
                    work = f"go 1.22.0\nuse (\n {gateway_path}\n {prefix}native\n"
                    if mode.endswith("use"):
                        work += f" {prefix}{candidate}\n)\n"
                    else:
                        work += f")\nreplace {supplied} => {prefix}{candidate}\n"
                    workspace.write_text(work)
                    selected = ""  # Exercise Go's root/ancestor workspace discovery.
                self.write("gateway/go.mod", module)
                env = dict(os.environ, GOWORK=selected, GOPROXY="off", GOSUMDB="off",
                           GOTOOLCHAIN="local", GOFLAGS="-buildvcs=false",
                           GOCACHE=str(ROOT / ".tmp/gocache"))
                built = subprocess.run(
                    ["go", "build", "./internal/semhub/..."], cwd=clone, env=env,
                    capture_output=True, text=True, timeout=150,
                )
                self.assertEqual(0, built.returncode, built.stderr)
                listed = subprocess.run(
                    ["go", "list", "-deps", "./internal/semhub/..."], cwd=clone, env=env,
                    capture_output=True, text=True, timeout=30,
                )
                self.assertEqual(0, listed.returncode, listed.stderr)
                self.assertIn(NATIVE, listed.stdout.splitlines())
                with mock.patch.dict(os.environ, GOWORK=selected):
                    with self.assertRaisesRegex(ValueError, "SemReg .* is not allowed"):
                        guard.parse_sources(clone)

    def test_semreg_workspace_replacements_fail_closed(self):
        semreg = guard.ALLOWED_EXTERNAL[0]
        self.write("go.mod", f"module {guard.MODULE}\n")
        self.write("internal/semhub/base.go", f'package neutral\nimport "{semreg}/semreg/v1"\n')
        self.write("replacement/native.go", f'package replacement\nimport "{NATIVE}"\n')
        for directive in (
            f"replace {semreg} => ./replacement",
            f"replace (\n {semreg} v0.0.0 => ./replacement\n)",
            f'replace "{semreg}" => "./replacement"',
            f"replace {semreg} => example.org/replacement v0.0.0",
            f"replace {semreg}/submodule => ./replacement",
        ):
            with self.subTest(directive=directive):
                self.write("go.work", f"go 1.25.0\nuse .\n{directive}\n")
                with self.assertRaisesRegex(ValueError, "SemReg replacement is not allowed"):
                    self.errors()
        result = subprocess.run([sys.executable, str(ROOT / "scripts/check_semantic_imports.py"),
                                 "--root", str(self.root)], capture_output=True, text=True, timeout=160)
        self.assertEqual(1, result.returncode)
        self.assertIn("SemReg replacement is not allowed", result.stderr)

    def test_unrelated_workspace_replacement_is_allowed(self):
        self.write("go.mod", f"module {guard.MODULE}\n")
        self.write("go.work", "go 1.25.0\nuse .\nreplace example.org/helper => ./helper\n")
        self.assertEqual([], self.errors())

    def test_semreg_workspace_use_modules_fail_closed(self):
        semreg = guard.ALLOWED_EXTERNAL[0]
        self.write("go.mod", f"module {guard.MODULE}\n")
        self.write("internal/semhub/base.go", f'package neutral\nimport "{semreg}/semreg/v1"\n')
        self.write("workspace-semreg/semreg/v1/base.go",
                   f'package neutral\nimport "{NATIVE}"\n')
        for module in (semreg, semreg + "/submodule"):
            for path in ("./workspace-semreg", str(self.root / "workspace-semreg")):
                with self.subTest(module=module, path=path):
                    self.write("workspace-semreg/go.mod", f"module {module}\n")
                    self.write("go.work", f'go 1.25.0\nuse (\n .\n "{path}"\n)\n')
                    with self.assertRaisesRegex(ValueError, "SemReg workspace module is not allowed"):
                        self.errors()
        result = subprocess.run([sys.executable, str(ROOT / "scripts/check_semantic_imports.py"),
                                 "--root", str(self.root)], capture_output=True, text=True, timeout=160)
        self.assertEqual(1, result.returncode)
        self.assertIn("SemReg workspace module is not allowed", result.stderr)

    def test_unrelated_workspace_use_module_is_allowed(self):
        self.write("go.mod", f"module {guard.MODULE}\n")
        self.write("workspace-helper/go.mod", "module example.org/helper\n")
        self.write("go.work", "go 1.25.0\nuse (\n .\n ./workspace-helper\n)\n")
        self.assertEqual([], self.errors())

    def test_workspace_use_module_replacement_fails_closed(self):
        semreg = guard.ALLOWED_EXTERNAL[0]
        self.write("go.mod", f"module {guard.MODULE}\n")
        self.write("workspace-helper/go.mod",
                   f"module example.org/helper\nreplace {semreg} => ../replacement\n")
        self.write("go.work", "go 1.25.0\nuse (\n .\n ./workspace-helper\n)\n")
        with self.assertRaisesRegex(ValueError, "SemReg replacement is not allowed"):
            self.errors()

    def test_workspace_use_missing_or_malformed_module_fails_closed(self):
        self.write("go.work", "go 1.25.0\nuse ./workspace-helper\n")
        for content in (None, "module (\n"):
            with self.subTest(content=content):
                if content is not None:
                    self.write("workspace-helper/go.mod", content)
                with self.assertRaises(subprocess.CalledProcessError):
                    self.errors()

    def test_malformed_workspace_fails_closed(self):
        self.write("go.work", "replace (\n")
        with self.assertRaises(subprocess.CalledProcessError):
            self.errors()

    def workspace_clone(self):
        self.write("gateway/go.mod", f"module {guard.MODULE}\n")
        for owner in guard.PROTECTED:
            self.write(f"gateway/{owner}/base.go", "package neutral\n")
        return self.root / "gateway"

    def test_selected_workspace_semreg_use_fails_closed(self):
        clone = self.workspace_clone()
        semreg = guard.ALLOWED_EXTERNAL[0]
        self.write("local-semreg/go.mod", f"module {semreg}\n")
        self.write("local-semreg/base.go", f'package neutral\nimport "{NATIVE}"\n')
        for selection in ("", str(self.root / "external.work")):
            with self.subTest(selection=selection):
                filename = "external.work" if selection else "go.work"
                self.write(filename, "go 1.25.0\nuse (\n ./gateway\n ./local-semreg\n)\n")
                with mock.patch.dict(os.environ, GOWORK=selection):
                    with self.assertRaisesRegex(ValueError, "SemReg workspace module is not allowed"):
                        guard.parse_sources(clone)

    def test_ancestor_workspace_semreg_replacement_fails_closed(self):
        clone = self.workspace_clone()
        semreg = guard.ALLOWED_EXTERNAL[0]
        self.write("go.work", f"go 1.25.0\nuse ./gateway\nreplace {semreg} => ./replacement\n")
        with mock.patch.dict(os.environ, GOWORK=""):
            with self.assertRaisesRegex(ValueError, "SemReg replacement is not allowed"):
                guard.parse_sources(clone)

    def test_selected_workspace_use_module_replacement_fails_closed(self):
        clone = self.workspace_clone()
        semreg = guard.ALLOWED_EXTERNAL[0]
        self.write("helper/go.mod", f"module example.org/helper\nreplace {semreg} => ../replacement\n")
        self.write("go.work", "go 1.25.0\nuse (\n ./gateway\n ./helper\n)\n")
        with mock.patch.dict(os.environ, GOWORK=""):
            with self.assertRaisesRegex(ValueError, "SemReg replacement is not allowed"):
                guard.parse_sources(clone)

    def test_unrelated_ancestor_workspace_is_allowed(self):
        clone = self.workspace_clone()
        self.write("helper/go.mod", "module example.org/helper\n")
        self.write("go.work", "go 1.25.0\nuse (\n ./gateway\n ./helper\n)\n")
        with mock.patch.dict(os.environ, GOWORK=""):
            self.assertEqual([], guard.violations(guard.parse_sources(clone)))

    def test_disabled_ancestor_workspace_is_ignored(self):
        clone = self.workspace_clone()
        self.write("go.work", f"go 1.25.0\nuse ./gateway\nreplace {guard.ALLOWED_EXTERNAL[0]} => ./replacement\n")
        with mock.patch.dict(os.environ, GOWORK="off"):
            self.assertEqual([], guard.violations(guard.parse_sources(clone)))

    def test_missing_explicit_workspace_fails_closed(self):
        with mock.patch.dict(os.environ, GOWORK=str(self.root / "missing.work")):
            with self.assertRaises(subprocess.CalledProcessError):
                self.errors()

    def test_buildable_selected_workspace_vendor_fails_closed(self):
        clone = self.workspace_clone()
        semreg = guard.ALLOWED_EXTERNAL[0]
        native_module = NATIVE.rsplit("/", 1)[0]
        self.write("gateway/go.mod", f"module {guard.MODULE}\ngo 1.22.0\n"
                   f"require (\n {semreg} v0.0.0\n {native_module} v0.0.0\n)\n")
        self.write("gateway/internal/semhub/base.go",
                   f'package neutral\nimport _ "{semreg}/semreg/v1"\n')
        self.write(f"vendor/{semreg}/semreg/v1/base.go",
                   f'package neutral\nimport _ "{NATIVE}"\n')
        self.write(f"vendor/{NATIVE}/base.go", "package protocol\n")
        self.write("vendor/modules.txt", "## workspace\n"
                   f"# {semreg} v0.0.0\n## explicit; go 1.22.0\n{semreg}/semreg/v1\n"
                   f"# {native_module} v0.0.0\n## explicit; go 1.22.0\n{NATIVE}\n")
        for selection in ("", str(self.root / "external.work")):
            with self.subTest(selection=selection):
                self.write("external.work" if selection else "go.work",
                           "go 1.22.0\nuse ./gateway\n")
                env = dict(os.environ, GOWORK=selection, GOPROXY="off", GOSUMDB="off",
                           GOTOOLCHAIN="local", GOFLAGS="-buildvcs=false",
                           GOCACHE=str(ROOT / ".tmp/gocache"))
                built = subprocess.run(
                    ["go", "build", "./internal/semhub/..."], cwd=clone, env=env,
                    capture_output=True, text=True, timeout=150,
                )
                self.assertEqual(0, built.returncode, built.stderr)
                with mock.patch.dict(os.environ, GOWORK=selection, GOFLAGS="-buildvcs=false"):
                    with self.assertRaisesRegex(ValueError, "vendored SemReg is not allowed"):
                        guard.parse_sources(clone)

    def test_disabled_workspace_vendor_is_ignored(self):
        clone = self.workspace_clone()
        self.write("go.work", "go 1.22.0\nuse ./gateway\n")
        self.write(f"vendor/{guard.ALLOWED_EXTERNAL[0]}/semreg/v1/base.go",
                   f'package neutral\nimport _ "{NATIVE}"\n')
        with mock.patch.dict(os.environ, GOWORK="off"):
            self.assertEqual([], guard.violations(guard.parse_sources(clone)))

    def test_buildable_alternate_module_file_fails_closed(self):
        semreg = guard.ALLOWED_EXTERNAL[0]
        native_module = NATIVE.rsplit("/", 1)[0]
        self.write("go.mod", f"module {guard.MODULE}\ngo 1.22.0\n")
        self.write("alternate.mod", f"module {guard.MODULE}\ngo 1.22.0\n"
                   f"require (\n {semreg} v0.0.0\n {native_module} v0.0.0\n)\n"
                   f"replace {semreg} => ./candidate\n"
                   f"replace {native_module} => ./native\n")
        self.write("candidate/go.mod", f"module {semreg}\ngo 1.22.0\n")
        self.write("candidate/semreg/v1/base.go", f'package neutral\nimport _ "{NATIVE}"\n')
        self.write("native/go.mod", f"module {native_module}\ngo 1.22.0\n")
        self.write("native/protocol/base.go", "package protocol\n")
        self.write("internal/semhub/base.go", f'package neutral\nimport _ "{semreg}/semreg/v1"\n')
        for modfile in ("alternate.mod", str(self.root / "alternate.mod")):
            with self.subTest(modfile=modfile):
                flags = f'-buildvcs=false "-modfile={modfile}"'
                env = dict(os.environ, GOWORK="off", GOPROXY="off", GOSUMDB="off",
                           GOTOOLCHAIN="local", GOFLAGS=flags,
                           GOCACHE=str(ROOT / ".tmp/gocache"))
                built = subprocess.run(
                    ["go", "build", "./internal/semhub/..."], cwd=self.root, env=env,
                    capture_output=True, text=True, timeout=150,
                )
                self.assertEqual(0, built.returncode, built.stderr)
                with mock.patch.dict(os.environ, GOWORK="off", GOFLAGS=flags):
                    with self.assertRaisesRegex(ValueError, "GOFLAGS -modfile is not allowed"):
                        self.errors()
                result = subprocess.run(
                    [sys.executable, str(ROOT / "scripts/check_semantic_imports.py"),
                     "--root", str(self.root)], env=env,
                    capture_output=True, text=True, timeout=160,
                )
                self.assertEqual(1, result.returncode)
                self.assertIn("GOFLAGS -modfile is not allowed", result.stderr)

    def test_vendored_semreg_fails_closed(self):
        semreg = guard.ALLOWED_EXTERNAL[0]
        self.write("internal/semhub/base.go", f'package neutral\nimport "{semreg}/semreg/v1"\n')
        self.write(f"vendor/{semreg}/semreg/v1/base.go",
                   f'package neutral\nimport "{NATIVE}"\n')
        with self.assertRaisesRegex(ValueError, "vendored SemReg is not allowed"):
            self.errors()
        result = subprocess.run([sys.executable, str(ROOT / "scripts/check_semantic_imports.py"),
                                 "--root", str(self.root)], capture_output=True, text=True, timeout=160)
        self.assertEqual(1, result.returncode)
        self.assertIn("vendored SemReg is not allowed", result.stderr)

    def test_symlinked_vendored_semreg_fails_closed(self):
        semreg = guard.ALLOWED_EXTERNAL[0]
        self.write("replacement/base.go", "package neutral\n")
        target = self.root / "vendor" / semreg
        target.parent.mkdir(parents=True)
        target.symlink_to(self.root / "replacement", target_is_directory=True)
        with self.assertRaisesRegex(ValueError, "vendored SemReg is not allowed"):
            self.errors()

    def test_unrelated_vendored_package_is_allowed(self):
        self.write("vendor/example.org/helper/base.go", f'package helper\nimport "{NATIVE}"\n')
        self.assertEqual([], self.errors())

    def test_exact_test_helper_rule_checks_its_production_sources(self):
        helper = next(iter(guard.ALLOWED_TEST_LOCAL))
        self.write("internal/semanticoperations/semoptest/base.go", "package fixture_test\n")
        self.write("internal/semhub/helper_test.go", f'package neutral_test\nimport "{helper}"\n')
        self.assertEqual([], self.errors())
        self.write("internal/semanticoperations/semoptest/base.go", f'package fixture_test\nimport "{NATIVE}"\n')
        self.assertIn(NATIVE, "\n".join(self.errors()))
        self.write("internal/semanticoperations/semoptest/base.go", "package fixture_test\n")
        self.write("internal/semhub/helper.go", f'package neutral_test\nimport "{helper}"\n')
        self.assertIn("dependency requires reviewed rule", "\n".join(self.errors()))

    def test_comments_and_strings_are_not_imports(self):
        self.write("internal/semhub/base.go", 'package neutral\n' +
                   f'// import "{NATIVE}"\n/* import "{NATIVE}" */\n' +
                   f'const example = `import "{NATIVE}"`\n')
        self.assertEqual([], self.errors())

    def test_directory_exclusions_are_repository_relative(self):
        for directory in ("node_modules", "vendor", ".tmp", ".git"):
            with self.subTest(directory=directory):
                filename = f"internal/semhub/{directory}/base.go"
                self.write(filename, f'package neutral\nimport _ "{NATIVE}"\n')
                self.assertIn(filename, "\n".join(self.errors()))
        for directory in (".git", ".tmp", "portal/web/node_modules", "vendor"):
            self.write(f"{directory}/broken.go", "not Go syntax\n")
        # Known asset locations remain excluded, but the protected paths above
        # must still be visited even when they have the same directory names.
        self.assertIn(NATIVE, "\n".join(self.errors()))

    def test_buildable_protected_node_modules_subpackage_is_checked(self):
        native_module = NATIVE.rsplit("/", 1)[0]
        self.write("go.mod", f"module {guard.MODULE}\ngo 1.22.0\n"
                   f"require {native_module} v0.0.0\n"
                   f"replace {native_module} => ./native\n")
        self.write("native/go.mod", f"module {native_module}\ngo 1.22.0\n")
        self.write("native/protocol/base.go", "package protocol\n")
        filename = "internal/semhub/node_modules/base.go"
        self.write(filename, f'package neutral\nimport _ "{NATIVE}"\n')
        env = dict(os.environ, GOWORK="off", GOPROXY="off", GOSUMDB="off",
                   GOTOOLCHAIN="local", GOFLAGS="-buildvcs=false",
                   GOCACHE=str(ROOT / ".tmp/gocache"))
        built = subprocess.run(
            ["go", "build", "./internal/semhub/..."], cwd=self.root, env=env,
            capture_output=True, text=True, timeout=150,
        )
        self.assertEqual(0, built.returncode, built.stderr)
        self.assertIn(filename, "\n".join(self.errors()))

    def test_unknown_external_and_prefix_impostor_fail_closed(self):
        for imp in ("example.org/neutral", "fictionalstdlib", "C",
                    guard.ALLOWED_EXTERNAL[0] + "-native/semreg/v1",
                    f"{guard.MODULE}/internal/semanticviewextra"):
            with self.subTest(imp=imp):
                self.write("internal/semhub/base.go", f'package neutral\nimport "{imp}"\n')
                self.assertIn("dependency requires reviewed rule", "\n".join(self.errors()))

    def test_missing_owner_and_unresolved_local_fail_closed(self):
        (self.root / "internal/semanticview/base.go").unlink()
        self.write("internal/semanticview/only_test.go", "package neutral_test\n")
        self.write("internal/semhub/base.go", f'package neutral\nimport "{guard.MODULE}/internal/semanticview"\n')
        errors = "\n".join(self.errors())
        self.assertIn("missing protected package", errors)
        self.assertIn("unresolved local dependency", errors)

    def test_resource_allocator_is_a_semantic_owner(self):
        owner = "internal/semgraph"
        self.assertIn(owner, guard.PROTECTED)
        self.write(f"{owner}/base.go", 'package semgraph\nimport (\n"crypto/sha256"\n"sync"\n"'
                   + guard.ALLOWED_EXTERNAL[0] + '/semreg/v1"\n)\n')
        self.assertEqual([], self.errors())
        for imp in (NATIVE, f"{guard.MODULE}/internal/modbusadapter",
                    f"{guard.MODULE}/cmd/gateway", f"{guard.MODULE}/internal/helper"):
            with self.subTest(imp=imp):
                self.write("internal/helper/base.go", "package helper\n")
                self.write("cmd/gateway/main.go", "package main\n")
                self.write(f"{owner}/base.go", f'package semgraph\nimport "{imp}"\n')
                errors = "\n".join(self.errors())
                self.assertIn(f"{guard.MODULE}/{owner} -> {imp}", errors)
        (self.root / owner / "base.go").unlink()
        self.assertIn(f"missing protected package: {guard.MODULE}/{owner}", "\n".join(self.errors()))

    def test_malformed_source_fails_parser(self):
        self.write("internal/semhub/broken.go", "package neutral\nimport (\n")
        with self.assertRaises(subprocess.CalledProcessError):
            self.errors()

    def test_cli_planted_import_has_nonzero_exit_and_both_packages(self):
        self.write("internal/semhub/base.go", f'package neutral\nimport "{NATIVE}"\n')
        result = subprocess.run([sys.executable, str(ROOT / "scripts/check_semantic_imports.py"),
                                 "--root", str(self.root)], capture_output=True, text=True, timeout=160)
        self.assertEqual(1, result.returncode)
        self.assertIn(f"{guard.MODULE}/internal/semhub", result.stderr)
        self.assertIn(NATIVE, result.stderr)


BOUNDARY = ROOT / "tools/boundary"
SNIPPETS = BOUNDARY / "testdata/snippets"
BOUNDARY_FILES = ("check.py", "vocabulary.json", "exceptions.json", "guard_operands.json")
REGISTRY = "internal/promotioncapture/contracts/leaf-promotion-captured-multi-leaf-registry-v1.json"
CAPTURE = "scripts/capture_m8_source_window.sh"
ADOPT = "scripts/adopt_modbusreg_packages.py"


def _load_boundary():
    sys.dont_write_bytecode = True
    spec = importlib.util.spec_from_file_location("boundary_check", BOUNDARY / "check.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


boundary = _load_boundary()
REAL_LEDGER = boundary.read_ledger_text((BOUNDARY / "ledger.tsv").read_text(encoding="utf-8"))


def ledger_text(rows):
    return "".join("\t".join(row) + "\n" for row in rows)


class BoundaryCheckTest(unittest.TestCase):
    def setUp(self):
        scratch = ROOT / ".tmp"
        scratch.mkdir(parents=True, exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(dir=scratch)
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)

    def tree(self, files):
        root = Path(tempfile.mkdtemp(dir=self.base))
        for rel, text in files.items():
            target = root / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text, encoding="utf-8")
        return root

    def install(self, root, rows):
        target = root / "tools/boundary"
        target.mkdir(parents=True, exist_ok=True)
        for name in BOUNDARY_FILES:
            shutil.copy(BOUNDARY / name, target / name)
        (target / "ledger.tsv").write_text(ledger_text(rows), encoding="utf-8")
        return root

    def rows_for(self, paths):
        return [row for row in REAL_LEDGER if row[0] in paths]

    def cli(self, root, event=None):
        env = {key: value for key, value in os.environ.items() if key != "GITHUB_EVENT_PATH"}
        env["PYTHONDONTWRITEBYTECODE"] = "1"
        if event is not None:
            env["GITHUB_EVENT_PATH"] = str(event)
        return subprocess.run([sys.executable, str(root / "tools/boundary/check.py")], cwd=root, env=env,
                              capture_output=True, text=True, timeout=600)

    def at(self, rel, text):
        return [item for item in boundary.scan(self.tree({rel: text})) if item["path"] == rel]

    def test_base_tree_passes_the_bare_cli(self):
        env = {key: value for key, value in os.environ.items() if key != "GITHUB_EVENT_PATH"}
        env["PYTHONDONTWRITEBYTECODE"] = "1"
        result = subprocess.run([sys.executable, str(BOUNDARY / "check.py")], cwd=ROOT, env=env,
                                capture_output=True, text=True, timeout=600)
        self.assertEqual(0, result.returncode, result.stderr)

    def test_shell_protocol_calls_ratchet_against_the_ledger(self):
        text = (ROOT / CAPTURE).read_text(encoding="utf-8")
        owners = {row[3] for row in REAL_LEDGER if row[0] == CAPTURE}
        self.assertEqual({"GG10"}, owners)
        root = self.install(self.tree({CAPTURE: text}), self.rows_for({CAPTURE}))
        self.assertEqual(0, self.cli(root).returncode)
        target = root / CAPTURE
        cases = {
            "new eeBUS call": text + "rpc scoped eebus.v1.services.list '{}' \"${staging}/extra.json\"\n",
            "duplicate ledgered call": text + "rpc public ebus.v1.registry.devices.list '{}' \"${staging}/ebus-devices.json\"\n",
            "deleted call, ledger kept": text.replace(
                "rpc scoped eebus.v1.topology.get '{}' \"${staging}/eebus-topology.json\"\n", "", 1),
        }
        for label, mutated in cases.items():
            with self.subTest(label=label):
                target.write_text(mutated, encoding="utf-8")
                result = self.cli(root)
                self.assertEqual(1, result.returncode)
                self.assertIn(CAPTURE, result.stderr)
        target.write_text(cases["deleted call, ledger kept"], encoding="utf-8")
        self.install(root, boundary.build_ledger(root))
        self.assertEqual(0, self.cli(root).returncode)

    def test_python_rewrite_tables_fail_on_unledgered_family_keys(self):
        text = (ROOT / ADOPT).read_text(encoding="utf-8")
        root = self.install(self.tree({ADOPT: text}), self.rows_for({ADOPT}))
        self.assertEqual(0, self.cli(root).returncode)
        for header in ("RENAMED: dict[str, tuple[tuple[str, str], ...]] = {",
                       "QUALIFIER_ONLY: dict[str, tuple[str, ...]] = {"):
            with self.subTest(header=header):
                self.assertIn(header, text)
                (root / ADOPT).write_text(text.replace(header, header + '\n    "fixturebrand": (),', 1), encoding="utf-8")
                result = self.cli(root)
                self.assertEqual(1, result.returncode)
                self.assertIn(ADOPT, result.stderr)
                self.assertIn("fixturebrand", result.stderr)
        renamed = text.replace('"growatt": (', '"growattx": (', 1)
        (root / ADOPT).write_text(renamed, encoding="utf-8")
        self.assertEqual(1, self.cli(root).returncode)
        self.install(root, boundary.build_ledger(root))
        self.assertEqual(0, self.cli(root).returncode)

    def test_ui_manufacturer_branches_fail_and_projection_passes(self):
        paths = [rel for rel, _ in boundary.scanned_files(ROOT) if rel.startswith(("ui/static/", "portal/web/src/"))]
        files = {rel: (ROOT / rel).read_text(encoding="utf-8") for rel in paths}
        self.assertNotIn("fixturebrand", boundary.load_vocabulary(BOUNDARY))
        root = self.install(self.tree(files), self.rows_for(set(paths)))
        self.assertEqual(0, self.cli(root).returncode)
        anchor = "function normalizeDevices(data) {\n"
        app = files["ui/static/app.js"]
        self.assertIn(anchor, app)
        cases = {
            "equality": '  const fixture = (device) => device.manufacturer === "Fixturebrand";\n',
            "reversed equality": '  const fixture = (device) => "Fixturebrand" !== device.manufacturer;\n',
            "switch case": '  switch (device.manufacturer) {\n    case "Fixturebrand":\n      break;\n  }\n',
        }
        for label, inserted in cases.items():
            with self.subTest(label=label):
                (root / "ui/static/app.js").write_text(app.replace(anchor, anchor + inserted, 1), encoding="utf-8")
                result = self.cli(root)
                self.assertEqual(1, result.returncode)
                self.assertIn("ui/static/app.js", result.stderr)
                self.assertIn("Fixturebrand", result.stderr)
        projection = '  const shown = device.manufacturer ? device.manufacturer : "unknown";\n'
        (root / "ui/static/app.js").write_text(app.replace(anchor, anchor + projection, 1), encoding="utf-8")
        self.assertEqual(0, self.cli(root).returncode)

    def test_token_rules_for_identifiers_strings_and_prose(self):
        positive = (SNIPPETS / "tokens_positive.go.txt").read_text(encoding="utf-8")
        found = {(item["kind"], item["item"]) for item in self.at("internal/fixture/tokens.go", positive)}
        for expected in (("name", "ebus"), ("name", "eebus"), ("name", "can"), ("name", "spine"), ("name", "ship")):
            self.assertIn(expected, found)
        for line in ('var a = "eBUS"', 'var b = "eeBUS"', "var eebusV1Mu = 1", "var CANBus = 2",
                     "var EEBUSDevice = 3", "var SPINERead = 4", "var SHIPSession = 5",
                     "var RegisterEEBusV1Provider = 6", 'var c = "CAN bus"', 'var d = "CAN frame"',
                     'var e = "CAN identifier"', 'var f = "can"'):
            with self.subTest(line=line):
                self.assertTrue(self.at("internal/fixture/one.go", "package fixture\n" + line + "\n"))
        negative = (SNIPPETS / "tokens_negative.go.txt").read_text(encoding="utf-8")
        self.assertEqual([], [i for i in self.at("internal/fixture/tokens.go", negative) if i["kind"] == "name"])

    def test_go_import_of_driver_module_fails_and_names_file(self):
        text = ('package fixture\n\nimport "github.com/Project-Helianthus/helianthus-ebusreg/registry"\n\n'
                "var _ = registry.X\n")
        root = self.install(self.tree({"internal/fixture/x.go": text}), [])
        result = self.cli(root)
        self.assertEqual(1, result.returncode)
        self.assertIn("internal/fixture/x.go", result.stderr)
        self.assertIn("helianthus-ebusreg/registry", result.stderr)

    def test_drivers_and_matter_exceptions_are_exact(self):
        drivers = (SNIPPETS / "drivers_ok.go.txt").read_text(encoding="utf-8")
        matter = (SNIPPETS / "matter_enabled.go.txt").read_text(encoding="utf-8")
        cases = [
            ("cmd/gateway/drivers.go", drivers, True),
            ("cmd/gateway/drivers.go", drivers.replace(
                "\n)\n", '\n\tmatterdriver "github.com/Project-Helianthus/helianthus-matterreg/driver"\n)\n', 1), False),
            ("cmd/gateway/drivers.go", drivers.replace("helianthus-ebusreg/driver", "helianthus-ebusreg", 1), False),
            ("cmd/gateway/drivers.go", drivers.replace("helianthus-ebusreg/driver", "helianthus-ebusreg/registry", 1), False),
            ("cmd/gateway/extra.go", drivers, False),
            ("cmd/gateway/matter_profile_enabled.go", matter, True),
            ("cmd/gateway/matter_profile_enabled.go", matter.replace("!nomatter", "nomatter"), False),
            ("cmd/gateway/matter_profile_disabled.go", matter, False),
            ("cmd/gateway/matter_profile_enabled.go", matter.replace(
                "helianthus-matterreg/driver", "helianthus-matterreg", 1), False),
            ("cmd/gateway/matter_profile_enabled.go", matter.replace(
                "helianthus-matterreg/driver", "helianthus-matterreg/other", 1), False),
            ("cmd/gateway/matter_profile_enabled.go", matter.replace(
                "\n)\n", '\n\t"github.com/Project-Helianthus/helianthus-ebusgo/protocol"\n)\n', 1), False),
            ("cmd/gateway/matter_profile_enabled.go", matter + '\nvar flag = "-matter-endpoint-store"\n', False),
            ("cmd/gateway/matter_extra.go", "package main\n", False),
            ("cmd/gateway/profile_managed_driver_test.go", "package main\n", True),
        ]
        for rel, text, clean in cases:
            with self.subTest(path=rel, clean=clean):
                self.assertEqual(clean, self.at(rel, text) == [])

    def test_register_table_items_are_exact_mappings(self):
        text = (ROOT / REGISTRY).read_text(encoding="utf-8")
        root = self.install(self.tree({REGISTRY: text}), self.rows_for({REGISTRY}))
        self.assertEqual(0, self.cli(root).returncode)

        def entry(data, path):
            return next(item for item in data["candidate_catalog"] if item.get("semantic_path") == path)

        def new_row(data):
            data["candidate_catalog"].append(dict(entry(data, "/dhw/temperature_c"),
                                                  candidate_id="m7-fixture-0001", semantic_path="/fixture/row"))

        def brand(data):
            entry(data, "/system/gateway_brand")["eebus_source"]["field_path"] = "vendorName"

        def mapping(data):
            entry(data, "/zones/zone_2/operation_mode_changeable")["eebus_source"]["exact_mapping"]["pairs"][0]["normalized"] = True

        for label, mutate in (("new selector row", new_row), ("brandName to vendorName", brand),
                              ("raw false mapping", mapping)):
            with self.subTest(label=label):
                data = json.loads(text)
                mutate(data)
                (root / REGISTRY).write_text(json.dumps(data, indent=2, ensure_ascii=False), encoding="utf-8")
                result = self.cli(root)
                self.assertEqual(1, result.returncode)
                self.assertIn(REGISTRY, result.stderr)
        data = json.loads(text)
        entry(data, "/system/gateway_brand")["retirement_state"] = "FIXTURE_RETIRED"
        entry(data, "/system/gateway_brand")["source_status"] = "FIXTURE_STATUS"
        (root / REGISTRY).write_text(json.dumps(data, indent=2, ensure_ascii=False), encoding="utf-8")
        self.assertEqual(0, self.cli(root).returncode)
        reordered = json.loads(text, object_pairs_hook=lambda pairs: dict(reversed(pairs)))
        (root / REGISTRY).write_text(json.dumps(reordered, indent=4, ensure_ascii=False), encoding="utf-8")
        self.assertEqual(0, self.cli(root).returncode)

    def test_numeric_keyed_register_rows_are_register_table_findings(self):
        probe = "package fixture\n\nvar m = map[uint16][]uint16{0x0001: identity, 0x000d: {0, 0}}\n"
        found = [i["item"] for i in self.at("internal/fixture/n.go", probe) if i["kind"] == "register_table"]
        self.assertEqual(["0x0001:identity", "0x000d:{0,0}"], found)

    def test_numeric_row_added_to_existing_b509_table_fails_the_cli(self):
        path = "cmd/gateway/controllerless_site_acceptance_test.go"
        text = (ROOT / path).read_text(encoding="utf-8")
        anchor = "var controllerlessHeatSourceRegisters = map[uint16][]byte{\n"
        self.assertIn(anchor, text)
        root = self.install(self.tree({path: text}), self.rows_for({path}))
        self.assertEqual(0, self.cli(root).returncode)
        (root / path).write_text(text.replace(anchor, anchor + "\t0x0F99: {0x01},\n", 1), encoding="utf-8")
        result = self.cli(root)
        self.assertEqual(1, result.returncode)
        self.assertIn(path, result.stderr)
        self.assertIn("0x0F99", result.stderr)

    def test_constant_block_uint16_addresses_are_register_table_findings(self):
        probe = ("package fixture\n\nconst (\n\tsystem_flow_temperature = uint16(0x004B)\n"
                 "\tsystem_fixture uint16 = 0x0099\n\tregulatorInstance = byte(0x00)\n)\n")
        found = [i["item"] for i in self.at("internal/fixture/c.go", probe) if i["kind"] == "register_table"]
        self.assertEqual(["system_flow_temperature=uint16(0x004B)", "system_fixture=uint16(0x0099)",
                          "regulatorInstance=byte(0x00)"], found)

    def test_byte_constants_byte_keyed_and_string_keyed_address_maps_are_register_table_findings(self):
        probe = ("package fixture\n\nconst (\n\topcodeRead = byte(0x06)\n\topcodeWrite byte = 0x07\n)\n"
                 "var names = map[byte]string{\n\t0x05: \"pump\",\n}\n"
                 "var specs = map[string]uint16{\"flow_temperature\": 0x004B}\n"
                 "var frame = Frame{Primary: 0xB5, Secondary: 0x24}\n")
        found = [i["item"] for i in self.at("cmd/gateway/fixture.go", probe) if i["kind"] == "register_table"]
        self.assertEqual(["opcodeRead=byte(0x06)", "opcodeWrite=byte(0x07)", '0x05:"pump"',
                          '"flow_temperature":0x004B', "Frame{Primary:0xB5,Secondary:0x24}"], found)

    def test_edited_byte_address_or_added_byte_keyed_row_fails_the_cli(self):
        path = "cmd/gateway/semantic_vaillant_registers.go"
        text = (ROOT / path).read_text(encoding="utf-8")
        opcode = "vail" + "lantB524OpcodeRead"
        read = f"{opcode}       = byte(0x06)\n"
        cases = (
            (read, f"{opcode}       = byte(0x16)\n", f"{opcode}=byte(0x16)"),
            (read, read + "\tresetOpcode = byte(0x0F)\n", "resetOpcode=byte(0x0F)"),
            ("var b555HCNames = map[byte]string{\n", "var b555HCNames = map[byte]string{\n\t0x05: \"pump\",\n",
             '0x05:"pump"'),
        )
        root = self.install(self.tree({path: text}), self.rows_for({path}))
        self.assertEqual(0, self.cli(root).returncode)
        for old, new, item in cases:
            with self.subTest(item=item):
                self.assertEqual(1, text.count(old))
                (root / path).write_text(text.replace(old, new, 1), encoding="utf-8")
                result = self.cli(root)
                self.assertEqual(1, result.returncode, result.stdout)
                self.assertIn(path, result.stderr)
                self.assertIn(item, result.stderr)
                (root / path).write_text(text, encoding="utf-8")

    def test_changed_name_to_address_map_and_new_address_file_fail_the_cli(self):
        path = "graphql/naming_contract_test.go"
        text = (ROOT / path).read_text(encoding="utf-8")
        old = '"circuit_cooling_enabled": 0x0006}'
        self.assertEqual(1, text.count(old))
        root = self.install(self.tree({path: text}), self.rows_for({path}))
        self.assertEqual(0, self.cli(root).returncode)
        (root / path).write_text(text.replace(old, '"circuit_cooling_enabled": 0x0007}', 1), encoding="utf-8")
        result = self.cli(root)
        self.assertEqual(1, result.returncode)
        self.assertIn('"circuit_cooling_enabled":0x0007', result.stderr)
        (root / path).write_text(text, encoding="utf-8")
        added = root / "graphql/new_addresses.go"
        added.write_text('package graphql\n\nvar addrs = map[string]uint16{"flow_temperature": 0x004B}\n',
                         encoding="utf-8")
        result = self.cli(root)
        self.assertEqual(1, result.returncode)
        self.assertIn("graphql/new_addresses.go", result.stderr)
        self.assertIn('"flow_temperature":0x004B', result.stderr)

    def test_map_rows_with_typed_and_call_values_are_register_table_findings(self):
        probe = ("package fixture\n\nvar m = map[uint16][]byte{\n\t0x0F99: []byte{0x01},\n"
                 "\tsystem_new_point: vpF32(1),\n\t// a comment, with a brace }\n\tsystem_old: {0x00},\n}\n"
                 "var n = map[uint16][]uint16{0x0001: identity, 0x000d: {0, 0}}\n")
        found = [i["item"] for i in self.at("internal/fixture/r.go", probe) if i["kind"] == "register_table"]
        self.assertEqual(["0x0F99:[]byte{0x01}", "system_new_point:vpF32(1)", "system_old:{0x00}",
                          "0x0001:identity", "0x000d:{0,0}"], found)

    def test_result_type_of_a_function_is_not_counted_as_a_register_row(self):
        probe = ("package fixture\n\nfunc f() map[uint16][]uint16 {\n"
                 "\treturn map[uint16][]uint16{0x0001: identity}\n}\n")
        found = [i["item"] for i in self.at("internal/fixture/f.go", probe) if i["kind"] == "register_table"]
        self.assertEqual(["0x0001:identity"], found)

    def test_added_constant_or_changed_address_in_register_block_fails_the_cli(self):
        path = "cmd/gateway/semantic_vaillant_registers.go"
        text = (ROOT / path).read_text(encoding="utf-8")
        anchor = "= uint16(0x004B)\n"
        self.assertEqual(1, text.count(anchor))
        root = self.install(self.tree({path: text}), self.rows_for({path}))
        self.assertEqual(0, self.cli(root).returncode)
        (root / path).write_text(text.replace(anchor, anchor + "\tsystem_new_register = uint16(0x0099)\n", 1),
                                 encoding="utf-8")
        result = self.cli(root)
        self.assertEqual(1, result.returncode)
        self.assertIn(path, result.stderr)
        self.assertIn("system_new_register=uint16(0x0099)", result.stderr)
        (root / path).write_text(text.replace(anchor, "= uint16(0x004C)\n", 1), encoding="utf-8")
        result = self.cli(root)
        self.assertEqual(1, result.returncode)
        self.assertIn("system_flow_temperature=uint16(0x004C)", result.stderr)

    def register_items(self, rel, text):
        return [item["item"] for item in self.at(rel, text) if item["kind"] == "register_table"]

    def replace_fails_until_ledger_matches(self, path, text, old, new, item):
        self.assertEqual(1, text.count(old))
        mutated = text.replace(old, new, 1)
        before = self.register_items(path, text)
        after = self.register_items(path, mutated)
        self.assertNotEqual(sorted(before), sorted(after))
        self.assertIn(item, after)
        self.assertNotIn(item, before)
        root = self.install(self.tree({path: text}), self.rows_for({path}))
        self.assertEqual(0, self.cli(root).returncode, path)
        (root / path).write_text(mutated, encoding="utf-8")
        result = self.cli(root)
        self.assertEqual(1, result.returncode, result.stdout)
        self.assertIn(path, result.stderr)
        self.assertIn(item, result.stderr)
        self.install(root, boundary.build_ledger(root))
        self.assertEqual(0, self.cli(root).returncode, result.stderr)

    def test_identifier_address_selectors_change_the_register_multiset(self):
        discovery = "cmd/gateway/semantic_vaillant_discovery.go"
        discovered = self.register_items(discovery, (ROOT / discovery).read_text(encoding="utf-8"))
        numeric = "{opcode:localRegulator.opcode,group:localRegulator.group,instance:regulatorInstance,addr:0x0001}"
        named = "{opcode:localDHW.opcode,group:localDHW.group,instance:dhwInstance,addr:dhw_operation_mode}"
        self.assertIn(numeric, discovered)
        self.assertIn(named, discovered)
        boiler = "cmd/gateway/semantic_vaillant_boiler.go"
        boiled = self.register_items(boiler, (ROOT / boiler).read_text(encoding="utf-8"))
        flow = ("{field:boilerStatusFieldFlowTemperature,decoder:boilerStatusRegisterDecoderFloat32,"
                "opcode:localRegulator.opcode,group:localRegulator.group,instance:regulatorInstance,"
                "addr:system_flow_temperature,}")
        self.assertIn(flow, boiled)
        golden = "cmd/gateway/vaillant_publication_golden_test.go"
        text = (ROOT / golden).read_text(encoding="utf-8")
        selector = ("{opcode: vaillantB524OpcodeLocal, group: localZones.group, instance: 0x01, "
                    "addr: zone_current_temp}")
        variants = {
            "opcode": selector.replace("vaillantB524OpcodeLocal", "vaillantB524OpcodeRead"),
            "group": selector.replace("localZones.group", "localDHW.group"),
            "instance": selector.replace("instance: 0x01", "instance: 0x02"),
            "addr": selector.replace("zone_current_temp", "zone_target_temp"),
        }
        for field, variant in variants.items():
            with self.subTest(field=field):
                self.replace_fails_until_ledger_matches(
                    golden, text, selector, variant, "".join(variant.split()))
        self.replace_fails_until_ledger_matches(
            golden, text, "boiler_b509_flow_temperature", "boiler_b509_fixture_flow",
            "boiler_b509_fixture_flow")

    def test_standalone_native_constants_change_the_register_multiset(self):
        probe = ("package fixture\n\nconst (\n\tsystem_flow_temperature = uint16(0x004B)\n)\n"
                 "const OpcodeLocal = byte(0x02)\nconst fixtureTyped byte = 0x07\n")
        found = self.register_items("internal/vaillant/systemconfig/fixture.go", probe)
        self.assertEqual(["system_flow_temperature=uint16(0x004B)", "OpcodeLocal=byte(0x02)",
                          "fixtureTyped=byte(0x07)"], found)
        path = "internal/vaillant/systemconfig/write_specs.go"
        text = (ROOT / path).read_text(encoding="utf-8")
        items = self.register_items(path, text)
        self.assertIn("OpcodeLocal=byte(0x02)", items)
        self.assertIn("Instance=byte(0x00)", items)
        grouped = ('"room_humidity_limit_maximum":{RegisterGroup:0x00,RegisterAddr:0x000E,'
                   "ValueType:ValueUint16,Min:30,Max:80}")
        self.assertIn(grouped, items)
        anchor = "const OpcodeLocal = byte(0x02)\n"
        cases = (
            ("name", anchor + "const fixtureSelector = byte(0x99)\n", "fixtureSelector=byte(0x99)"),
            ("value", "const OpcodeLocal = byte(0x03)\n", "OpcodeLocal=byte(0x03)"),
            ("call type", "const OpcodeLocal = uint16(0x02)\n", "OpcodeLocal=uint16(0x02)"),
            ("typed name", anchor + "const fixtureTyped byte = 0x99\n", "fixtureTyped=byte(0x99)"),
            ("typed type", anchor + "const fixtureTyped uint8 = 0x99\n", "fixtureTyped=uint8(0x99)"),
            ("typed value", anchor + "const fixtureTyped byte = 0x98\n", "fixtureTyped=byte(0x98)"),
        )
        for field, replacement, item in cases:
            with self.subTest(field=field):
                self.replace_fails_until_ledger_matches(path, text, anchor, replacement, item)

    def test_preexisting_register_row_may_be_recorded_against_the_base(self):
        path = "cmd/gateway/semantic_vaillant_probe.go"
        body = ("probeSelector{opcode: opLocal, group: zoneGroup, instance: 0x01, "
                "addr: zone_current_temp}")
        source = "package main\n\nvar rows = []probeSelector{" + body + "}\n"
        item = "".join(body.split())
        name = (path, "name", "vaillant", "GG08")
        register = (path, "register_table", item, "GG08")
        present = [{"path": path, "kind": "register_table", "item": item, "owner": "GG08"}]
        self.assertEqual([], boundary.base_ratchet([register], [], present))
        changed = item.replace("zone_current_temp", "zone_target_temp")
        for mutated in (
            (path, "register_table", changed, "GG08"),
            (path, "register_table", item, "GG09"),
            (path, "name", item, "GG08"),
            ("cmd/gateway/other_vaillant.go", "register_table", item, "GG08"),
        ):
            with self.subTest(mutated=mutated):
                self.assertNotEqual([], boundary.base_ratchet([mutated], [], present))
        repo = self.repo()
        base = self.commit(repo, {**self.boundary_files(), "tools/boundary/ledger.tsv": ledger_text([name]),
                                  path: source}, "base")
        self.commit(repo, {"tools/boundary/ledger.tsv": ledger_text([name, register])}, "record the row")
        allowed = self.cli(repo, self.event({"before": base}))
        self.assertEqual(0, allowed.returncode, allowed.stderr)
        edited = source.replace("zone_current_temp", "zone_target_temp")
        self.commit(repo, {path: edited, "tools/boundary/ledger.tsv": ledger_text(
            [name, (path, "register_table", changed, "GG08")])}, "change the address")
        rejected = self.cli(repo, self.event({"before": base}))
        self.assertEqual(1, rejected.returncode)
        self.assertIn(path, rejected.stderr)
        self.assertIn(changed, rejected.stderr)
        self.assertIn("exceeds the base revision", rejected.stderr)

    def test_added_map_row_with_call_or_typed_value_fails_the_cli(self):
        cases = {
            "cmd/gateway/vaillant_publication_site_test.go":
                ("for addr, value := range map[uint16][]byte{\n",
                 "\t\tsystem_fixture_point: vpF32(1),\n", "system_fixture_point:vpF32(1)"),
            "cmd/gateway/controllerless_site_acceptance_test.go":
                ("var controllerlessHeatSourceRegisters = map[uint16][]byte{\n",
                 "\t0x0F99: []byte{0x01},\n", "0x0F99:[]byte{0x01}"),
        }
        for path, (anchor, row, item) in cases.items():
            text = (ROOT / path).read_text(encoding="utf-8")
            self.assertEqual(1, text.count(anchor), path)
            root = self.install(self.tree({path: text}), self.rows_for({path}))
            self.assertEqual(0, self.cli(root).returncode, path)
            (root / path).write_text(text.replace(anchor, anchor + row, 1), encoding="utf-8")
            result = self.cli(root)
            self.assertEqual(1, result.returncode, path)
            self.assertIn(path, result.stderr)
            self.assertIn(item, result.stderr)

    def test_growattcan_package_import_takes_the_gg07_owner(self):
        imported = "github.com/Project-Helianthus/helianthus-gateway/internal/growattcanclassify"
        text = f'package main\n\nimport "{imported}"\n\nvar _ = 1\n'
        found = [(i["item"], i["owner"]) for i in self.at("cmd/gateway/fixture.go", text)
                 if i["kind"] == "import"]
        self.assertEqual([(imported, "GG07")], found)
        owners = {row[3] for row in REAL_LEDGER if row[1] == "import" and row[2] == imported}
        self.assertEqual({"GG07"}, owners)

    def test_item_60_approval_is_quoted_verbatim_beside_its_entry(self):
        exceptions = json.loads((BOUNDARY / "exceptions.json").read_text(encoding="utf-8"))
        question = ("Pentru implementare, aprobi ca fișierul existent `cmd/gateway/matter_profile_enabled.go` "
                    "să importe doar constructorul public `matterreg`, ca excepție suplimentară față de `drivers.go`?")
        answer = "Da, aprob importul constructorului în acel fișier (recomandat)"
        self.assertEqual(question, exceptions["item_60_question"])
        self.assertEqual(answer, exceptions["item_60_answer"])
        matter = next(e for e in exceptions["entries"] if e["path"] == "cmd/gateway/matter_profile_enabled.go")
        self.assertIn(f"«{question}» Answer: «{answer}»", matter["approval"])

    def test_unclassified_protocol_import_path_is_a_name_finding(self):
        for imported, token in (("github.com/enbility/eebus-go/api", "eebus"),
                                ("github.com/enbility/ship-go/api", "ship")):
            with self.subTest(imported=imported):
                text = f'package fixture\n\nimport api "{imported}"\n\nvar _ = api.X\n'
                root = self.install(self.tree({"internal/fixture/x.go": text}), [])
                result = self.cli(root)
                self.assertEqual(1, result.returncode)
                self.assertIn("internal/fixture/x.go", result.stderr)
                self.assertIn(f"name {token!r}", result.stderr)

    def test_vendor_identifier_declaration_forms_are_findings(self):
        for line in ("const manufacturerID uint16 = 0x1234", "manufacturerID := 0x1234",
                     "const manufacturerID = 0x1234"):
            with self.subTest(line=line):
                text = "package fixture\n\nfunc f() {\n\t" + line + "\n}\n"
                found = [i["item"] for i in self.at("internal/fixture/v.go", text) if i["kind"] == "vendor"]
                self.assertEqual(["manufacturerid=0x1234"], found)

    def test_manufacturer_branch_on_any_receiver_is_a_name_finding(self):
        cases = {
            "equality": 'if (item.manufacturer === "Fixturebrand") { run(); }\n',
            "switch case": 'switch (item.manufacturer) {\n  case "Fixturebrand":\n    break;\n}\n',
        }
        for label, text in cases.items():
            with self.subTest(label=label):
                found = [i["item"] for i in self.at("portal/web/src/app.js", text) if i["kind"] == "name"]
                self.assertIn("Fixturebrand", found)
        shown = 'const vendor = escapeHtml(item.manufacturer || "unknown");\n'
        self.assertEqual([], [i for i in self.at("portal/web/src/app.js", shown) if i["kind"] == "name"])

    def test_go_register_map_entries_are_register_table_findings(self):
        probe = ('package fixture\n\nvar m = map[string]struct{ RegisterAddr uint16 }{"flow_temperature": '
                 "{RegisterAddr: 0x0099}}\n")
        found = [i for i in self.at("internal/fixture/m.go", probe) if i["kind"] == "register_table"]
        self.assertEqual(1, len(found))
        path = "internal/vaillant/systemconfig/write_specs.go"
        text = (ROOT / path).read_text(encoding="utf-8")
        root = self.install(self.tree({path: text}), self.rows_for({path}))
        self.assertEqual(0, self.cli(root).returncode)
        (root / path).write_text(text.replace("RegisterAddr: 0x0014", "RegisterAddr: 0x0015", 1), encoding="utf-8")
        result = self.cli(root)
        self.assertEqual(1, result.returncode)
        self.assertIn(path, result.stderr)
        (root / path).write_text(text, encoding="utf-8")
        new_file = self.install(self.tree({"internal/fixture/m.go": probe}), [])
        result = self.cli(new_file)
        self.assertEqual(1, result.returncode)
        self.assertIn("internal/fixture/m.go", result.stderr)

    def test_explicitly_typed_register_entries_are_register_table_findings(self):
        path = "internal/vaillant/systemconfig/write_specs.go"
        text = (ROOT / path).read_text(encoding="utf-8")
        anchor = "var systemConfigFieldSpecs = map[string]ConfigFieldSpec{\n"
        self.assertEqual(1, text.count(anchor))
        typed = text.replace(anchor, anchor + '\t"fixture_temperature": ConfigFieldSpec{RegisterGroup: 0x00, '
                             'RegisterAddr: 0x0099, ValueType: ValueFloat32},\n', 1)
        root = self.install(self.tree({path: text}), self.rows_for({path}))
        self.assertEqual(0, self.cli(root).returncode)
        (root / path).write_text(typed, encoding="utf-8")
        result = self.cli(root)
        self.assertEqual(1, result.returncode)
        self.assertIn(path, result.stderr)
        self.assertIn("fixture_temperature", result.stderr)

    def test_protocol_address_mappings_are_register_table_findings(self):
        cases = {
            "graphql/mutations.go": [
                ("addr: 0x000F", "addr: 0x0030", 1),
                ('var circuitConfigFieldSpecs = map[string]configFieldSpec{\n',
                 'var circuitConfigFieldSpecs = map[string]configFieldSpec{\n'
                 '\t"circuit_probe": {group: 0x02, addr: 0x0030, valueType: configValueBoolU8},\n', 1),
            ],
            "matter/projection.go": [
                ("Cluster: 0x0090, Kind: ElementAttribute", "Cluster: 0x0091, Kind: ElementAttribute", 1),
                ("ElementID: 0x0005}", "ElementID: 0x0006}", 1),
            ],
            "cmd/gateway/controllerless_site_acceptance_test.go": [
                ("var controllerlessHeatSourceRegisters = map[uint16][]byte{\n",
                 "var controllerlessHeatSourceRegisters = map[uint16][]byte{\n"
                 "\tboiler_b509_fixture: {0x01},\n", 1),
                ("boiler_b509_flame_active:       {0xF0},", "boiler_b509_flame_active:       {0xF1},", 1),
            ],
        }
        for path, edits in cases.items():
            text = (ROOT / path).read_text(encoding="utf-8")
            root = self.install(self.tree({path: text}), self.rows_for({path}))
            self.assertEqual(0, self.cli(root).returncode, path)
            for old, new, count in edits:
                with self.subTest(path=path, edit=new):
                    self.assertEqual(1, text.count(old))
                    (root / path).write_text(text.replace(old, new, count), encoding="utf-8")
                    result = self.cli(root)
                    self.assertEqual(1, result.returncode, result.stdout)
                    self.assertIn(path, result.stderr)
                    self.assertIn("register_table", result.stderr)
                    (root / path).write_text(text, encoding="utf-8")

    def test_matter_closure_required_set_is_a_guard_operand_not_debt(self):
        path = "scripts/check_no_matter_profile.py"
        real = (ROOT / path).read_text(encoding="utf-8")
        line = 'required = {MODULE + suffix for suffix in ("/matter", "/matter/feedv1", "/matter/libraryhost")}'
        self.assertEqual([], [item for item in self.at(path, real) if item["text"] == line])
        root = self.install(self.tree({path: real}), self.rows_for({path}))
        self.assertEqual(0, self.cli(root).returncode)
        product = "cmd/gateway/fixture_closure.go"
        (root / product).parent.mkdir(parents=True, exist_ok=True)
        (root / product).write_text('package main\n\nvar required = "/matter/feedv1"\n', encoding="utf-8")
        result = self.cli(root)
        self.assertEqual(1, result.returncode)
        self.assertIn(product, result.stderr)

    def test_go_fabric_import_in_any_module_is_an_import_finding(self):
        for imported in ("github.com/project-chip/connectedhomeip/src/controller/go-fabric",
                         "example.invalid/go-fabric"):
            with self.subTest(imported=imported):
                text = f'package fixture\n\nimport "{imported}"\n\nvar _ = 1\n'
                found = [i for i in self.at("internal/fixture/fabric.go", text) if i["kind"] == "import"]
                self.assertEqual([(imported, "GG11")], [(i["item"], i["owner"]) for i in found])

    def test_gateway_protocol_packages_and_subpackages_are_import_findings(self):
        gateway = "github.com/Project-Helianthus/helianthus-gateway"
        for imported, owner in (("internal/adaptermux/v8classifier", "GG08"), ("internal/adaptermux", "GG08"),
                                ("internal/transport", "GG08"), ("internal/transport/sub", "GG08")):
            with self.subTest(imported=imported):
                text = f'package fixture\n\nimport "{gateway}/{imported}"\n\nvar _ = 1\n'
                found = [i for i in self.at("cmd/gateway/fixture.go", text) if i["kind"] == "import"]
                self.assertEqual([(f"{gateway}/{imported}", owner)], [(i["item"], i["owner"]) for i in found])
        neutral = f'package fixture\n\nimport "{gateway}/internal/semhub"\n\nvar _ = 1\n'
        self.assertEqual([], [i for i in self.at("cmd/gateway/fixture.go", neutral) if i["kind"] == "import"])

    def test_classified_gateway_package_without_owner_fails_the_scan(self):
        unowned = (("internal/transport", None),)
        with mock.patch.object(boundary, "GATEWAY_PROTOCOL_PACKAGES", unowned):
            with self.assertRaisesRegex(ValueError, "internal/transport"):
                boundary.scan(self.tree({"internal/fixture/x.go": "package fixture\n"}))

    def test_raw_string_driver_imports_are_import_findings(self):
        single = "package fixture\n\nimport `github.com/Project-Helianthus/helianthus-ebusgo/protocol`\n"
        block = ("package fixture\n\nimport (\n\t`github.com/Project-Helianthus/helianthus-ebusreg/driver`\n"
                 "\t\"fmt\"\n)\n")
        for label, text, imported in (("single", single, "github.com/Project-Helianthus/helianthus-ebusgo/protocol"),
                                      ("block", block, "github.com/Project-Helianthus/helianthus-ebusreg/driver")):
            with self.subTest(label=label):
                found = [i for i in self.at("internal/fixture/raw.go", text) if i["kind"] == "import"]
                self.assertEqual([(imported, "GG08")], [(i["item"], i["owner"]) for i in found])

    def test_graphql_ebus_findings_are_owned_by_gg13(self):
        text = ('package fixture\n\nimport "github.com/Project-Helianthus/helianthus-ebusgo/types"\n\n'
                'var v = "vaillant"\nvar m = "modbus"\n')
        owners = {(i["kind"], i["item"]): i["owner"] for i in self.at("graphql/fixture.go", text)}
        self.assertEqual("GG13", owners[("import", "github.com/Project-Helianthus/helianthus-ebusgo/types")])
        self.assertEqual("GG13", owners[("name", "vaillant")])
        self.assertEqual("GG09", owners[("name", "modbus")])
        self.assertEqual([], [row for row in REAL_LEDGER if row[0].startswith("graphql/") and row[3] == "GG08"])

    def test_bare_command_without_event_scans_the_working_tree(self):
        clean = self.install(self.tree({"internal/fixture/x.go": "package fixture\n"}), [])
        self.assertEqual(0, self.cli(clean).returncode)
        files = {"internal/fixture/x.go": "package fixture\n", "internal/fixture/y.go": 'package fixture\n\nvar a = "eBUS"\n'}
        root = self.install(self.tree(files), [("internal/fixture/y.go", "name", "ebus", "GG08")])
        self.assertEqual(0, self.cli(root).returncode)
        (root / "internal/fixture/y.go").write_text("package fixture\n", encoding="utf-8")
        result = self.cli(root)
        self.assertEqual(1, result.returncode)
        self.assertIn("ledger line no longer matches", result.stderr)
        self.install(root, [])
        self.assertEqual(0, self.cli(root).returncode)

    def repo(self):
        root = Path(tempfile.mkdtemp(dir=self.base))
        subprocess.run(["git", "-C", str(root), "init", "-q"], check=True, capture_output=True)
        return root

    def boundary_files(self):
        return {f"tools/boundary/{name}": (BOUNDARY / name).read_text(encoding="utf-8") for name in BOUNDARY_FILES}

    def commit(self, root, files, message):
        for rel, text in files.items():
            target = root / rel
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text, encoding="utf-8")
        git = ["git", "-C", str(root), "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid",
               "-c", "commit.gpgsign=false"]
        subprocess.run([*git, "add", "-A"], check=True, capture_output=True)
        subprocess.run([*git, "commit", "-q", "--allow-empty", "-m", message], check=True, capture_output=True)
        return subprocess.run(["git", "-C", str(root), "rev-parse", "HEAD"], check=True,
                              capture_output=True, text=True).stdout.strip()

    def event(self, payload):
        path = Path(tempfile.mkdtemp(dir=self.base)) / "event.json"
        path.write_text(json.dumps(payload), encoding="utf-8")
        return path

    def test_ratchet_rejects_added_debt_on_push_and_stacked_restore(self):
        plain = "package fixture\n"
        debt = "package fixture\n\nvar CANBusID = 1\n"
        rows = [("internal/fixture/x.go", "name", "can", "GG07"), ("internal/fixture/x.go", "name", "canbus", "GG07")]
        repo = self.repo()
        base = self.commit(repo, {**self.boundary_files(), "tools/boundary/ledger.tsv": "",
                                  "internal/fixture/x.go": plain}, "base")
        self.commit(repo, {"internal/fixture/x.go": debt, "tools/boundary/ledger.tsv": ledger_text(rows)},
                    "add debt with ledger lines")
        result = self.cli(repo, self.event({"before": base}))
        self.assertEqual(1, result.returncode)
        self.assertIn("internal/fixture/x.go", result.stderr)
        self.assertIn("exceeds the base revision", result.stderr)

        stack_root = self.repo()
        origin = self.commit(stack_root, {**self.boundary_files(), "tools/boundary/ledger.tsv": ledger_text(rows),
                                          "internal/fixture/x.go": debt}, "origin with debt")
        parent = self.commit(stack_root, {"internal/fixture/x.go": plain, "tools/boundary/ledger.tsv": ""},
                             "parent removes debt and lines")
        self.commit(stack_root, {"internal/fixture/x.go": debt, "tools/boundary/ledger.tsv": ledger_text(rows)},
                    "child restores debt and lines")
        child = self.cli(stack_root, self.event({"pull_request": {"base": {"sha": parent}}}))
        self.assertEqual(1, child.returncode)
        self.assertIn("internal/fixture/x.go", child.stderr)
        self.assertEqual(0, self.cli(stack_root, self.event({"pull_request": {"base": {"sha": origin}}})).returncode)

    def test_ratchet_shrink_owner_reassignment_and_seeding(self):
        debt = "package fixture\n\nvar CANBusID = 1\n"
        rows = [("internal/fixture/x.go", "name", "can", "GG07"), ("internal/fixture/x.go", "name", "canbus", "GG07")]
        shrink = self.repo()
        before = self.commit(shrink, {**self.boundary_files(), "tools/boundary/ledger.tsv": ledger_text(rows),
                                      "internal/fixture/x.go": debt}, "debt with lines")
        self.commit(shrink, {"internal/fixture/x.go": "package fixture\n", "tools/boundary/ledger.tsv": ""},
                    "debt and lines removed")
        self.assertEqual(0, self.cli(shrink, self.event({"before": before})).returncode)

        reown = self.repo()
        before = self.commit(reown, {**self.boundary_files(), "tools/boundary/ledger.tsv": ledger_text(rows),
                                     "internal/fixture/x.go": debt}, "debt with lines")
        self.commit(reown, {"internal/fixture/x.go": debt,
                            "tools/boundary/ledger.tsv": ledger_text([(r[0], r[1], r[2], "GG09") for r in rows])},
                    "owner reassigned")
        reowned = self.cli(reown, self.event({"before": before}))
        self.assertEqual(1, reowned.returncode)
        self.assertIn("exceeds the base revision", reowned.stderr)

        seeding = self.repo()
        seed_base = self.commit(seeding, {**self.boundary_files(), "internal/fixture/x.go": "package fixture\n"},
                                "no ledger at base")
        self.commit(seeding, {"internal/fixture/x.go": debt, "tools/boundary/ledger.tsv": ledger_text(rows)},
                    "seed ledger")
        seeded = self.cli(seeding, self.event({"before": seed_base}))
        self.assertEqual(0, seeded.returncode)
        self.assertIn("initial seeding", seeded.stdout)

    def test_matter_and_sensitive_literals_are_findings_outside_admitted_file(self):
        text = 'package main\n\nvar flag = "-matter-endpoint-store"\n'
        self.assertTrue(self.at("cmd/gateway/product_flags.go", text))
        self.assertEqual([], self.at("cmd/gateway/profile_managed_driver_test.go", "package main\n"))


if __name__ == "__main__":
    unittest.main()
