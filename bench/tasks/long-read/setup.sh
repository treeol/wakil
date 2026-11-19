#!/bin/sh
# setup.sh for long-read: generate the 60 prose files (fixed seed).
# Runs with the work copy as the working directory.
set -eu
go run ./cmd/gen
# The generator is not part of the task; remove it so the agent only sees
# the generated docs/ (plus the module scaffolding it may need).
rm -rf cmd
