"""Read token prices from LiteLLM's packaged JSON without importing its client."""

import json
from functools import lru_cache
from importlib.metadata import distribution
from pathlib import Path


def price_path():
    return Path(
        distribution("litellm").locate_file(
            "litellm/model_prices_and_context_window_backup.json"
        )
    )


@lru_cache
def load_prices(path=None):
    return json.loads((Path(path) if path else price_path()).read_text())


def estimate(provider, model, usage, *, prices=None):
    if any(
        f in usage.get("Unknown", [])
        for f in ("input", "cached_input", "cache_write_input", "output")
    ):
        return None
    prices = load_prices() if prices is None else prices
    prefix = "anthropic" if provider in {"claude-api", "claude-code"} else provider
    entry = next((prices[m] for m in (f"{prefix}/{model}", model) if m in prices), None)
    if (
        entry is None
        or not {"input_cost_per_token", "output_cost_per_token"} <= entry.keys()
    ):
        return None
    cached, written = (
        usage.get("CachedInputTokens", 0),
        usage.get("CacheWriteInputTokens", 0),
    )
    uncached = usage.get("InputTokens", 0) - cached - written
    input_price, output_price = (
        entry["input_cost_per_token"],
        entry["output_cost_per_token"],
    )
    return (
        uncached * input_price
        + cached * entry.get("cache_read_input_token_cost", input_price)
        + written * entry.get("cache_creation_input_token_cost", input_price)
        + usage.get("OutputTokens", 0) * output_price
    )
