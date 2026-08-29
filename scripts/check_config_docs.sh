#!/bin/sh
# check_config_docs.sh — verify every config key in config.example.json is
# documented in docs/configuration.md.
#
# This catches drift: when a new config key is added to the example config
# but not documented, the CI gate fails. Keys that are documented via their
# flag or env var name (not the JSON key) are listed in the allowlist below.
#
# Usage:
#   scripts/check_config_docs.sh
#   (run from repo root — needs jq)
set -eu

config="config.example.json"
docs="docs/configuration.md"

if [ ! -f "$config" ]; then
	echo "check_config_docs: $config not found (run from repo root)"
	exit 1
fi
if [ ! -f "$docs" ]; then
	echo "check_config_docs: $docs not found"
	exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
	echo "check_config_docs: jq is required but not installed"
	exit 1
fi

# Keys documented via flag/env name in the flags table, not as JSON keys.
# These are still documented — just under a different name — so they're allowed.
allowlist="
exec_mode
image
searxng_url
google_api_key
google_cx
mention_base
trace_sessions
trace_dir
"

# Extract top-level keys from config.example.json, excluding _comment* keys.
keys=$(jq -r 'keys[] | select(startswith("_comment") | not)' "$config" | sort)

fail=0
for key in $keys; do
	# Skip allowlisted keys (documented via flag/env name).
	if echo "$allowlist" | grep -qx "$key"; then
		continue
	fi
	# Check for the JSON key as a whole word in the docs.
	if grep -wF "$key" "$docs" >/dev/null 2>&1; then
		echo "check_config_docs: ok   $key"
	else
		echo "check_config_docs: FAIL $key — not found in $docs"
		fail=1
	fi
done

if [ "$fail" -ne 0 ]; then
	echo "check_config_docs: undocumented config key(s) — add to $docs or the allowlist in this script"
	exit 1
fi
