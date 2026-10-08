import unittest
import shutil
import subprocess
import sys
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest.mock import patch

import yaml
from generate import SpecLoader, discriminator_values, main, prepare_discriminators


class GenerateTest(unittest.TestCase):
    def test_discriminator_mapping(self) -> None:
        references = {
            "#/components/schemas/Comparison": {
                "properties": {"type": {"enum": ["eq", "ne"]}}
            },
            "#/components/schemas/Compound": {
                "properties": {"type": {"enum": ["and", "or"]}}
            },
        }
        schema = {
            "oneOf": [{"$ref": ref} for ref in references],
            "discriminator": {"propertyName": "type"},
        }
        prepare_discriminators(schema, references)
        self.assertEqual(
            schema["discriminator"]["mapping"],
            {
                "eq": "#/components/schemas/Comparison",
                "ne": "#/components/schemas/Comparison",
                "and": "#/components/schemas/Compound",
                "or": "#/components/schemas/Compound",
            },
        )

    def test_ambiguous_discriminator(self) -> None:
        references = {
            f"#/components/schemas/{name}": {
                "properties": {"type": {"const": "message"}}
            }
            for name in ("Input", "Output")
        }
        schema = {
            "oneOf": [{"$ref": ref} for ref in references],
            "discriminator": {"propertyName": "type"},
        }
        with self.assertRaises(ValueError):
            prepare_discriminators(schema, references)

    def test_missing_discriminator_values(self) -> None:
        schema = {
            "anyOf": [{"$ref": "#/components/schemas/Untagged"}],
            "discriminator": {"propertyName": "type"},
        }
        with self.assertRaises(ValueError):
            prepare_discriminators(
                schema, {"#/components/schemas/Untagged": {"type": "object"}}
            )

    def test_yaml_scalars(self) -> None:
        values = yaml.load(
            '[true, 42, 1.5, null, 2024-10-01, 2024-10-01T12:30:00Z, "001"]',
            Loader=SpecLoader,
        )
        self.assertEqual(
            values,
            [True, 42, 1.5, None, "2024-10-01", "2024-10-01T12:30:00Z", "001"],
        )

    def test_unpinned_yaml_rejected_before_generation(self) -> None:
        with patch("generate.yaml.__version__", "0.0.0"), patch(
            "generate.subprocess.run"
        ) as run:
            with self.assertRaisesRegex(SystemExit, "PyYAML==6.0.3"):
                main()
            run.assert_not_called()

    def test_unknown_reference_fails_closed(self) -> None:
        with self.assertRaises(KeyError):
            discriminator_values({"$ref": "#/missing"}, "type", {})

    def test_recursive_reference_does_not_loop(self) -> None:
        reference = "#/recursive"
        self.assertEqual(
            discriminator_values({"$ref": reference}, "type", {reference: {"$ref": reference}}),
            set(),
        )

    def test_explicit_mapping_is_preserved(self) -> None:
        schema = {
            "oneOf": [{"$ref": "#/one"}],
            "discriminator": {"propertyName": "type", "mapping": {"wire": "#/one"}},
        }
        prepare_discriminators(schema, {})
        self.assertEqual(schema["discriminator"]["mapping"], {"wire": "#/one"})

    def test_fresh_generation_matches_checked_in_types(self) -> None:
        root = Path(__file__).resolve().parents[2]
        files = (
            "go.mod", "go.sum", "internal/apigen/go.mod", "internal/apigen/go.sum",
            "internal/apijson/imports.tmpl", "internal/openaiapi/codegen.yaml",
            "internal/openaiapi/generate.py", "third_party/openai-openapi/openapi.yaml",
        )
        with TemporaryDirectory(prefix="unreal-openai-generator-test-") as temporary:
            checkout = Path(temporary)
            for name in files:
                destination = checkout / name
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(root / name, destination)
            subprocess.run(
                [sys.executable, "-B", str(checkout / "internal/openaiapi/generate.py")],
                cwd=checkout, check=True,
            )
            self.assertEqual(
                (checkout / "internal/openaiapi/types.gen.go").read_bytes(),
                (root / "internal/openaiapi/types.gen.go").read_bytes(),
                "generated wire types must match a fresh isolated generation",
            )


if __name__ == "__main__":
    unittest.main()
