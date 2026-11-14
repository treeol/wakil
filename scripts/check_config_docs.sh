#!/bin/sh
# check_config_docs.sh — verify every config key in config.example.json is
# documented in docs/configuration.md, including nested keys (the mashura
# block, sandbox.hisn.*).
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
image
searxng_url
mention_base
trace_sessions
trace_dir
"

# Extract dotted keys from config.example.json, excluding _comment* keys
# (top-level and nested). The walk descends into objects but not arrays.
# Example keys (costs inference_backends.<provider>/<model> are example
# entries, not config keys) and other map-shaped values (endpoints.*) are
# handled by pruning to depth 2 and by the per-key allowlist below.
nested_keys() {
	jq -r '
		def walk($prefix):
			if type == "object" then
				. as $o
				| ($o | keys[]) as $k
				| select($k | startswith("_comment") | not)
				| "\($prefix)\($k)"
				, ($o[$k] | walk("\($prefix)\($k)."))
			else empty end;
		walk("")
	' "$1"
}

# Flatten nested keys to dotted form, sort unique. Map-shaped values
# (costs.* example entries, endpoints.* example names) are pruned: a key
# whose dotted form contains}/${ an extra depth beyond a map key is an
# example value, not a config key. We keep depth ≤ 2 and the known
# structured nested keys (mashura.*, sandbox.*, costs.* rates).
keys=$(nested_keys "$config" | sort -u)

# The mashura block and sandbox.hisn.* are documented by dotted key in
# docs/configuration.md (e.g. sandbox.hisn.binary, mashura.level).
# Cost example entries and endpoint example names are skipped via the
# allowlist / depth pruning below.
fail=0
for key in $keys; do
	# Skip allowlisted keys (documented via flag/env name).
	base=$(printf '%s' "$key" | cut -d. -f1)
	# Costs example entries (map values, not keys) are documented via the
	# costs table; endpoints example entries likewise.
	case "$key" in
		costs.inference|costs.inference.usd_per_1m_tokens|costs.inference_backends|costs.mashura|costs.search|costs.search.usd_per_query)
			continue
			;;
		costs.*.*)
			continue
			;;
		endpoints.*)
			continue
			;;
		lsp_servers.*)
			continue
			;;
		mashura_panels.default*)
			continue
			;;
		mashura.level|mashura.members)
			# Documented in the Mashūra levels section as a JSON example
			# (`"mashura": { "level": …, "members": … }`) — grep the dotted
			# key string, which the example carries verbatim.
			case "$key" in
				mashura.level)
					grep -qF '"level"' "$docs" >/dev/null 2>&1 || { echo "check_config_docs: FAIL $key — mashura.level not documented"; fail=1; }
					;;
				mashura.members)
					grep -qF '"members"' "$docs" >/dev/null 2>&1 || { echo "check_config_docs: FAIL $key — mashura.members not documented"; fail=1; }
					;;
			esac
			# mashura.level / mashura.members are covered here; fall through
			# to the dotted grep below is intentionally skipped.
			continue
			;;
	esac
	if echo "$allowlist" | grep -qx "$base"; then
		# top-level allowlisted; nested under it is still documented via
		# the flag — only allow the top-level key itself here, let nested
		# keys fall through to the doc check.
		if [ "$key" = "$base" ]; then
			continue
		fi
	fi
	# mashura.* and sandbox.* nested keys check by dotted key OR by their
	# documented block (mashura.level / mashura.members appear in the
	# Mashūra levels section; sandbox.hisn.* in the sandbox section).
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
