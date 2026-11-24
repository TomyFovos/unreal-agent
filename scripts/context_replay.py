#!/usr/bin/env python3
"""Replay input-token cost and context rebuilds from read-only Session copies."""

import argparse
from collections import Counter, defaultdict
import csv
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from itertools import product
import json
import math
from pathlib import Path
from statistics import median
import sys

from context_replay_records import ROUTE_PROVIDERS, read_traces, timestamp


# Claude's 1h lifetime was recorded by Claude Code. Codex's 1h and DeepSeek's
# 6h are effective lifetime assumptions. A4 also tests 5m and 1h hard expiry.
# Weights normalize to ordinary input. Codex uses the experiment's 0.10 input
# ratio and logged subscription writes. DeepSeek uses V4.1 Flash, 0.006/0.30.
# https://platform.claude.com/docs/en/build-with-claude/prompt-caching
# https://developers.openai.com/api/docs/guides/prompt-caching
# https://api-docs.deepseek.com/quick_start/pricing/
@dataclass(frozen=True)
class Cache:
    ttl: int
    read_weight: float
    write_weight: float
    unit: int
    minimum: int
    explicit_write: bool = False


CACHES = {
    "claude": Cache(3600, 0.10, 1.25, 1, 512, True),
    "claude-api": Cache(3600, 0.10, 1.25, 1, 512, True),
    "codex": Cache(3600, 0.10, 1.0, 128, 1024),
    "deepseek": Cache(21600, 0.02, 1.0, 128, 128),
    # Preserve token comparisons for other OpenCode Go models without
    # assigning DeepSeek's model-specific read discount.
    "opencode-go": Cache(3600, 1.0, 1.0, 128, 128),
}
W_GRID = (100_000, 150_000, 200_000, 250_000, 300_000, 400_000)
RATIO_GRID = (0.3, 0.5, 0.7)
L2_GRID = (2500, 5000, 10_000)
NON_SIZE_CAUSES = {"header_change", "selection_change", "historical_context", "provider_overflow"}


@dataclass(frozen=True)
class Settings:
    w: int = 200_000
    ratio: float = 0.5
    l2: int = 5000
    l1: int = 0  # zero leaves the rebuild target in charge of the tail bound
    trigger: str = "growth"


CONTEXT_DEFAULTS = {
    "WindowThresholdTokens": Settings.w,
    "PostRebuildRatio": Settings.ratio,
    "L2TargetTokens": Settings.l2,
    "RecentEventCount": 0,
    "ToolOutputExcerptChars": 0,
    "CacheRewarmMinutes": 0,
    "FallbackWindow": 272_000,
    "InputBudget": 16 << 20,
    "ResponseReserve": 4096,
    "ProtocolReserve": 1024,
}
DEFAULT_ON_ZERO = {"L2TargetTokens", "FallbackWindow", "InputBudget", "ResponseReserve", "ProtocolReserve"}
COMMON_CONTEXT_KEYS = {
    "Routes", "Version", "Mode", "WindowStrategy", "ClaudeCompactThresholdTokens",
    "RecentReserve", "RetrievalReserve", "RetrievalLimit", "CheckpointThreshold",
    "LargeToolTokens", "ToolResultKeepTokens", "ToolResultMinPruneTokens",
}


def rebuild_target(w, ratio, budget):
    return max(1, int(min(w, budget) * ratio))


def resolve_context_policy(fields, location, common=None):
    if not isinstance(fields, dict):
        raise ValueError(f"{location} must be an object")
    allowed = CONTEXT_DEFAULTS.keys() | COMMON_CONTEXT_KEYS if common is None else CONTEXT_DEFAULTS.keys()
    if extra := fields.keys() - allowed:
        raise ValueError(f"{location} has unknown fields: {', '.join(sorted(extra))}")
    policy = dict(CONTEXT_DEFAULTS if common is None else common)
    for key in CONTEXT_DEFAULTS:
        value = fields.get(key)
        if value is None:
            continue
        if key == "PostRebuildRatio":
            if type(value) not in (int, float) or not 0 < value < 1:
                raise ValueError(f"{location}.{key} must be between 0 and 1 (exclusive)")
        elif type(value) is not int or not 0 <= value < 2**63:
            raise ValueError(f"{location}.{key} must be a nonnegative integer")
        if common is not None or key not in DEFAULT_ON_ZERO or value != 0:
            policy[key] = value
    w = policy["WindowThresholdTokens"]
    if w <= 0:
        raise ValueError(f"{location}.WindowThresholdTokens must be positive")
    if policy["L2TargetTokens"] > rebuild_target(w, policy["PostRebuildRatio"], w):
        raise ValueError(f"{location}.L2TargetTokens exceeds the rebuild target")
    if policy["InputBudget"] <= 0 or policy["FallbackWindow"] <= policy["ResponseReserve"] + policy["ProtocolReserve"]:
        raise ValueError(f"{location} has an invalid input budget or window reserves")
    return policy


def read_context_policies(path):
    document = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(document, dict):
        raise ValueError("runtime configuration must be an object")
    fields = document.get("Context")
    if fields is None:
        fields = {}
    common = resolve_context_policy(fields, "Context")
    routes = fields.get("Routes")
    if routes is None:
        routes = {}
    if not isinstance(routes, dict):
        raise ValueError("Context.Routes must be an object")
    return common, {provider: resolve_context_policy({} if overrides is None else overrides,
                                                      f"Context.Routes[{provider}]", common)
                    for provider, overrides in routes.items()}


@dataclass
class Profile:
    knots: list
    visible_share: float
    header: int
    l2_demand: list
    still_cached: float
    suffix: int
    head_reads: dict
    uncached: int
    usual_step: int

    def fresh_input(self, estimate):
        """Interpolate held-out-independent fits of measured vs. estimated fresh inputs."""
        if not self.knots:
            return estimate
        if len(self.knots) == 1:
            x, y = self.knots[0]
            return max(1, estimate + y - x)
        for i in range(1, len(self.knots)):
            x0, y0 = self.knots[i - 1]
            x1, y1 = self.knots[i]
            if estimate <= x1 or i == len(self.knots) - 1:
                slope = max(0.1, (y1 - y0) / (x1 - x0))
                return max(1, round(y0 + (estimate - x0) * slope))
        raise AssertionError("missing interpolation segment")

    def desired_l2(self, peers):
        if not self.l2_demand:
            return 0
        nearest = sorted(self.l2_demand, key=lambda item: abs(math.log1p(item[0]) - math.log1p(peers)))
        return round(median(tokens for _, tokens in nearest[:7]))


def same_epoch(previous, current):
    return (previous is not None and previous.model == current.model and previous.effort == current.effort
            and previous.route == current.route and previous.window is current.window
            and previous.header_key == current.header_key)


def split_routes(sessions):
    traces = defaultdict(list)
    for rows in sessions.values():
        trace = []
        for request in rows:
            if trace and trace[-1].route != request.route:
                traces[trace[0].route].append(trace)
                trace = []
            trace.append(request)
        if trace:
            traces[trace[0].route].append(trace)
    return traces


def learnt_profiles(traces, cutoff, cache):
    fresh = defaultdict(list)
    headers, l2, regular = [], [], []
    growth, delta, uncached, cache_pairs = [], [], [], []
    head_reads = defaultdict(list)
    calibration_count = 0
    for rows in traces:
        previous = estimated = None
        for request in rows:
            if request.at >= cutoff:
                break
            calibration_count += 1
            model = request.model
            if not same_epoch(previous, request):
                estimated = None
                if request.estimate:
                    fresh[model].append((request.estimate, request.input))
                if request.read is not None:
                    head_reads[model, bool(request.window)].append(request.read)
                    head_reads["", bool(request.window)].append(request.read)
                if request.window:
                    w = request.window
                    if w.header:
                        headers.append(w.header)
                    if w.l2:
                        l2.append((request.peers, w.l2))
            else:
                step = request.input - previous.input
                if step > 0:
                    delta.append(step)
                if request.estimate and estimated:
                    change = request.input - estimated.input
                    if change > 100:
                        growth.append((request.estimate - estimated.estimate) / change)
                if request.read is not None and 0 <= request.at - previous.at <= 300:
                    limit = min(previous.input, request.input)
                    cache_pairs.append((limit, request.read))
                    if request.read > limit / 2:
                        regular.append(limit - request.read)
            if request.estimate:
                estimated = request
            if request.read is not None and request.write is not None:
                uncached.append(max(0, request.input - request.read - request.write))
            previous = request

    def fit(samples):
        bins = defaultdict(list)
        for x, y in samples:
            edge = next((v for v in (10_000, 40_000, 70_000, 150_000) if x < v), math.inf)
            bins[edge].append((x, y))
        return [(median(x for x, _ in items), median(y for _, y in items))
                for _, items in sorted(bins.items())]

    # The best-effort hit fraction is learned only from short, same-window gaps.
    suffix = max(0, round(median(regular) - (cache.unit - 1) / 2)) if regular else 0
    eligible = [(prefix(limit, suffix, cache), read) for limit, read in cache_pairs]
    available = sum(cap for cap, _ in eligible)
    still_cached = sum(min(cap, read) for cap, read in eligible) / available if available else 1
    overall = [point for group in fresh.values() for point in group]
    common = dict(
        visible_share=max(0, min(2, median(growth))) if growth else 1,
        header=round(median(headers)) if headers else 0,
        l2_demand=l2,
        still_cached=still_cached,
        suffix=suffix,
        head_reads={key: median(items) for key, items in head_reads.items()},
        uncached=round(median(uncached)) if uncached else 0,
        usual_step=round(median(delta)) if delta else 0,
    )
    profiles = {model: Profile(fit(points), **common) for model, points in fresh.items() if len(points) >= 8}
    profiles[None] = Profile(fit(overall), **common)
    return profiles, calibration_count


def prefix(length, suffix, cache):
    cap = max(0, int(length - suffix)) // cache.unit * cache.unit
    return cap if cap >= cache.minimum else 0


@dataclass
class Step:
    request: object
    public: int
    actual: int
    observed_estimate: int
    budget: int
    rebuild_record: bool
    switched: bool
    head_gap: float = math.inf
    l2_demand: int = 0


def prepare(traces, profiles, policy=None):
    prepared = []
    for rows in traces:
        steps = []
        previous = None
        estimate = 0
        observed_budget = 994_880 if rows[0].route == "deepseek" else 266_880
        model_window = 0
        for request in rows:
            profile = profiles.get(request.model, profiles[None])
            w = request.window
            boundary = w is not None and (previous is None or w is not previous.window)
            switched = (previous is not None and bool(previous.header_key) and bool(request.header_key)
                        and previous.header_key != request.header_key)
            observed_budget = request.budget or observed_budget
            budget = observed_budget
            if policy is not None:
                if previous is None or previous.model != request.model:
                    model_window = 0
                if request.admission.get("WindowSource") == "conservative-fallback":
                    model_window = policy["FallbackWindow"]
                else:
                    model_window = request.admission.get("WindowTokens") or model_window
                if model_window:
                    budget = model_window - policy["ResponseReserve"] - policy["ProtocolReserve"]
                budget = min(budget, policy["InputBudget"])
                if budget <= 0:
                    raise ValueError(f"candidate leaves no input budget for {request.route}/{request.model}")
            if not previous:
                estimate = request.estimate or (w.after if w else request.input)
                public, actual = estimate, request.input
            elif boundary:
                # The new window hid the original next prompt. Responses recorded
                # its estimated addition to the previous model-reported prefix.
                # Claude's hook recorded the previous input, before the tool batch.
                if w.measurement == "measured-prefix+estimated-delta":
                    public = max(0, w.before - previous.input)
                elif w.measurement == "estimated" and w.before >= estimate:
                    public = w.before - estimate
                else:
                    public = round(profile.usual_step * profile.visible_share)
                actual = max(0, round(public / (profile.visible_share or 1)))
                estimate = request.estimate or w.after
            else:
                actual = request.input - previous.input
                # Request estimates are workload observations within the original
                # window. Use provider deltas for Claude's unlogged hook steps.
                public = request.estimate - estimate if request.estimate else round(actual * profile.visible_share)
                estimate = max(1, estimate + public)
            steps.append(Step(request, public, actual, estimate, budget, boundary, switched,
                              l2_demand=profile.desired_l2(request.peers)))
            previous = request
        prepared.append(steps)

    latest = {}
    for step in sorted((s for steps in prepared for s in steps), key=lambda s: s.request.at):
        request = step.request
        key = (request.model, request.effort, request.header_key)
        if request.header_key and key in latest:
            step.head_gap = request.at - latest[key]
        latest[key] = request.at
    return prepared


@dataclass
class Context:
    estimate: int = 0
    input: int = 0
    rebuilt_at_size: int = 0
    l2: int = 0
    human_tail: int = 0
    invocation_tail: int = 0
    cached_at: float = -math.inf


@dataclass(frozen=True)
class Tokens:
    input: float
    read: float
    write: float
    uncached: float

    def cost(self, cache):
        return self.read * cache.read_weight + self.write * cache.write_weight + self.uncached


def predict_cache(step, profile, cache, total, previous_total, gap, new_window, ttl=None):
    ttl = cache.ttl if ttl is None else ttl
    request = step.request
    cold = profile.head_reads.get((request.model, new_window), profile.head_reads.get(("", new_window), 0))
    # The system/tools cache can survive a rotation and be populated by another
    # compatible Session. Conversation reuse is scoped to this window.
    head = prefix(min(total, cold), 0, cache) if step.head_gap <= ttl else 0
    eligible = prefix(min(previous_total, total), profile.suffix, cache)
    read = max(head, eligible * profile.still_cached) if not new_window and gap <= ttl else head
    cached_through = max(read, total - profile.uncached) if cache.explicit_write and total >= cache.minimum else read
    return Tokens(total, read, cached_through - read, total - cached_through)


def observed_tokens(request):
    if request.read is None:
        return None
    write = request.write or 0
    if request.read < 0 or write < 0 or request.read + write > request.input:
        return None
    return Tokens(request.input, request.read, write, request.input - request.read - write)


def fitted_projection(settings, profile, step, preferred, minimum, human_changed):
    target = rebuild_target(settings.w, settings.ratio, step.budget)
    header = profile.header
    l2 = min(settings.l2, step.l2_demand, max(1, target - header))
    capacity = max(0, target - header - l2)
    if settings.l1:
        capacity = min(settings.l1, capacity)
    # The observed input size of a new human message is a floor for soft target
    # fitting. A last-unit cut keeps the current batch rather than arbitrary old tail.
    recent = max(0, step.public)
    mandatory = min(recent, capacity)
    if human_changed:
        mandatory = max(mandatory, step.request.new_human_tokens)
    if preferred <= capacity:
        tail = preferred
    elif minimum <= capacity:
        tail = minimum
    else:
        tail = mandatory
    tail = min(tail, max(0, step.budget - header - l2))
    return header + l2 + tail, l2, tail


@dataclass
class Result:
    calls: int = 0
    rebuilds: int = 0
    actual_rebuilds: int = 0
    exact_rebuilds: int = 0
    rebuild_tail: int = 0
    interval_calls: list = field(default_factory=list)
    interval_seconds: list = field(default_factory=list)
    distance_to_rebuild: list = field(default_factory=list)
    predicted: Counter = field(default_factory=Counter)
    actual: Counter = field(default_factory=Counter)
    errors: Counter = field(default_factory=Counter)
    actual_calls: int = 0

    def add(self, tokens, actual, cache):
        values = {"input": tokens.input, "read": tokens.read, "write": tokens.write,
                  "uncached": tokens.uncached, "cost": tokens.cost(cache)}
        self.calls += 1
        self.predicted.update(values)
        if actual is not None:
            actual_values = {"input": actual.input, "read": actual.read, "write": actual.write,
                             "uncached": actual.uncached, "cost": actual.cost(cache)}
            self.actual.update(actual_values)
            self.errors.update({key: abs(values[key] - act) for key, act in actual_values.items()})
            self.actual_calls += 1

    def summary(self):
        def rate(value, total):
            return round(value * 100 / total, 3) if total else None

        return {
            "requests": self.calls,
            "rebuilds": self.rebuilds,
            "observed_rebuilds": self.actual_rebuilds,
            "same_request_rebuilds": self.exact_rebuilds,
            "median_distance_to_rebuild_requests": median(self.distance_to_rebuild) if self.distance_to_rebuild else None,
            "observed_rebuilds_without_prediction_in_session": self.actual_rebuilds - len(self.distance_to_rebuild)
            if self.actual_calls else None,
            "requests_per_rebuild": round(self.calls / self.rebuilds, 1) if self.rebuilds else None,
            "median_interval_requests": median(self.interval_calls) if self.interval_calls else None,
            "median_interval_minutes": round(median(self.interval_seconds) / 60, 2) if self.interval_seconds else None,
            "average_new_l1": round(self.rebuild_tail / self.rebuilds) if self.rebuilds else None,
            "predicted": {k: round(v) for k, v in self.predicted.items()},
            "observed": dict(self.actual),
            "validation_n": self.actual_calls,
            "wape_percent": {k: rate(v, self.actual[k]) for k, v in self.errors.items()},
            "bias_percent": {k: rate(v - self.actual[k], self.actual[k]) for k, v in self.predicted.items()}
            if self.calls == self.actual_calls else None,
        }


def replay(traces, profiles, cache, settings, since=-math.inf, trace_writer=None, observed_windows=False, score=False):
    result = Result()
    for stream_id, steps in enumerate(traces, 1):
        state = Context()
        previous = last_rotation = None
        predicted_positions, observed_positions = [], []
        for position, step in enumerate(steps):
            request = step.request
            profile = profiles.get(request.model, profiles[None])
            if request.at < since:
                previous = step
                if request.window:
                    state.rebuilt_at_size = request.window.after
                    state.l2 = request.window.l2
                growth = max(0, step.public)
                state.human_tail = growth if position == 0 or request.human != steps[position - 1].request.human else state.human_tail + growth
                state.invocation_tail = 0 if position == 0 or request.invocation != steps[position - 1].request.invocation else state.invocation_tail + growth
                if step.rebuild_record:
                    state.human_tail, state.invocation_tail = request.window.tail, 0
                state.estimate, state.input, state.cached_at = step.observed_estimate, request.input, request.at
                continue

            w = request.window
            causes = set(w.causes) if step.rebuild_record else set()
            mid_turn = request.route in {"claude", "claude-api"} and previous is not None and (
                request.invocation == previous.request.invocation or "mid_turn" in causes)
            estimate = max(1, state.estimate + step.public)
            before = estimate if previous is None else state.input
            if previous and not mid_turn:
                before += step.public
            relative = state.rebuilt_at_size and settings.trigger == "growth"
            pressure = estimate - state.rebuilt_at_size if relative else before
            rotate = bool(pressure >= settings.w or before > step.budget or causes & NON_SIZE_CAUSES or step.switched)
            reset = rotate or previous is None or previous.request.model != request.model
            added = max(0, step.public)
            raw = max(0, estimate - profile.header - state.l2)
            human_changed = previous is None or request.human != previous.request.human
            preferred = min(raw, state.human_tail + added) if request.human and not human_changed else raw
            if request.human and human_changed:
                preferred = min(raw, added)
            process_tail = min(raw, state.invocation_tail + added) if mid_turn else added
            minimum = min(preferred, process_tail)

            if observed_windows:
                rotate = step.rebuild_record
                reset = not same_epoch(previous.request if previous else None, request)
                total, estimate = request.input, step.observed_estimate
            elif rotate:
                estimate, state.l2, tail = fitted_projection(
                    settings, profile, step, preferred, minimum, human_changed,
                )
                total = profile.fresh_input(estimate)
                result.rebuild_tail += tail
                state.rebuilt_at_size = estimate
                preferred, process_tail = min(preferred, tail), 0
            else:
                total = profile.fresh_input(estimate) if previous is None else max(1, state.input + step.actual)

            if rotate:
                result.rebuilds += 1
                predicted_positions.append(position)
                if last_rotation is not None:
                    prior_position, prior_time = last_rotation
                    result.interval_calls.append(position - prior_position)
                    result.interval_seconds.append((request.at - prior_time))
                last_rotation = position, request.at
            result.actual_rebuilds += step.rebuild_record
            result.exact_rebuilds += rotate and step.rebuild_record
            if step.rebuild_record:
                observed_positions.append(position)
            tokens = predict_cache(step, profile, cache, total, state.input, request.at - state.cached_at, reset)
            result.add(tokens, observed_tokens(request) if score else None, cache)
            if trace_writer:
                trace_writer.writerow([request.route, stream_id, request.seq, iso_time(request.at), rotate,
                                       round(total), round(tokens.read), round(tokens.write), round(tokens.uncached),
                                       round(tokens.cost(cache), 1), request.input, request.read,
                                       request.write, request.output])
            state.estimate, state.input, state.cached_at = estimate, total, request.at
            state.human_tail, state.invocation_tail = preferred, process_tail if mid_turn else 0
            previous = step
        if score and predicted_positions:
            result.distance_to_rebuild.extend(
                min(abs(i - j) for j in predicted_positions) for i in observed_positions
            )
    return result


def ttl_report(traces, profiles, cache, since):
    thresholds = (300, 3600)
    scenarios = {key: Counter() for key in thresholds}
    costs = Counter()
    for steps in traces:
        previous = None
        for step in steps:
            r = step.request
            p = previous.request if previous else None
            gap = r.at - p.at if p else math.inf
            reset = not same_epoch(p, r)
            profile = profiles.get(r.model, profiles[None])
            if r.at >= since:
                costs["requests"] += 1
                full = predict_cache(step, profile, cache, r.input, p.input if p else 0, gap, reset, math.inf)
                base = predict_cache(step, profile, cache, r.input, p.input if p else 0, gap, reset)
                costs["default"] += base.cost(cache)
                for ttl, stats in scenarios.items():
                    limited = predict_cache(step, profile, cache, r.input, p.input if p else 0, gap, reset, ttl)
                    price = limited.cost(cache)
                    if ttl == 3600 and cache.explicit_write:
                        price += (2 - cache.write_weight) * limited.write
                    costs[f"{ttl}s"] += price

                    # With a fixed schedule, a ping can fire even if the next
                    # request arrives just before expiry or starts a new window.
                    if ttl == 300 and p and gap > 0:
                        warmups = max(0, math.ceil(min(gap, 3600) / (ttl * 0.9)) - 1)
                        stats["hour_bounded_warmups"] += warmups
                        stats["hour_bounded_net"] -= warmups * prefix(
                            p.input, profile.suffix, cache,
                        ) * cache.read_weight
                    if reset or gap <= ttl or not p:
                        continue
                    stats["gaps"] += 1
                    stats["idle_gaps_over_threshold"] += r.at - p.end > ttl
                    loss = full.read - limited.read
                    if loss <= 0:
                        continue
                    stats["simulated_expiries"] += 1
                    stats["lost_cache_read_tokens"] += loss
                    if r.read is not None:
                        stats["observed_read_below_half_of_prefix"] += r.read < 0.5 * full.read
                    # Ideal warm-ups know when the next request arrives. The
                    # scheduled estimate sends every 90% TTL for up to an hour
                    # (including warm-ups on longer gaps). Both omit output.
                    miss_weight = 2 if ttl == 3600 and cache.explicit_write else cache.write_weight
                    gross = loss * (miss_weight - cache.read_weight)
                    read_price = full.read * cache.read_weight
                    stats["ideal_keepalive_net"] += gross - (math.ceil(gap / ttl) - 1) * read_price
                    if ttl == 300 and gap <= 3600:
                        stats["hour_bounded_net"] += gross
            previous = step
    return {"cost": dict(costs), "time_scenarios": {ttl: dict(stats) for ttl, stats in scenarios.items()}}


def quality_report(traces, since, width=10, lookback=30):
    totals = {"before": Counter(), "after": Counter()}
    for steps in traces:
        boundaries = [i for i, step in enumerate(steps) if step.rebuild_record]
        for j, i in enumerate(boundaries):
            if steps[i].request.at < since:
                continue
            begin = max(0, i - width, boundaries[j - 1] if j else 0)
            end = min(len(steps), i + width, boundaries[j + 1] if j + 1 < len(boundaries) else len(steps))
            for label, start, stop in (("before", begin, i), ("after", i, end)):
                counts = totals[label]
                counts["requests"] += stop - start
                prior = {}
                for k in range(max(0, start - lookback), stop):
                    for kind, value in steps[k].request.activity:
                        if k >= start:
                            if kind in {"show", "find"}:
                                counts[kind] += 1
                                counts["l3_previous_tail_hit"] += value
                            if kind in {"file", "repeat"}:
                                last = prior.get((kind, value), -lookback - 1)
                                if k - lookback <= last < start:
                                    counts[kind] += 1
                        if kind in {"file", "repeat"}:
                            prior[kind, value] = k
    return totals


def data_cutoff(traces, since):
    times = sorted(r.at for rows in traces for r in rows)
    if any(t >= since for t in times) or not times:
        return since, "current"
    return times[len(times) * 4 // 5], "older temporal holdout"


def iso_time(t):
    return datetime.fromtimestamp(t, timezone.utc).isoformat()


def pareto(rows):
    # Collapse ties by preferring the default ratio and L2 budget.
    ordered = sorted(rows, key=lambda row: (
        row["rebuilds"], row["predicted"]["cost"],
        abs(row["settings"]["ratio"] - 0.5), abs(row["settings"]["l2"] - 5000),
    ))
    frontier = []
    for row in ordered:
        if not frontier or row["predicted"]["cost"] < frontier[-1]["predicted"]["cost"]:
            frontier.append(row)
    return frontier


def analyze_route(traces, route, since, requested, writer, policy=None):
    cutoff, cohort = data_cutoff(traces, since)
    cache = CACHES[route]
    profiles, calibration_count = learnt_profiles(traces, cutoff, cache)
    steps = prepare(traces, profiles)
    current = replay(steps, profiles, cache, Settings(), cutoff, score=True).summary()
    cache_only = replay(steps, profiles, cache, Settings(), cutoff, observed_windows=True, score=True).summary()
    full_baseline = replay(steps, profiles, cache, Settings()).summary()

    def sweep(start):
        rows = []
        for w, ratio, l2 in product(W_GRID, RATIO_GRID, L2_GRID):
            settings = Settings(w, ratio, l2, requested.l1, requested.trigger)
            outcome = replay(steps, profiles, cache, settings, start).summary()
            rows.append({"settings": asdict(settings), **outcome})
        return rows

    candidate = {}
    if policy is not None:
        configured = Settings(policy["WindowThresholdTokens"], policy["PostRebuildRatio"], policy["L2TargetTokens"])
        configured_steps = prepare(traces, profiles, policy)
        candidate = {"candidate": {
            "provider": ROUTE_PROVIDERS[route],
            "policy": policy,
            "holdout": replay(configured_steps, profiles, cache, configured, cutoff, trace_writer=writer).summary(),
            "full": replay(configured_steps, profiles, cache, configured).summary(),
        }}
    elif writer:
        replay(steps, profiles, cache, requested, cutoff, trace_writer=writer)
    historical = sweep(-math.inf)
    recent = sweep(cutoff)
    return {
        "validation_since": iso_time(cutoff),
        "validation_cohort": cohort,
        "validation_implementations": Counter(
            r.implementation or "unknown" for trace in traces for r in trace if r.at >= cutoff
        ),
        "training_requests": calibration_count,
        **candidate,
        "validation": current, "cache_only_validation": cache_only,
        "full_baseline": full_baseline, "historical_frontier": pareto(historical),
        "holdout_frontier": pareto(recent), "holdout_sweep": recent, "full_sweep": historical,
        "quality": quality_report(steps, cutoff),
        "ttl": ttl_report(steps, profiles, cache, cutoff),
        "ttl_all": ttl_report(steps, profiles, cache, -math.inf),
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("copies", type=Path, nargs="+", help="directories of Session copies, oldest first")
    parser.add_argument("--claude-main", help="base name of a legacy Claude Session without a Claude provider ID")
    parser.add_argument("--since", required=True, help="RFC3339 start of the modern validation cohort")
    parser.add_argument("--config", type=Path, help="runtime.json with Context defaults and provider overrides to replay")
    parser.add_argument("--l1", type=int, default=0, help="post-rebuild L1 token cap (0 uses the target alone)")
    parser.add_argument("--trigger", choices=("growth", "total"), default="growth")
    parser.add_argument("--w", type=int, default=200_000, help="W for the optional request CSV")
    parser.add_argument("--ratio", type=float, default=0.5, help="rebuild target ratio for the optional request CSV")
    parser.add_argument("--l2", type=int, default=5000, help="L2 budget for the optional request CSV")
    parser.add_argument("--request-csv", type=Path, help="per-request results for --config or --w, --ratio and --l2")
    args = parser.parse_args()
    if args.w < 1 or not 0 < args.ratio <= 1 or args.l1 < 0 or args.l2 < 1:
        parser.error("positive W and L2, a target ratio in (0, 1], and a nonnegative L1 cap are required")
    common, routes = None, {}
    if args.config:
        try:
            common, routes = read_context_policies(args.config)
        except (OSError, ValueError) as error:
            parser.error(str(error))

    sessions = read_traces(args.copies, args.claude_main)
    traces = split_routes(sessions)
    settings = Settings(args.w, args.ratio, args.l2, args.l1, args.trigger)

    def report(writer):
        output = {"session_logs": len(sessions), "cache": {key: asdict(v) for key, v in CACHES.items()}}
        for route, provider in ROUTE_PROVIDERS.items():
            if traces[route]:
                output[route] = analyze_route(traces[route], route, timestamp(args.since), settings, writer,
                                              routes.get(provider, common))
        json.dump(output, sys.stdout, indent=2)
        print()

    if args.request_csv:
        # Exclusive creation also prevents accidentally altering an input log.
        with args.request_csv.open("x", newline="") as destination:
            writer = csv.writer(destination)
            writer.writerow(["route", "stream", "sequence", "at", "rebuild", "input",
                             "read", "write", "uncached", "weighted_input", "observed_input",
                             "observed_read", "observed_write", "observed_output"])
            report(writer)
    else:
        report(None)


if __name__ == "__main__":
    main()
