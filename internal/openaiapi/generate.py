# /// script
# requires-python = ">=3.12"
# dependencies = ["PyYAML==6.0.3"]
# ///

import json
import os
import subprocess
from pathlib import Path
from tempfile import NamedTemporaryFile

import yaml


class SpecLoader(yaml.SafeLoader):
    yaml_constructors = yaml.SafeLoader.yaml_constructors | {
        # keep date/datetime-like as strings
        "tag:yaml.org,2002:timestamp": yaml.SafeLoader.construct_scalar,
    }


def discriminator_values(
    schema: dict, field: str, references: dict, seen: frozenset[str] = frozenset()
) -> set[str]:
    if "$ref" in schema:
        reference = schema["$ref"]
        if reference in seen:
            return set()
        return discriminator_values(
            references[reference], field, references, seen | {reference}
        )
    prop = schema.get("properties", {}).get(field, {})
    if "$ref" in prop:
        prop = references[prop["$ref"]]
    if isinstance(prop.get("const"), str):
        return {prop["const"]}
    if "enum" in prop:
        return {value for value in prop["enum"] if isinstance(value, str)}
    return {
        value
        for keyword in ("allOf", "anyOf", "oneOf")
        for child in schema.get(keyword, [])
        for value in discriminator_values(child, field, references, seen)
    }


def prepare_discriminators(schema: dict, references: dict) -> None:
    discriminator = schema.get("discriminator")
    members = schema.get("oneOf", schema.get("anyOf"))
    if (
        discriminator is not None
        and "mapping" not in discriminator
        and members is not None
    ):
        mapping = {}
        for member in members:
            reference = member["$ref"]
            values = discriminator_values(
                member, discriminator["propertyName"], references
            )
            # Missing values leave branches unmapped; overlaps overwrite mappings.
            # Codegen can still succeed by falling back to component names, causing
            # generated From... helpers to write incorrect discriminator values.
            if not values:
                raise ValueError(f"no discriminator values for {reference}")
            if overlap := mapping.keys() & values:
                raise ValueError(f"overlapping discriminator values: {sorted(overlap)}")
            mapping.update({value: reference for value in sorted(values)})
        discriminator["mapping"] = mapping

    for child in schema.get("properties", {}).values():
        prepare_discriminators(child, references)
    for keyword in ("items", "additionalProperties"):
        child = schema.get(keyword)
        if isinstance(child, dict):
            prepare_discriminators(child, references)
    for keyword in ("allOf", "anyOf", "oneOf"):
        for child in schema.get(keyword, []):
            prepare_discriminators(child, references)


def prepare_spec(spec: dict) -> dict:
    spec.pop("webhooks", None)
    schemas = spec["components"]["schemas"]

    # These unions contain multiple message variants with the same type value.
    for name in (
        "Item",
        "InputItem",
        "ItemResource",
        "BetaItem",
        "BetaInputItem",
        "BetaItemResource",
        "RealtimeConversationItem",
    ):
        del schemas[name]["discriminator"]

    # Discriminator preparation traverses schemas before codegen filters operations:
    # Realtime has inline variants; the beta filter uses a legacy $recursiveRef.
    del schemas["RealtimeTurnDetection"]["anyOf"][0]["discriminator"]
    del schemas["BetaCompoundFilter"]["properties"]["filters"]["items"]["discriminator"]

    # Resolve the snapshot's legacy recursive-reference spelling explicitly.
    schemas["CompoundFilter"]["properties"]["filters"]["items"]["oneOf"][1] = {
        "$ref": "#/components/schemas/CompoundFilter",
    }
    references = {
        f"#/components/schemas/{name}": schema for name, schema in schemas.items()
    }
    for schema in schemas.values():
        prepare_discriminators(schema, references)
    return spec


def main() -> None:
    if yaml.__version__ != "6.0.3":
        raise SystemExit(
            "OpenAPI generation requires PyYAML==6.0.3; "
            "see third_party/openai-openapi/README.md for the isolated environment."
        )
    directory = Path(__file__).resolve().parent
    source = directory / "../../third_party/openai-openapi/openapi.yaml"
    with source.open() as original:
        spec = prepare_spec(yaml.load(original, Loader=SpecLoader))
    with NamedTemporaryFile(mode="w+", suffix=".json") as prepared:
        json.dump(spec, prepared)
        prepared.flush()
        subprocess.run(
            [
                "go",
                "tool",
                "-modfile",
                str(directory.parent / "apigen/go.mod"),
                "oapi-codegen",
                "-config",
                "codegen.yaml",
                prepared.name,
            ],
            check=True,
            cwd=directory,
            env=os.environ | {"GOWORK": "off"},
        )


if __name__ == "__main__":
    main()
