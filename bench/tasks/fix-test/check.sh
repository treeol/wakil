#!/bin/sh
# check.sh for fix-test: copies the hidden test in and runs the package tests.
set -eu
cp "$(dirname "$0")/hidden/wordcount_test.go" wordcount/wordcount_test.go
go test ./wordcount/
