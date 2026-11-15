#!/bin/sh
# release_notes.sh — print the CHANGELOG.md section for a released version.
#
# Usage:
#   scripts/release_notes.sh <version>
#
# The version is a bare number (0.8.3) or the same with a leading v (v0.8.3).
# The section runs from its top-level heading ("## [0.8.3] - ...") up to (not
# including) the next top-level heading. Exit status is non-zero when the
# section is missing or empty, so callers can distinguish "no notes" from
# "empty notes" and fall back to generated notes if they want to.
#
# Pure POSIX sh with a small awk filter. Sections are delimited by "^## "
# lines; subsection heads ("### ...") are kept.
set -eu

raw=${1:-}
ver=$(printf '%s' "$raw" | sed -e 's/^v//')
changelog="CHANGELOG.md"

if [ -z "$ver" ]; then
	echo "release_notes: usage: $0 <version> (e.g. 0.8.3 or v0.8.3)" >&2
	exit 1
fi

if [ ! -f "$changelog" ]; then
	echo "release_notes: $changelog not found (run from repo root)" >&2
	exit 1
fi

# Extract from the version heading up to (not including) the next "^## "
# line. The version is matched literally (index, not regex), so dots and
# any '+' in a pre-release tag never act as metacharacters. Turn on at a
# heading iff it is exactly this version ("## [X.Y.Z]" or "## [X.Y.Z] -"),
# so a shorter request like "0.8" never matches "0.8.3".
body=$(awk -v ver="$ver" '
  index($0, "## [" ver "]") == 1 {
    rest = substr($0, length("## [" ver "]") + 1)
    if (rest == "" || index(rest, " -") == 1) found = 1
  }
  found && index($0, "## ") == 1 && !(index($0, "## [" ver "]") == 1 && (rest == "" || index(rest, " -") == 1)) { exit }
  found { print }
' "$changelog")

if [ -z "$body" ]; then
	echo "release_notes: no CHANGELOG section for version $ver" >&2
	exit 1
fi

# Body must have prose below the heading line.
body_without_heading=$(printf '%s\n' "$body" | tail -n +2)
trimmed=$(printf '%s' "$body_without_heading" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e '/./,$!d')
if [ -z "$trimmed" ]; then
	echo "release_notes: CHANGELOG section for version $ver is empty" >&2
	exit 1
fi

printf '%s\n' "$body"
