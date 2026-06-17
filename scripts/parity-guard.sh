#!/usr/bin/env bash
#
# parity-guard.sh — CI vocabulary gate.
#
# Fails the build if Python/parity-world vocabulary reappears in the tree. The
# old Python repo is retired (see docs/design/python-parity-cleanup.md): this
# repo no longer treats Python/parity as a design constraint or source of truth.
# This guard is the depguard-style counterpart for prose: depguard keeps the
# layering from drifting, this keeps the retired vocabulary from creeping back.
#
# It greps the working tree (excluding planning/design notes, generated reports,
# VCS + vendored deps) for forbidden, case-insensitive, word-ish keywords and
# exits non-zero on any hit that is not on the explicit allowlist.
#
# Allowlist: scripts/parity-guard-allowlist.txt — one "<path>:<keyword>" entry
# per line (see that file's header). Aim to keep it EMPTY; an entry is a debt,
# not a feature.
#
# Usage:
#   scripts/parity-guard.sh          # gate (non-zero on any disallowed hit)
#   scripts/parity-guard.sh --list   # print every hit (debugging; never fails)

set -euo pipefail

# Resolve repo root from this script's location so it works from any cwd
# (the layering gate likewise runs relative to the module root).
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ALLOWLIST="$SCRIPT_DIR/parity-guard-allowlist.txt"

cd "$ROOT"

# Forbidden vocabulary. Case-insensitive, word boundaries (-w) so substrings
# like "pyjson"/"pytime" (legitimate Python-compatible serialisation helpers,
# not parity) do NOT trip the gate. "MUST-MATCH" and "src/strategies" carry
# their own punctuation so they are matched verbatim via a separate pattern.
WORD_KEYWORDS='python|parity|nautilus|cpython|dataclass'
# These contain non-word punctuation, so -w would not anchor them correctly;
# matched as fixed-ish substrings instead.
LITERAL_KEYWORDS='MUST-MATCH|src/strategies'

# Paths never scanned:
#   .claude/        — agent worktrees + memory (out of repo scope)
#   .git/           — VCS internals
#   node_modules/   — vendored JS deps
#   .next/          — Next.js generated build output (gitignored; not source)
#   docs/design/    — design/planning notes (this guard is defined there; the
#                     plan necessarily quotes the very words it retires)
#   e2e/report/     — generated Playwright report artifacts
#   scripts/parity-guard*  — this guard + its allowlist legitimately name the words
EXCLUDE_DIRS=(--exclude-dir=.claude --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=.next)
# Path-prefix excludes (grep -r has no glob exclude for nested dirs, so we
# filter them out of the result stream).
PATH_FILTER='^(\./)?(docs/design/|e2e/report/|scripts/parity-guard)'

collect_hits() {
	# Emit "path:lineno:line" for every match of either keyword class.
	# -I skips binary files (e.g. a built ./tms binary that incidentally embeds
	# these strings) so the gate only scans source/text.
	#
	# Two tokens are legitimate self-references, not retired vocabulary, and are
	# stripped before re-checking so a line is only reported when a forbidden
	# keyword survives independently of them:
	#   - "parity-guard"             — the gate's own name (Makefile target,
	#                                  README/docs prose, `make lint`).
	#   - "python-parity-cleanup"    — the design note that defines this cleanup
	#                                  (docs/design/python-parity-cleanup.md);
	#                                  pointers to it must name the file.
	{
		grep -rIniwE "$WORD_KEYWORDS" . "${EXCLUDE_DIRS[@]}" 2>/dev/null || true
		grep -rIniE "$LITERAL_KEYWORDS" . "${EXCLUDE_DIRS[@]}" 2>/dev/null || true
	} | grep -vE "$PATH_FILTER" \
	  | while IFS= read -r line; do
			stripped="${line//parity-guard/}"
			stripped="${stripped//python-parity-cleanup/}"
			if printf '%s' "$stripped" | grep -qiwE "$WORD_KEYWORDS" ||
				printf '%s' "$stripped" | grep -qiE "$LITERAL_KEYWORDS"; then
				printf '%s\n' "$line"
			fi
		done | sort -u
}

# Build the allowlist matcher: each non-comment, non-blank line is "<path>:<kw>".
# A hit is allowed iff its "path" prefix AND keyword both appear in an entry.
# To keep it simple and exact, we match the literal "path:lineno" not used;
# instead an allowlist entry is "<path>\t<reason>" and suppresses ALL hits in
# that path. This is intentionally coarse — an allowlisted file is a known,
# justified residual documented by its reason.
allowed_paths=()
if [[ -f "$ALLOWLIST" ]]; then
	while IFS= read -r line; do
		# strip comments / blanks
		line="${line%%#*}"
		line="$(printf '%s' "$line" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
		[[ -z "$line" ]] && continue
		# entry format: <path><whitespace><reason...>; take the first field.
		p="$(printf '%s' "$line" | awk '{print $1}')"
		[[ -n "$p" ]] && allowed_paths+=("$p")
	done < "$ALLOWLIST"
fi

is_allowed() {
	local hitpath="$1"
	local p
	for p in "${allowed_paths[@]:-}"; do
		[[ -z "$p" ]] && continue
		# normalise leading ./ on the hit path for comparison
		local norm="${hitpath#./}"
		[[ "$norm" == "$p" ]] && return 0
	done
	return 1
}

hits="$(collect_hits)"

if [[ "${1:-}" == "--list" ]]; then
	printf '%s\n' "$hits"
	exit 0
fi

violations=()
while IFS= read -r hit; do
	[[ -z "$hit" ]] && continue
	# path is the field before the first ":" (grep -n format: path:lineno:line)
	path="${hit%%:*}"
	if is_allowed "$path"; then
		continue
	fi
	violations+=("$hit")
done <<< "$hits"

if [[ ${#violations[@]} -eq 0 ]]; then
	echo "parity-guard: OK — 0 forbidden vocabulary hits."
	exit 0
fi

echo "parity-guard: FAILED — forbidden Python/parity vocabulary found." >&2
echo "  Keywords: $WORD_KEYWORDS | $LITERAL_KEYWORDS" >&2
echo "  The old Python repo is retired; do not reintroduce parity vocabulary." >&2
echo "  See docs/design/python-parity-cleanup.md. If a hit is a TRUE residual" >&2
echo "  that must stay, add its path + a reason to scripts/parity-guard-allowlist.txt." >&2
echo "" >&2
printf '%s\n' "${violations[@]}" >&2
echo "" >&2
echo "parity-guard: ${#violations[@]} disallowed hit(s)." >&2
exit 1
