#!/usr/bin/env bash
# git_stats.sh — Monthly git commit statistics
#
# Usage:
#   ./git_stats.sh [YEAR] [MONTH] [AUTHOR]
#
# Defaults to the current year/month when arguments are omitted.
# AUTHOR is an optional substring filter for --author.
#
# Examples:
#   ./git_stats.sh                   # current month
#   ./git_stats.sh 2026 3            # March 2026
#   ./git_stats.sh 2026 3 alice      # March 2026, filtered by author

set -euo pipefail

YEAR="${1:-$(date +%Y)}"
MONTH="${2:-$(date +%m)}"
AUTHOR="${3:-}"

# Zero-pad month
MONTH_PAD=$(printf "%02d" "$MONTH")

SINCE="${YEAR}-${MONTH_PAD}-01"

# Compute first day of next month and last day of target month arithmetically
_next_month_year() {
    local y=$1 m=$2
    if (( m == 12 )); then echo $(( y + 1 )); else echo "$y"; fi
}
_next_month() {
    local m=$1
    if (( m == 12 )); then echo 1; else echo $(( m + 1 )); fi
}
_days_in_month() {
    local y=$1 m=$2
    case $m in
        1|3|5|7|8|10|12) echo 31 ;;
        4|6|9|11)         echo 30 ;;
        2) if (( (y % 4 == 0 && y % 100 != 0) || y % 400 == 0 )); then echo 29; else echo 28; fi ;;
    esac
}

NEXT_YEAR=$(_next_month_year "${YEAR}" "${MONTH}")
NEXT_MONTH=$(_next_month "${MONTH}")
NEXT_MONTH_PAD=$(printf "%02d" "${NEXT_MONTH}")
UNTIL="${NEXT_YEAR}-${NEXT_MONTH_PAD}-01"

LAST_DAY_NUM=$(_days_in_month "${YEAR}" "${MONTH}")
LAST_DAY="${YEAR}-${MONTH_PAD}-$(printf "%02d" "${LAST_DAY_NUM}")"

LABEL="${YEAR}-${MONTH_PAD}"
echo "Git commit statistics for ${LABEL}"
echo "============================================="

# Collect numstat
if [ -n "${AUTHOR}" ]; then
    NUMSTAT=$(git log --since="${SINCE}" --until="${UNTIL}" \
        --author="${AUTHOR}" --pretty=tformat: --numstat 2>/dev/null || true)
else
    NUMSTAT=$(git log --since="${SINCE}" --until="${UNTIL}" \
        --pretty=tformat: --numstat 2>/dev/null || true)
fi

LINES_ADDED=0
LINES_DELETED=0
FILES_CHANGED=0

while IFS=$'\t' read -r add del _file; do
    [[ "${add}" =~ ^[0-9]+$ ]] || continue
    LINES_ADDED=$(( LINES_ADDED + add ))
    LINES_DELETED=$(( LINES_DELETED + del ))
    FILES_CHANGED=$(( FILES_CHANGED + 1 ))
done <<< "${NUMSTAT}"

# Count commits
if [ -n "${AUTHOR}" ]; then
    COMMIT_COUNT=$(git log --since="${SINCE}" --until="${UNTIL}" \
        --author="${AUTHOR}" --oneline 2>/dev/null | grep -c . || true)
else
    COMMIT_COUNT=$(git log --since="${SINCE}" --until="${UNTIL}" \
        --oneline 2>/dev/null | grep -c . || true)
fi

NET_LINES=$(( LINES_ADDED - LINES_DELETED ))

printf "Period        : %s — %s\n" "${SINCE}" "${LAST_DAY}"
if [ -n "${AUTHOR}" ]; then
    printf "Author filter : %s\n" "${AUTHOR}"
fi
printf "Commits       : %d\n" "${COMMIT_COUNT}"
printf "Files changed : %d\n" "${FILES_CHANGED}"
printf "Lines added   : %d\n" "${LINES_ADDED}"
printf "Lines deleted : %d\n" "${LINES_DELETED}"
printf "Net lines     : %d\n" "${NET_LINES}"
