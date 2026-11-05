#!/bin/sh
# check_go_version_pins.sh — verify the Go toolchain pinned in the Dockerfiles
# equals the `go` directive in go.mod.
#
# Pins stay literal in the Dockerfiles (they are image tags). This check makes
# drift fail CI instead of silently building with a different toolchain.
#
# Usage:
#   scripts/check_go_version_pins.sh
#   (run from repo root)
set -eu

gomod="go.mod"
dockerfiles="Dockerfile Dockerfile.daemon"

if [ ! -f "$gomod" ]; then
	echo "check_go_version_pins: $gomod not found (run from repo root)"
	exit 1
fi

want=$(sed -n 's/^go[[:space:]]\{1,\}\([0-9][0-9.]*\)[[:space:]]*$/\1/p' "$gomod")
if [ -z "$want" ]; then
	echo "check_go_version_pins: no 'go' directive found in $gomod"
	exit 1
fi

status=0
for df in $dockerfiles; do
	if [ ! -f "$df" ]; then
		echo "check_go_version_pins: $df not found"
		status=1
		continue
	fi
	# Every golang:<version>[-variant] image reference in the file must carry
	# the go.mod version.
	pins=$(sed -n 's/^[^#]*FROM[[:space:]]\{1,\}golang:\([0-9][0-9.]*\)-.*$/\1/p' "$df")
	if [ -z "$pins" ]; then
		echo "check_go_version_pins: no 'FROM golang:<version>' line in $df"
		status=1
		continue
	fi
	for pin in $pins; do
		if [ "$pin" != "$want" ]; then
			echo "check_go_version_pins: $df pins golang:$pin but $gomod says go $want"
			status=1
		fi
	done
done

if [ "$status" -ne 0 ]; then
	exit 1
fi
echo "check_go_version_pins: Dockerfiles pin golang $want, matching $gomod"
