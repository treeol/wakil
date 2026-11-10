#!/bin/sh
# install.sh - quick installer for Wakil, hisn and kvr-server.
#
#   curl -fsSL https://github.com/treeol/wakil/releases/latest/download/install.sh -o install.sh
#   sh install.sh [--help]
#
# What it does: downloads the Wakil release archive (and, on Linux, the hisn
# sandbox binary and the optional kvr-server staging binary), verifies EVERY
# download against the sha256 sums published with that release BEFORE anything
# is extracted, made executable or moved, and only then installs. If one check
# fails nothing is installed. It never edits shell rc files, never calls a
# package manager and never starts a service.
#
# Do not run it under `sh -x`: tracing prints expanded values. The script itself
# never expands OPENROUTER_API_KEY (it tests with ${VAR:+set}, which traces as
# the word "set"), but other tools would show your environment.
#
# Config: it never writes a config itself. After the install it keeps an
# existing config, or runs `wakil init` (see "config" below), or prints the next
# step. The API key never passes through this script: OPENROUTER_API_KEY is only
# tested for being set, and `wakil init --key-env` records the variable NAME.
#
# Test-only: WAKIL_INSTALL_TEST_CORRUPT=1 corrupts every downloaded artifact
# (a value other than 1 corrupts only artifacts whose file name contains it),
# so CI and maintainers can prove that a checksum mismatch aborts the install.
# Never set it in normal use.
#
# The manual install in the README Quickstart remains the reference path.

set -eu

# Keep HISN_VERSION equal to MinVersion in internal/sandbox/hisn/backend.go
# (enforced by TestInstallScriptHisnVersionMatchesMinVersion).
HISN_VERSION="0.4.2"
KVR_VERSION="0.2.0"

WAKIL_REPO="treeol/wakil"
HISN_REPO="treeol/hisn"
KVR_REPO="treeol/kvrust"
MANUAL_URL="https://github.com/treeol/wakil#quickstart"
COSIGN_IDENTITY_RE='^https://github.com/treeol/wakil/\.github/workflows/release\.yml@refs/tags/v'
COSIGN_ISSUER='https://token.actions.githubusercontent.com'
KVR_DEST="/usr/local/bin/kvr-server"
MANIFEST_NAME=".wakil-install.manifest"

# Globals set while running.
TMP=""
CUR_NEW=""
KVR_TMP=""
DL=""
SHA=""
OS=""
ARCH=""
GLIBC=""
GLIBC_OK="no"
PRIV=""
TAG=""
VER=""
DIR=""

OPT_VERSION=""
OPT_PREFIX=""
OPT_NO_HISN=0
OPT_NO_KVR=0
OPT_NO_CONFIG=0
OPT_SUDO=0
OPT_REQUIRE_SIG=0
OPT_UNINSTALL=0

step() { printf '==> %s\n' "$*"; }
say() { printf '    %s\n' "$*"; }
warn() { printf 'install.sh: warning: %s\n' "$*" >&2; }
die() {
	printf 'install.sh: error: %s\n' "$*" >&2
	exit 1
}
have() { command -v "$1" >/dev/null 2>&1; }

cleanup() {
	if [ -n "$CUR_NEW" ]; then
		rm -f "$CUR_NEW" 2>/dev/null || true
		CUR_NEW=""
	fi
	if [ -n "$KVR_TMP" ]; then
		run_priv rm -f "$KVR_TMP" 2>/dev/null || true
		KVR_TMP=""
	fi
	if [ -n "$TMP" ]; then
		rm -rf "$TMP"
		TMP=""
	fi
}

usage() {
	cat <<'EOF'
Usage: sh install.sh [options]

Installs Wakil (and, on Linux, hisn and kvr-server) with sha256 verification.

Options:
  --version vX.Y.Z    install this Wakil release (default: the latest release)
  --prefix DIR        install directory (default: $WAKIL_INSTALL_DIR, else ~/.local/bin)
  --no-hisn           do not install hisn (Wakil then uses Docker)
  --no-kvr            do not install kvr-server (staging stays off)
  --no-config         do not run `wakil init` after installing
  --sudo              allow sudo (may prompt) to place kvr-server in /usr/local/bin
  --require-signature fail unless the release signature was verified with cosign
  --uninstall         remove what this script installed (config and data are kept)
  -h, --help          show this help

Manual install: https://github.com/treeol/wakil#quickstart
EOF
}

# ── helpers ───────────────────────────────────────────────────────────────

# fetch URL DEST: download over https only (curl), or with wget.
fetch() {
	case $DL in
	curl) curl -fsSL --proto '=https' --proto-redir '=https' --retry 2 -o "$2" "$1" ;;
	wget) wget -q --https-only -O "$2" "$1" ;;
	esac
}

sha256_of() {
	case $SHA in
	sha256sum) sha256sum "$1" | awk '{print $1}' ;;
	shasum) shasum -a 256 "$1" | awk '{print $1}' ;;
	esac
}

# verify_sha256 FILE NAME SUMSFILE: FILE must match the one entry for NAME.
verify_sha256() {
	_vf=$1
	_vn=$2
	_vs=$3
	_vexp=$(awk -v n="$_vn" '{ f = $2; sub(/^\*/, "", f); if (f == n) print $1 }' "$_vs")
	case $_vexp in
	'') die "no checksum entry for $_vn: nothing installed" ;;
	*[!0-9a-f]*) die "checksum entry for $_vn is not a lowercase sha256 hex string (or appears twice): nothing installed" ;;
	esac
	[ "${#_vexp}" -eq 64 ] || die "checksum entry for $_vn is not 64 hex characters: nothing installed"
	_vact=$(sha256_of "$_vf")
	if [ "$_vact" != "$_vexp" ]; then
		die "checksum mismatch for $_vn (expected $_vexp, got $_vact): nothing installed"
	fi
	say "sha256 ok: $_vn"
}

# entry_count NAME SUMSFILE: how many entries SUMSFILE has for NAME.
entry_count() {
	awk -v n="$1" '{ f = $2; sub(/^\*/, "", f); if (f == n) c++ } END { print c + 0 }' "$2"
}

# maybe_corrupt FILE NAME: test hook, see the header.
maybe_corrupt() {
	[ -n "${WAKIL_INSTALL_TEST_CORRUPT:-}" ] || return 0
	case $2 in checksums.txt* | SHA256SUMS) return 0 ;; esac
	if [ "$WAKIL_INSTALL_TEST_CORRUPT" != 1 ]; then
		case $2 in *"$WAKIL_INSTALL_TEST_CORRUPT"*) ;; *) return 0 ;; esac
	fi
	printf 'x' >>"$1"
	warn "test hook: corrupted $2"
}

# get_verified URL NAME SUMSFILE: download into $TMP/NAME and verify it.
get_verified() {
	fetch "$1" "$TMP/$2" || die "download failed: $1"
	maybe_corrupt "$TMP/$2" "$2"
	verify_sha256 "$TMP/$2" "$2" "$3"
}

# version_ge A B: dotted numeric A >= B.
version_ge() {
	_a=$1
	_b=$2
	case $_a$_b in *[!0-9.]* | '') return 1 ;; esac
	while [ -n "$_a" ] || [ -n "$_b" ]; do
		_ap=${_a%%.*}
		_bp=${_b%%.*}
		[ -n "$_ap" ] || _ap=0
		[ -n "$_bp" ] || _bp=0
		case $_a in *.*) _a=${_a#*.} ;; *) _a="" ;; esac
		case $_b in *.*) _b=${_b#*.} ;; *) _b="" ;; esac
		if [ "$_ap" -gt "$_bp" ]; then return 0; fi
		if [ "$_ap" -lt "$_bp" ]; then return 1; fi
	done
	return 0
}

valid_tag() {
	case $1 in v[0-9]*) ;; *) return 1 ;; esac
	case $1 in *[!0-9A-Za-z._+-]*) return 1 ;; esac
	return 0
}

# glibc_version prints the glibc version ("" on non-glibc systems).
glibc_version() {
	_gv=""
	if have getconf; then
		_gv=$(getconf GNU_LIBC_VERSION 2>/dev/null | awk '{print $2}') || _gv=""
	fi
	if [ -z "$_gv" ] && have ldd; then
		_gl=$(ldd --version 2>&1 | head -n 1) || _gl=""
		case $_gl in
		*GLIBC* | *"GNU libc"*) _gv=${_gl##* } ;;
		esac
	fi
	case $_gv in
	[0-9]*.[0-9]*) printf '%s' "$_gv" ;;
	esac
}

glibc_at_least_235() {
	[ -n "$GLIBC" ] || return 1
	_gmaj=${GLIBC%%.*}
	_grest=${GLIBC#*.}
	_gmin=${_grest%%[!0-9]*}
	case $_gmaj$_gmin in *[!0-9]* | '') return 1 ;; esac
	[ "$_gmaj" -gt 2 ] && return 0
	[ "$_gmaj" -eq 2 ] && [ "${_gmin:-0}" -ge 35 ]
}

# ── argument parsing ──────────────────────────────────────────────────────

parse_args() {
	while [ $# -gt 0 ]; do
		case $1 in
		--version)
			[ $# -ge 2 ] || die "--version needs a value like v0.6.0"
			OPT_VERSION=$2
			shift
			;;
		--version=*) OPT_VERSION=${1#--version=} ;;
		--prefix)
			[ $# -ge 2 ] || die "--prefix needs a directory"
			OPT_PREFIX=$2
			shift
			;;
		--prefix=*) OPT_PREFIX=${1#--prefix=} ;;
		--no-hisn) OPT_NO_HISN=1 ;;
		--no-kvr) OPT_NO_KVR=1 ;;
		--no-config) OPT_NO_CONFIG=1 ;;
		--sudo) OPT_SUDO=1 ;;
		--require-signature) OPT_REQUIRE_SIG=1 ;;
		--uninstall) OPT_UNINSTALL=1 ;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			usage >&2
			die "unknown option: $1"
			;;
		esac
		shift
	done
}

# ── environment checks ───────────────────────────────────────────────────

check_tools() {
	if have curl; then
		DL=curl
	elif have wget; then
		# Every download must be https-only; a wget that cannot enforce that
		# (BusyBox) is refused rather than trusted.
		wget --help 2>&1 | grep -q -e '--https-only' ||
			die "this wget cannot restrict downloads to https (--https-only); install curl or GNU wget"
		DL=wget
	else
		die "need curl or wget"
	fi
	if have sha256sum; then
		SHA=sha256sum
	elif have shasum; then
		SHA=shasum
	else
		die "need sha256sum or shasum"
	fi
	have tar || die "need tar"
	have uname || die "need uname"
	have install || die "need install"
	have awk || die "need awk"
	have mktemp || die "need mktemp"
}

detect_platform() {
	case $(uname -s) in
	Linux) OS=linux ;;
	Darwin) OS=darwin ;;
	*) die "unsupported OS $(uname -s): see the manual install at $MANUAL_URL" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) ARCH=amd64 ;;
	aarch64 | arm64) ARCH=arm64 ;;
	*) die "unsupported CPU $(uname -m): see the manual install at $MANUAL_URL" ;;
	esac
	if [ "$OS" = linux ]; then
		GLIBC=$(glibc_version)
		if glibc_at_least_235; then GLIBC_OK=yes; fi
	fi
}

resolve_install_dir() {
	[ -n "${HOME:-}" ] || die "HOME is not set"
	if [ -n "$OPT_PREFIX" ]; then
		DIR=$OPT_PREFIX
	elif [ -n "${WAKIL_INSTALL_DIR:-}" ]; then
		DIR=$WAKIL_INSTALL_DIR
	else
		DIR=$HOME/.local/bin
	fi
	case $DIR in
	/*) ;;
	*) DIR=$PWD/$DIR ;;
	esac
	DIR=${DIR%/}
	[ -n "$DIR" ] || die "refusing to install into /"
}

resolve_version() {
	if [ -n "$OPT_VERSION" ]; then
		TAG=$OPT_VERSION
		case $TAG in v*) ;; *) TAG=v$TAG ;; esac
		valid_tag "$TAG" || die "invalid version: $OPT_VERSION (expected vX.Y.Z)"
	else
		_url="https://github.com/$WAKIL_REPO/releases/latest"
		case $DL in
		curl) _loc=$(curl -fsS --proto '=https' --proto-redir '=https' -o /dev/null -w '%{redirect_url}' "$_url") || _loc="" ;;
		wget) _loc=$(wget -S --https-only --max-redirect=0 -O /dev/null "$_url" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | head -n 1) || _loc="" ;;
		esac
		_loc=$(printf '%s' "$_loc" | tr -d '\r\n ')
		case $_loc in
		*/releases/tag/*) TAG=${_loc##*/} ;;
		*) die "could not resolve the latest release from $_url; pass --version vX.Y.Z" ;;
		esac
		valid_tag "$TAG" || die "latest release tag looks wrong ($TAG); pass --version vX.Y.Z"
	fi
	VER=${TAG#v}
}

# ── plans ─────────────────────────────────────────────────────────────────

HISN_ASSET=""
HISN_NOTE=""
HISN_KEEP_BIN=""
HISN_KEEP_VER=""
HISN_STATE="none" # none | install | keep

plan_hisn() {
	if [ "$OPT_NO_HISN" = 1 ]; then
		HISN_NOTE="hisn skipped (--no-hisn); Wakil will use Docker"
		return 0
	fi
	if [ "$OS" != linux ]; then
		HISN_NOTE="hisn is Linux-only; Wakil will use Docker"
		return 0
	fi
	# A hisn the user already has on PATH and that is new enough is kept as it
	# is. This runs `hisn --version` on the user's own binary (it is not a
	# download), which is the only program executed before verification.
	_c=$(command -v hisn 2>/dev/null || true)
	if [ -n "$_c" ] && [ -x "$_c" ]; then
		_cv=$("$_c" --version </dev/null 2>/dev/null | sed -n 's/^hisn \([0-9][0-9.]*\).*/\1/p' | head -n 1) || _cv=""
		if [ -n "$_cv" ] && version_ge "$_cv" "$HISN_VERSION"; then
			HISN_STATE=keep
			HISN_KEEP_BIN=$_c
			HISN_KEEP_VER=$_cv
			HISN_NOTE="found hisn $_cv at $_c (>= $HISN_VERSION): keeping it"
			return 0
		fi
	fi
	# A hisn in the install dir that this script did not put there is not ours
	# to replace (it could be newer). Left untouched and not executed.
	if [ -e "$DIR/hisn" ] || [ -L "$DIR/hisn" ]; then
		if ! grep -qxF "$DIR/hisn" "$DIR/$MANIFEST_NAME" 2>/dev/null; then
			HISN_NOTE="found $DIR/hisn that this script did not install: left untouched (needs hisn >= $HISN_VERSION; remove it and re-run to get $HISN_VERSION)"
			return 0
		fi
	fi
	if [ "$GLIBC_OK" = yes ]; then
		case $ARCH in
		amd64) HISN_ASSET=hisn-x86_64-linux-gnu ;;
		arm64) HISN_ASSET=hisn-aarch64-linux-gnu ;;
		esac
		HISN_NOTE="hisn: $HISN_ASSET (glibc $GLIBC)"
	elif [ "$ARCH" = amd64 ]; then
		HISN_ASSET=hisn-x86_64-linux-musl
		HISN_NOTE="hisn: $HISN_ASSET (no glibc >= 2.35 found); note: the musl build is not leak-tested"
	else
		HISN_NOTE="hisn needs glibc >= 2.35 on arm64 (found: ${GLIBC:-none}); skipped, Wakil will use Docker"
		return 0
	fi
	HISN_STATE=install
}

KVR_ASSET=""
KVR_NOTE=""

detect_priv() {
	if [ "$(id -u)" = 0 ]; then
		PRIV=root
	elif have sudo && sudo -n true >/dev/null 2>&1; then
		PRIV=sudo
	elif [ "$OPT_SUDO" = 1 ]; then
		have sudo || die "--sudo given but sudo is not installed"
		PRIV=sudo
	fi
}

run_priv() {
	case $PRIV in
	root) "$@" ;;
	sudo) sudo "$@" ;;
	*) return 1 ;;
	esac
}

plan_kvr() {
	if [ "$OPT_NO_KVR" = 1 ]; then
		KVR_NOTE="kvr-server skipped (--no-kvr); staging stays off"
		return 0
	fi
	if [ "$OS" != linux ] || [ "$HISN_STATE" = none ]; then
		KVR_NOTE="kvr-server not needed (it serves the hisn sandbox only)"
		return 0
	fi
	if [ "$ARCH" = amd64 ]; then
		KVR_ASSET=kvr-server-x86_64-linux-musl
	elif [ "$GLIBC_OK" = yes ]; then
		KVR_ASSET=kvr-server-aarch64-linux-gnu
	else
		KVR_NOTE="kvr-server for arm64 needs glibc >= 2.35; skipped, staging stays off"
		return 0
	fi
	# Never replace a kvr-server somebody already put there.
	if [ -e "$KVR_DEST" ] || [ -L "$KVR_DEST" ]; then
		KVR_NOTE="kvr-server already present at $KVR_DEST: keeping it"
		KVR_ASSET=""
		return 0
	fi
	detect_priv
	if [ -z "$PRIV" ]; then
		_kv="https://github.com/$KVR_REPO/releases/download/v$KVR_VERSION"
		KVR_NOTE="kvr-server skipped: /usr/local/bin needs root (re-run with --sudo, or as root). Staging stays off. To add it later:
      curl -fsSLO --proto '=https' --proto-redir '=https' $_kv/$KVR_ASSET && curl -fsSLO --proto '=https' --proto-redir '=https' $_kv/SHA256SUMS && grep -F '  $KVR_ASSET' SHA256SUMS | sha256sum --check - && sudo install -m 0755 $KVR_ASSET $KVR_DEST"
		KVR_ASSET=""
	fi
}

# ── install helpers ───────────────────────────────────────────────────────

# install_file SRC DEST: install(1) to a sibling file, then rename over DEST,
# so a running old binary is never written to.
install_file() {
	# mv would move into a directory (or a symlink to one) instead of replacing it.
	if [ -d "$2" ]; then
		die "$2 is a directory: refusing to replace it (nothing was changed there)"
	fi
	CUR_NEW=$(mktemp "$2.new.XXXXXX") || die "could not create a temp file next to $2"
	install -m 0755 "$1" "$CUR_NEW" || die "could not write $CUR_NEW"
	mv -f "$CUR_NEW" "$2" || die "could not move $CUR_NEW to $2"
	CUR_NEW=""
}

manifest_add() {
	_m="$DIR/$MANIFEST_NAME"
	if [ -d "$_m" ]; then
		die "$_m is a directory: refusing to use it"
	fi
	CUR_NEW=$(mktemp "$DIR/.manifest.XXXXXX") || die "could not create a temp file in $DIR"
	{
		[ -f "$_m" ] && cat "$_m"
		printf '%s\n' "$1"
	} | sort -u >"$CUR_NEW" || die "could not update $_m"
	mv -f "$CUR_NEW" "$_m" || die "could not update $_m"
	CUR_NEW=""
}

# install_kvr publishes kvr-server without ever replacing an existing file:
# stage under a private name in the same directory, then hard-link it to the
# final name (ln fails if that name exists). Needs run_priv.
install_kvr() {
	[ -d /usr/local/bin ] || run_priv mkdir -p /usr/local/bin || return 1
	KVR_TMP=$(run_priv mktemp /usr/local/bin/.kvr-server.XXXXXX) || return 1
	run_priv install -m 0755 "$TMP/$KVR_ASSET" "$KVR_TMP" || return 1
	# ln would link into a directory that appeared since planning.
	if [ -e "$KVR_DEST" ] || [ -L "$KVR_DEST" ]; then return 1; fi
	run_priv ln "$KVR_TMP" "$KVR_DEST" || return 1
	return 0
}

# ── config ────────────────────────────────────────────────────────────────

tty_usable() {
	[ -r /dev/tty ] && [ -w /dev/tty ] && (: </dev/tty) 2>/dev/null
}

CONFIG_RESULT=""

# setup_config: keep an existing config; else run `wakil init` (non-interactive
# when OPENROUTER_API_KEY is set, interactive when a terminal exists); else say
# what to run next. The key is never read, stored or passed on by this script.
setup_config() {
	_cfg=${WAKIL_CONFIG:-$HOME/.config/wakil/config.json}
	if [ -e "$_cfg" ] || [ -L "$_cfg" ]; then
		CONFIG_RESULT="kept your existing $_cfg"
		return 0
	fi
	# ${VAR:+set} expands to the word "set", never the value, so a traced run
	# (sh -x) cannot print the key from this test.
	if [ "${OPENROUTER_API_KEY:+set}" = set ]; then
		# --key-env records only the variable NAME; the key stays in the environment.
		if "$DIR/wakil" init --provider openrouter --key-env OPENROUTER_API_KEY --yes </dev/null; then
			CONFIG_RESULT="note: OPENROUTER_API_KEY must also be set whenever you run wakil"
		else
			CONFIG_RESULT="wakil init did not finish; run \`wakil init\` yourself"
		fi
		return 0
	fi
	if tty_usable; then
		if "$DIR/wakil" init </dev/tty; then
			CONFIG_RESULT=""
		else
			CONFIG_RESULT="wakil init did not finish; run \`wakil init\` yourself"
		fi
		return 0
	fi
	CONFIG_RESULT="next: run \`wakil init\`"
}

# ── uninstall ─────────────────────────────────────────────────────────────

do_uninstall() {
	_m="$DIR/$MANIFEST_NAME"
	step "Uninstall from $DIR"
	if [ ! -f "$_m" ]; then
		say "no $MANIFEST_NAME in $DIR: nothing this script installed was found there"
	else
		while IFS= read -r _p || [ -n "$_p" ]; do
			case $_p in
			"$DIR/wakil" | "$DIR/hisn")
				rm -f "$_p" && say "removed $_p"
				;;
			"$KVR_DEST")
				if [ -e "$_p" ]; then
					detect_priv
					if [ -n "$PRIV" ] && run_priv rm -f "$_p"; then
						say "removed $_p"
					else
						warn "could not remove $_p (needs root: re-run with --sudo, or: sudo rm $_p)"
					fi
				fi
				;;
			'') ;;
			*) warn "ignoring unexpected manifest entry: $_p" ;;
			esac
		done <"$_m"
		rm -f "$_m"
	fi
	say "left in place (delete yourself if you want them gone):"
	say "  config: $HOME/.config/wakil"
	say "  data:   $HOME/.local/share/wakil"
}

# ── main ──────────────────────────────────────────────────────────────────

main() {
	parse_args "$@"
	trap cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	trap 'exit 129' HUP
	resolve_install_dir
	if [ "$OPT_UNINSTALL" = 1 ]; then
		do_uninstall
		return 0
	fi

	check_tools
	if [ "$OPT_REQUIRE_SIG" = 1 ] && ! have cosign; then
		die "--require-signature needs cosign on PATH (https://docs.sigstore.dev/cosign/installation/)"
	fi
	detect_platform
	step "Platform: $OS/$ARCH${GLIBC:+, glibc $GLIBC}"
	resolve_version
	step "Wakil $TAG"
	plan_hisn
	plan_kvr

	TMP=$(mktemp -d "${TMPDIR:-/tmp}/wakil-install.XXXXXX") || die "could not create a temp dir"
	chmod 700 "$TMP"

	# ── download and verify everything; nothing is installed yet ──
	_base="https://github.com/$WAKIL_REPO/releases/download/$TAG"
	step "Downloading and verifying"
	fetch "$_base/checksums.txt" "$TMP/checksums.txt" || die "could not download $_base/checksums.txt (is $TAG a release?)"
	if have cosign; then
		if fetch "$_base/checksums.txt.sigstore.json" "$TMP/checksums.txt.sigstore.json"; then
			cosign verify-blob "$TMP/checksums.txt" \
				--bundle "$TMP/checksums.txt.sigstore.json" \
				--certificate-identity-regexp "$COSIGN_IDENTITY_RE" \
				--certificate-oidc-issuer "$COSIGN_ISSUER" >/dev/null 2>&1 ||
				die "cosign signature verification of checksums.txt FAILED: nothing installed"
			say "cosign signature ok: checksums.txt"
		elif [ "$OPT_REQUIRE_SIG" = 1 ]; then
			die "no signature bundle for $TAG and --require-signature was given"
		else
			warn "signature not checked: no checksums.txt.sigstore.json for $TAG"
		fi
	else
		say "signature not checked (cosign is not installed); sha256 against the release checksums.txt only"
	fi

	_wasset="wakil_${VER}_${OS}_${ARCH}.tar.gz"
	get_verified "$_base/$_wasset" "$_wasset" "$TMP/checksums.txt"

	if [ "$HISN_STATE" = install ]; then
		_hb="https://github.com/$HISN_REPO/releases/download/v$HISN_VERSION"
		fetch "$_hb/SHA256SUMS" "$TMP/hisn.SHA256SUMS" || die "could not download hisn SHA256SUMS"
		get_verified "$_hb/$HISN_ASSET" "$HISN_ASSET" "$TMP/hisn.SHA256SUMS"
	fi
	if [ -n "$KVR_ASSET" ]; then
		_kb="https://github.com/$KVR_REPO/releases/download/v$KVR_VERSION"
		fetch "$_kb/SHA256SUMS" "$TMP/kvr.SHA256SUMS" || die "could not download kvr-server SHA256SUMS"
		get_verified "$_kb/$KVR_ASSET" "$KVR_ASSET" "$TMP/kvr.SHA256SUMS"
	fi
	mkdir -p "$TMP/x"
	# The archive is already sha256-verified; still refuse odd member names.
	_members=$(tar -tzf "$TMP/$_wasset") || die "could not list $_wasset"
	case $(printf '\n%s\n' "$_members") in
	*"
/"* | *"
../"* | *"/../"* | *"/..
"*) die "$_wasset has an unexpected member path: nothing installed" ;;
	esac
	tar -xzf "$TMP/$_wasset" -C "$TMP/x" wakil || die "could not extract wakil from $_wasset"
	{ [ -f "$TMP/x/wakil" ] && [ ! -L "$TMP/x/wakil" ]; } || die "$_wasset does not contain a regular file named wakil"

	# A checksum-valid file can still be unusable (wrong libc, truncated build):
	# prove the staged binaries start BEFORE replacing anything. They are
	# verified, so running them is allowed.
	chmod 0755 "$TMP/x/wakil"
	_sv=$("$TMP/x/wakil" version </dev/null 2>&1) || die "the downloaded wakil does not run on this system: $_sv (nothing installed)"
	if [ "$HISN_STATE" = install ]; then
		chmod 0755 "$TMP/$HISN_ASSET"
		_sv=$("$TMP/$HISN_ASSET" --version </dev/null 2>&1) || die "the downloaded hisn does not run on this system: $_sv (nothing installed; try --no-hisn)"
	fi

	# ── install ──
	step "Installing into $DIR"
	[ -d "$DIR" ] || mkdir -p "$DIR" || die "could not create $DIR"
	install_file "$TMP/x/wakil" "$DIR/wakil"
	manifest_add "$DIR/wakil"
	SUMMARY="  wakil $TAG -> $DIR/wakil"
	HISN_BIN=""
	case $HISN_STATE in
	install)
		install_file "$TMP/$HISN_ASSET" "$DIR/hisn"
		manifest_add "$DIR/hisn"
		HISN_BIN=$DIR/hisn
		SUMMARY="$SUMMARY
  hisn $HISN_VERSION -> $DIR/hisn ($HISN_ASSET)"
		;;
	keep)
		SUMMARY="$SUMMARY
  hisn $HISN_KEEP_VER kept at $HISN_KEEP_BIN (not touched, not run beyond --version)"
		;;
	esac
	if [ -n "$KVR_ASSET" ]; then
		if install_kvr; then
			manifest_add "$KVR_DEST"
			SUMMARY="$SUMMARY
  kvr-server $KVR_VERSION -> $KVR_DEST ($KVR_ASSET)"
			KVR_NOTE=""
		else
			KVR_NOTE="kvr-server was not installed into /usr/local/bin (a file may already exist there, or it is not writable); staging stays off"
		fi
		if [ -n "$KVR_TMP" ]; then
			run_priv rm -f "$KVR_TMP" 2>/dev/null || true
			KVR_TMP=""
		fi
	fi

	# ── report ──
	step "Checking"
	if _ver_out=$("$DIR/wakil" version </dev/null 2>&1); then
		printf '%s\n' "$_ver_out" | sed 's/^/    /'
	else
		die "the installed $DIR/wakil does not run: $_ver_out"
	fi
	[ -z "$HISN_NOTE" ] || say "$HISN_NOTE"
	if [ -n "$HISN_BIN" ]; then
		_out=$("$HISN_BIN" check </dev/null 2>&1) && _rc=0 || _rc=$?
		if [ "$_rc" -eq 0 ]; then
			say "hisn check: ok"
		else
			_fail=$(printf '%s\n' "$_out" | sed -n '/FAIL/{p;q;}')
			[ -n "$_fail" ] || _fail=$(printf '%s\n' "$_out" | sed -n '1p')
			say "hisn check failed: $_fail"
			say "(not an install error) Wakil will fall back to Docker"
		fi
	fi
	[ -z "$KVR_NOTE" ] || say "$KVR_NOTE"

	CONFIG_RESULT=""
	if [ "$OPT_NO_CONFIG" = 0 ]; then
		step "Config"
		setup_config
		[ -z "$CONFIG_RESULT" ] || say "$CONFIG_RESULT"
	fi

	step "Installed"
	printf '%s\n' "$SUMMARY"
	case ":$PATH:" in
	*":$DIR:"*) ;;
	*)
		warn "$DIR is not on your PATH. Add it for this shell with:"
		# shellcheck disable=SC2016 # $PATH is printed literally on purpose.
		printf '    export PATH="%s:$PATH"\n' "$DIR"
		;;
	esac
	printf '\nNext:  cd your-project && wakil .\n'
}

main "$@"
