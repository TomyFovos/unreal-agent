import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).parent))
import pilot_kit
import snapshot


TEMPLATES = Path(__file__).with_name("pilot")


def source_archive(root, revision=pilot_kit.BASELINE, extra=None):
    artifacts = root / "source"
    artifacts.mkdir()
    archive = artifacts / "snapshot.tar.gz"
    metadata = {"revision": revision, "kind": "local-snapshot", "formal_release": False,
                "worktree_dirty": True, "target": "linux/amd64", "cgo_enabled": True}
    with tarfile.open(archive, "w:gz") as bundle:
        for name in sorted(snapshot.PACKAGE_FILES | ({extra} if extra else set())):
            data = json.dumps(metadata).encode() if name == "SNAPSHOT.json" else b"test fixture"
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.mode = 0o700 if name in snapshot.BINARIES else 0o600
            bundle.addfile(info, io.BytesIO(data))
    (artifacts / "SHA256SUMS").write_text(f"{pilot_kit.digest(archive)}  {archive.name}\n")
    return archive


class KitBuilderTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="unreal-pilot-unit-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def assemble(self, source):
        output = Path(tempfile.mkdtemp(prefix="output-", dir=self.root))
        with patch.object(pilot_kit, "binary_requirements", return_value="2.34"):
            return pilot_kit.assemble(source, output, TEMPLATES)

    def test_exact_inventory_provenance_no_authentication(self):
        source = source_archive(self.root)
        archive = self.assemble(source)
        with tarfile.open(archive) as bundle:
            self.assertEqual({item.name for item in bundle}, {pilot_kit.KIT_ROOT + "/" + name for name in (*pilot_kit.FILES, "SHA256SUMS")})
            self.assertTrue(all(item.isfile() for item in bundle))
            snapshot_raw = bundle.extractfile(pilot_kit.KIT_ROOT + "/SNAPSHOT.json").read()
            self.assertTrue(json.loads(snapshot_raw)["worktree_dirty"])
            with tarfile.open(source) as original:
                self.assertEqual(snapshot_raw, original.extractfile("SNAPSHOT.json").read())
            manifest = json.load(bundle.extractfile(pilot_kit.KIT_ROOT + "/KIT.json"))
            self.assertEqual(manifest["minimum_glibc"], "2.34")
            self.assertEqual(manifest["source_archive_sha256"], pilot_kit.digest(source))
        sums = archive.with_name(archive.name + ".sha256").read_text()
        self.assertEqual(sums, f"{pilot_kit.digest(archive)}  {archive.name}\n")

    def test_reject_unrelated_file_or_path_traversal(self):
        for extra in ("auth.json", "../outside", "data/session.json"):
            with self.subTest(extra=extra), tempfile.TemporaryDirectory() as temporary:
                source = source_archive(Path(temporary), extra=extra)
                with self.assertRaises(ValueError):
                    self.assemble(source)

    def test_baseline_mismatch_rejected(self):
        source = source_archive(self.root, revision="a" * 40)
        with self.assertRaisesRegex(ValueError, "baseline"):
            self.assemble(source)

    def test_source_corruption_rejected(self):
        source = source_archive(self.root)
        with source.open("ab") as stream:
            stream.write(b"modified")
        with self.assertRaisesRegex(ValueError, "checksum"):
            self.assemble(source)

    def test_output_and_delivery_never_overwrite_previous_files(self):
        output1 = pilot_kit.private_output(self.root)
        output2 = pilot_kit.private_output(self.root)
        self.assertNotEqual(output1, output2)
        self.assertEqual(output1.stat().st_mode & 0o777, 0o700)
        archive = self.root / "archive.tar.gz"
        archive.write_bytes(b"delivery fixture")
        archive.with_name(archive.name + ".sha256").write_bytes(b"checksum fixture")
        destination = self.root / "windows"
        destination.mkdir()
        sentinel = destination / "keep"
        sentinel.write_bytes(b"unchanged")
        first = pilot_kit.copy_delivery(archive, destination)
        second = pilot_kit.copy_delivery(archive, destination)
        self.assertNotEqual(first, second)
        self.assertEqual(sentinel.read_bytes(), b"unchanged")
        self.assertEqual((first / archive.name).read_bytes(), archive.read_bytes())
        self.assertEqual((second / archive.name).read_bytes(), archive.read_bytes())

    def test_symlink_output_or_delivery_parent_rejected(self):
        protected = self.root / "protected"
        protected.mkdir()
        (self.root / "dist").symlink_to(protected, target_is_directory=True)
        with self.assertRaises(ValueError):
            pilot_kit.private_output(self.root)
        destination = self.root / "delivery"
        destination.symlink_to(protected, target_is_directory=True)
        with self.assertRaises(ValueError):
            pilot_kit.copy_delivery(self.root / "unused", destination / "child")
        self.assertEqual(list(protected.iterdir()), [])

    def test_four_binaries_need_cgo_v1_baseline_and_ast(self):
        directory = self.root / "bins"
        directory.mkdir()
        for name in snapshot.BINARIES:
            (directory / name).write_bytes(b"\x7fELF\x02\x01\x01" + b"\0" * 11 + b"\x3e\0")
        info = f"CGO_ENABLED=1 GOAMD64=v1 GOOS=linux GOARCH=amd64 vcs.revision={pilot_kit.BASELINE} github.com/tree-sitter/go-tree-sitter"

        def response(args, **kwargs):
            if args[:2] == ["go", "version"]:
                return info
            if args[1] == "--version-info":
                return "GLIBC_2.2.5 GLIBC_2.34 GLIBC_2.3"
            return "(NEEDED) Shared library: [libc.so.6]"

        with patch.object(pilot_kit.subprocess, "check_output", side_effect=response):
            self.assertEqual(pilot_kit.binary_requirements(directory, pilot_kit.BASELINE), "2.34")
        info = info.replace("CGO_ENABLED=1", "CGO_ENABLED=0")
        with patch.object(pilot_kit.subprocess, "check_output", side_effect=response), self.assertRaises(ValueError):
            pilot_kit.binary_requirements(directory, pilot_kit.BASELINE)

    def test_readme_has_only_two_user_operations(self):
        archive = self.assemble(source_archive(self.root))
        with tarfile.open(archive) as bundle:
            text = bundle.extractfile(pilot_kit.KIT_ROOT + "/README.md").read().decode()
        self.assertEqual([line[:2] for line in text.splitlines() if line[:2] in ("1.", "2.", "3.")], ["1.", "2."])
        self.assertIn("sbx cp", text)
        self.assertNotIn("@", text)


@unittest.skipUnless(os.environ.get("UNREAL_PILOT_SNAPSHOT"), "set UNREAL_PILOT_SNAPSHOT to run actual snapshot installer/doctor tests")
class KitSnapshotTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory(prefix="unreal-pilot-real-binaries-")
        cls.root = Path(cls.temporary.name)
        cls.archive = pilot_kit.assemble(Path(os.environ["UNREAL_PILOT_SNAPSHOT"]).resolve(), cls.root, TEMPLATES)
        cls.package = cls.root / pilot_kit.KIT_ROOT

    @classmethod
    def tearDownClass(cls):
        cls.temporary.cleanup()

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="unreal-pilot-install-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.home = self.root / "home with spaces"
        self.home.mkdir()
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.home), "TMPDIR": str(self.root), "LC_ALL": "C"}

    def run_script(self, name, *args, package=None):
        return subprocess.run(["/bin/bash", str((package or self.package) / name), *args], env=self.env,
                              capture_output=True, text=True, timeout=120)

    def require_success(self, result):
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def shim(self, command, body):
        directory = self.root / "shims"
        directory.mkdir(exist_ok=True)
        path = directory / command
        path.write_text("#!/bin/sh\n" + body + "\n")
        path.chmod(0o700)
        self.env["PATH"] = str(directory) + ":" + os.environ["PATH"]

    def test_extract_checksums_and_four_binary_doctor(self):
        extracted = self.root / "unpacked"
        extracted.mkdir()
        with tarfile.open(self.archive) as bundle:
            bundle.extractall(extracted, filter="data")
        package = extracted / pilot_kit.KIT_ROOT
        result = subprocess.run(["sha256sum", "--check", "SHA256SUMS"], cwd=package, capture_output=True, text=True)
        self.require_success(result)
        result = self.run_script("doctor.sh", package=package)
        self.require_success(result)
        for name in snapshot.BINARIES:
            self.assertIn(name + ": non-inference CLI", result.stdout)
        self.assertIn("network/API connectivity NOT probed", result.stdout)

    def test_install_rerun_permissions_and_existing_codex_unchanged(self):
        existing = {".codex/auth.json": b"private fixture, never read", ".codex/config.toml": b"keep config",
                    ".bashrc": b"keep PATH", ".config/unreal-agent/runtime.json": b"keep runtime",
                    ".local/state/unreal-agent/session.json": b"keep canonical history", "bin/codex": b"keep CLI"}
        facts = {}
        for name, data in existing.items():
            path = self.home / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
            facts[name] = (path.stat().st_mtime_ns, path.stat().st_mode, data)
        self.env.update(OPENAI_API_KEY="unused fixture", OPENAI_CODEX_ACCESS_TOKEN="unused fixture")
        first = self.run_script("install.sh")
        self.require_success(first)
        second = self.run_script("install.sh")
        self.require_success(second)
        self.assertIn("identical installation reused", second.stdout)
        installed = Path(next(line.removeprefix("INSTALLED: ") for line in first.stdout.splitlines() if line.startswith("INSTALLED: ")))
        installed_second = next(line.removeprefix("INSTALLED: ") for line in second.stdout.splitlines() if line.startswith("INSTALLED: "))
        self.assertEqual(str(installed), installed_second)
        self.assertEqual(installed.stat().st_mode & 0o777, 0o700)
        for name in snapshot.BINARIES:
            self.assertEqual((installed / name).stat().st_mode & 0o777, 0o700)
        self.assertEqual((installed / "SNAPSHOT.json").stat().st_mode & 0o777, 0o600)
        self.assertFalse((installed.parent / ".install.lock").exists())
        self.assertEqual(sorted(path.name for path in installed.iterdir()), sorted((*pilot_kit.FILES, "SHA256SUMS")))
        for name, expected in facts.items():
            path = self.home / name
            self.assertEqual((path.stat().st_mtime_ns, path.stat().st_mode, path.read_bytes()), expected)
        self.assertNotIn("private fixture", first.stdout + first.stderr + second.stdout + second.stderr)
        self.assertNotIn("unused fixture", first.stdout + first.stderr)

    def test_invalid_os_arch_and_old_libc_refused_before_install(self):
        cases = [("uname", "printf 'Darwin\\n'", "Linux required"),
                 ("uname", "if [ \"$1\" = -s ]; then echo Linux; else echo aarch64; fi", "amd64"),
                 ("getconf", "echo 'glibc 2.33'", "glibc 2.34"),
                 ("getconf", "exit 1", "glibc required")]
        for command, body, marker in cases:
            with self.subTest(marker=marker):
                self.shim(command, body)
                result = self.run_script("install.sh")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(marker, result.stderr)
                self.assertFalse((self.home / ".local").exists())
                (self.root / "shims" / command).unlink()

    def test_missing_library_refused_before_install(self):
        self.shim("ldd", "echo 'libc.so.6 => not found'; exit 1")
        result = self.run_script("install.sh")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("library/loader check failed", result.stderr)
        self.assertFalse((self.home / ".local").exists())

    def test_missing_dependency_refused_before_install(self):
        self.shim("getconf", "echo 'glibc 2.34'")
        # A deliberately minimal PATH makes the preflight stop without changes.
        shims = self.root / "shims"
        for tool in ("bash", "dirname", "stat", "sha256sum", "grep", "uname"):
            (shims / tool).symlink_to(shutil.which(tool))
        self.env["PATH"] = str(shims)
        result = self.run_script("install.sh")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("required command unavailable", result.stderr)
        self.assertFalse((self.home / ".local").exists())

    def test_symlink_parent_refused_without_touching_target(self):
        protected = self.root / "protected"
        protected.mkdir()
        (self.home / ".local").symlink_to(protected, target_is_directory=True)
        result = self.run_script("install.sh")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("symlink", result.stderr)
        self.assertEqual(list(protected.iterdir()), [])

    def test_changed_or_symlinked_existing_install_never_overwritten(self):
        result = self.run_script("install.sh")
        self.require_success(result)
        installed = Path(next(line.removeprefix("INSTALLED: ") for line in result.stdout.splitlines() if line.startswith("INSTALLED: ")))
        path = installed / "README.md"
        path.write_bytes(b"keep user's modified installation")
        result = self.run_script("install.sh")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(path.read_bytes(), b"keep user's modified installation")
        path.unlink()
        protected = self.root / "private"
        protected.write_bytes(b"never read this")
        path.symlink_to(protected)
        result = self.run_script("install.sh")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("redirected member", result.stderr)
        self.assertEqual(protected.read_bytes(), b"never read this")

    def test_corrupt_source_and_doctor_refused_before_install(self):
        clone = self.root / "clone"
        shutil.copytree(self.package, clone, copy_function=os.link)
        path = clone / "README.md"
        path.unlink()
        path.write_bytes(b"bad payload")
        result = self.run_script("install.sh", package=clone)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum mismatch", result.stderr)
        self.assertFalse((self.home / ".local").exists())
        path = clone / "doctor.sh"
        path.unlink()
        path.write_text("#!/bin/bash\ntouch \"$HOME/forbidden\"\n")
        result = self.run_script("install.sh", package=clone)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("doctor checksum mismatch", result.stderr)
        self.assertFalse((self.home / "forbidden").exists())


if __name__ == "__main__":
    unittest.main()
