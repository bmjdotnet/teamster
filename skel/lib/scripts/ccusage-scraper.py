#!/usr/bin/env python3
"""Claude Code usage scraper — reads local JSONL logs via ccusage, exposes Prometheus metrics on :9123."""

import json
import logging
import os
import subprocess
import time
import threading

from prometheus_client import Gauge, Counter, start_http_server

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("claude-usage-scraper")

PORT = int(os.environ.get("SCRAPER_PORT", "9123"))
POLL_INTERVAL = int(os.environ.get("POLL_INTERVAL", "60"))

# Daily metrics
daily_input_tokens = Gauge("claude_daily_input_tokens", "Input tokens today")
daily_output_tokens = Gauge("claude_daily_output_tokens", "Output tokens today")
daily_cache_creation_tokens = Gauge("claude_daily_cache_creation_tokens", "Cache creation tokens today")
daily_cache_read_tokens = Gauge("claude_daily_cache_read_tokens", "Cache read tokens today")
daily_total_tokens = Gauge("claude_daily_total_tokens", "Total tokens today")
daily_cost = Gauge("claude_daily_cost_usd", "Estimated cost today (USD)")

# Billing block (5-hour window) metrics
block_total_tokens = Gauge("claude_block_total_tokens", "Tokens in current 5-hour billing block")
block_cost = Gauge("claude_block_cost_usd", "Cost in current 5-hour billing block (USD)")
block_entries = Gauge("claude_block_entries", "API entries in current billing block")
block_start = Gauge("claude_block_start_timestamp", "Start of current billing block (unix)")
block_end = Gauge("claude_block_end_timestamp", "End of current billing block (unix)")
block_is_active = Gauge("claude_block_is_active", "1 if currently in an active billing block")

# Cumulative totals
total_cost = Gauge("claude_total_cost_usd", "Total estimated cost across all recorded sessions")
total_tokens_gauge = Gauge("claude_total_tokens", "Total tokens across all recorded sessions")

# Per-model daily breakdown
model_tokens = Gauge("claude_daily_model_tokens", "Tokens by model today", ["model"])

# Scraper health
scrape_success = Gauge("claude_usage_scrape_success", "1 if last scrape succeeded")
scrape_timestamp = Gauge("claude_usage_scrape_timestamp", "Unix timestamp of last successful scrape")


def run_ccusage(subcommand, extra_args=None):
    cmd = ["ccusage"] + subcommand.split() + ["--json"]
    if extra_args:
        cmd.extend(extra_args)
    result = subprocess.run(cmd, capture_output=True, text=True, timeout=30,
                            env={**os.environ, "HOME": os.environ.get("CCUSAGE_HOME", os.path.expanduser("~"))})
    if result.returncode != 0:
        raise RuntimeError(f"ccusage {subcommand} failed: {result.stderr[:200]}")
    return json.loads(result.stdout)


def scrape():
    try:
        daily_data = run_ccusage("daily")
        today = time.strftime("%Y-%m-%d")

        today_entry = None
        if "daily" in daily_data:
            for entry in daily_data["daily"]:
                if entry.get("period") == today:
                    today_entry = entry
                    break

        if today_entry:
            daily_input_tokens.set(today_entry.get("inputTokens", 0))
            daily_output_tokens.set(today_entry.get("outputTokens", 0))
            daily_cache_creation_tokens.set(today_entry.get("cacheCreationTokens", 0))
            daily_cache_read_tokens.set(today_entry.get("cacheReadTokens", 0))
            daily_total_tokens.set(today_entry.get("totalTokens", 0))
            daily_cost.set(today_entry.get("totalCost", 0))

            for model in today_entry.get("modelsUsed", []):
                model_tokens.labels(model=model).set(1)
        else:
            daily_input_tokens.set(0)
            daily_output_tokens.set(0)
            daily_cache_creation_tokens.set(0)
            daily_cache_read_tokens.set(0)
            daily_total_tokens.set(0)
            daily_cost.set(0)

        if "totals" in daily_data:
            totals = daily_data["totals"]
            total_cost.set(totals.get("totalCost", 0))
            total_tokens_gauge.set(totals.get("totalTokens", 0))

        blocks_data = run_ccusage("blocks")
        if "blocks" in blocks_data:
            active_block = None
            for b in reversed(blocks_data["blocks"]):
                if b.get("isActive") and not b.get("isGap"):
                    active_block = b
                    break

            if active_block:
                block_is_active.set(1)
                block_total_tokens.set(active_block.get("totalTokens", 0))
                block_cost.set(active_block.get("costUSD", 0))
                block_entries.set(active_block.get("entries", 0))
                from datetime import datetime, timezone
                try:
                    st = datetime.fromisoformat(active_block["startTime"].replace("Z", "+00:00")).timestamp()
                    block_start.set(st)
                    et = datetime.fromisoformat(active_block["endTime"].replace("Z", "+00:00")).timestamp()
                    block_end.set(et)
                except (ValueError, KeyError):
                    pass
            else:
                block_is_active.set(0)
                block_total_tokens.set(0)
                block_cost.set(0)
                block_entries.set(0)

        scrape_success.set(1)
        scrape_timestamp.set(time.time())
        log.info("Scrape OK — today: %.0f tokens, $%.2f | block: %.0f tokens, %d entries",
                 today_entry.get("totalTokens", 0) if today_entry else 0,
                 today_entry.get("totalCost", 0) if today_entry else 0,
                 active_block.get("totalTokens", 0) if active_block else 0,
                 active_block.get("entries", 0) if active_block else 0)

    except Exception as e:
        log.error("Scrape failed: %s", str(e)[:300])
        scrape_success.set(0)


def run_loop():
    while True:
        scrape()
        time.sleep(POLL_INTERVAL)


if __name__ == "__main__":
    log.info("Starting Claude usage scraper (ccusage) on :%d (poll every %ds)", PORT, POLL_INTERVAL)
    start_http_server(PORT)
    run_loop()
