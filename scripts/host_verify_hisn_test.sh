#!/bin/sh
# host_verify_hisn_test.sh — asserts the hisn floor helpers of
# host_verify_hisn.sh: version_ge, the one-line message, and the check over
# fake hisn binaries (shell stubs in a temp dir that print a version; no real
# hisn is run, nothing is installed, no network).
set -eu
here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=host_verify_hisn.sh
. "$here/host_verify_hisn.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/hv-hisn.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
bad=0

# ok <case> <got> <want>: compares whole strings.
ok() {
    if [ "$2" = "$3" ]; then
        echo "  PASS $1"
    else
        echo "  FAIL $1" >&2
        printf '  want:\n%s\n  got:\n%s\n' "$3" "$2" >&2
        bad=$((bad + 1))
    fi
}

# fake <name> <version-output>: a stub that prints the output for --version.
fake() {
    printf '#!/bin/sh\necho "%s"\n' "$2" > "$tmp/$1"
    chmod +x "$tmp/$1"
}

# version_ge: the cases that used to be inline in host_verify.sh.
vge() {
    if version_ge "$1" "$2"; then got=true; else got=false; fi
    ok "version_ge $1 $2" "$got" "$3"
}
vge "0.6.0" "0.6.0" true
vge "0.6.0" "0.6.1" false
vge "0.6.1" "0.6.0" true
vge "0.5.9" "0.6.0" false
vge "0.7.0" "0.6.0" true
vge "0.10.0" "0.9.0" true
vge "0.6.0-dev1" "0.6.0" true
vge "hisn 0.6.0" "0.6.0" true
vge "0.9.0" "1.0.0" false

# The message: nothing when the version is enough, one line naming found,
# required and path otherwise.
ok "message: equal" "$(hv_hisn_floor_line 0.6.7 0.6.7 /opt/hisn)" ""
ok "message: newer" "$(hv_hisn_floor_line 0.6.8 0.6.7 /opt/hisn)" ""
want="hisn 0.6.5 at /usr/local/bin/hisn is below the required 0.6.7; get hisn 0.6.7 from https://github.com/treeol/hisn/releases/tag/v0.6.7 (the asset for your architecture and SHA256SUMS), put it first on PATH or set WAKIL_HISN_BIN"
ok "message: older" "$(hv_hisn_floor_line 0.6.5 0.6.7 /usr/local/bin/hisn)" "$want"
ok "message: one line" "$(hv_hisn_floor_line 0.6.5 0.6.7 /usr/local/bin/hisn | wc -l | tr -d ' ')" "1"

# The check over fake binaries. hv_hisn_floor_check <required> reads
# WAKIL_HISN_BIN and PATH; it prints one line per binary below the floor and
# returns 0 (fine), 1 (a binary below the floor) or 77 (no hisn to check).
fake hisn-old "hisn 0.6.5"
fake hisn-new "hisn 0.6.7"
fake hisn-newer "hisn 0.10.0"
fake hisn-junk "something unexpected"
mkdir "$tmp/oldbin" "$tmp/newbin" "$tmp/emptybin"
cp "$tmp/hisn-old" "$tmp/oldbin/hisn"
cp "$tmp/hisn-new" "$tmp/newbin/hisn"
# A PATH of our own: only the tools the helpers call, so a real hisn in
# /usr/bin or elsewhere on the host can never leak into a case.
mkdir "$tmp/tools"
for tool in sed cut head; do
    ln -s "$(command -v "$tool")" "$tmp/tools/$tool"
done
basepath="$tmp/tools"

run() { # run <WAKIL_HISN_BIN or -> <PATH dir or -> ; prints "rc|output"
    out=$(
        if [ "$1" = "-" ]; then unset WAKIL_HISN_BIN; else WAKIL_HISN_BIN="$1"; export WAKIL_HISN_BIN; fi
        if [ "$2" = "-" ]; then PATH=$basepath; else PATH="$2:$basepath"; fi
        export PATH
        hv_hisn_floor_check 0.6.7
    ) && rc=0 || rc=$?
    printf '%s|%s' "$rc" "$out"
}

ok "check: none anywhere" "$(run - -)" "77|"
ok "check: PATH hisn new enough" "$(run - "$tmp/newbin")" "0|"
ok "check: WAKIL_HISN_BIN new enough" "$(run "$tmp/hisn-newer" -)" "0|"
ok "check: PATH hisn too old" "$(run - "$tmp/oldbin")" "1|$(hv_hisn_floor_line 0.6.5 0.6.7 "$tmp/oldbin/hisn")"
ok "check: WAKIL_HISN_BIN too old" "$(run "$tmp/hisn-old" -)" "1|$(hv_hisn_floor_line 0.6.5 0.6.7 "$tmp/hisn-old")"
# Both are checked: a new WAKIL_HISN_BIN does not hide an old PATH hisn, which
# the real-hisn step uses.
ok "check: new WAKIL_HISN_BIN, old PATH hisn" "$(run "$tmp/hisn-new" "$tmp/oldbin")" "1|$(hv_hisn_floor_line 0.6.5 0.6.7 "$tmp/oldbin/hisn")"
# Both old: two lines, one per binary.
two=$(hv_hisn_floor_line 0.6.5 0.6.7 "$tmp/hisn-old"; hv_hisn_floor_line 0.6.5 0.6.7 "$tmp/oldbin/hisn")
ok "check: both old" "$(run "$tmp/hisn-old" "$tmp/oldbin")" "1|$two"
# The same binary named twice is checked once.
ok "check: same binary twice" "$(run "$tmp/oldbin/hisn" "$tmp/oldbin")" "1|$(hv_hisn_floor_line 0.6.5 0.6.7 "$tmp/oldbin/hisn")"
# An unparseable version is not a floor failure (the real-hisn step handles it).
ok "check: unparseable version" "$(run "$tmp/hisn-junk" -)" "0|"
# A WAKIL_HISN_BIN that is not executable is not a floor failure either.
ok "check: missing WAKIL_HISN_BIN" "$(run "$tmp/does-not-exist" -)" "77|"

[ "$bad" -eq 0 ] || { echo "host_verify_hisn_test: $bad failed" >&2; exit 1; }
echo "host_verify_hisn_test: ok"
