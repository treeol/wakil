#!/bin/sh
# check.sh for rename: builds, tests pass, old name gone.
set -eu
go build ./...

# The old name must be gone from every Go source file.
if grep -rn 'OldName' --include='*.go' . >/dev/null 2>&1; then
    echo "old name still present:"; grep -rn 'OldName' --include='*.go' .; exit 1
fi

# And the new name must actually be used.
grep -rn 'NewName' --include='*.go' . >/dev/null 2>&1 || { echo "new name unused"; exit 1; }

go test ./...
