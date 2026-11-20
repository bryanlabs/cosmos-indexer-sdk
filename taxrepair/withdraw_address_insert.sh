#!/bin/sh
# Dry-runs the withdraw-address insert, then waits for an APPLY file whose
# content is the plan's sha256 before committing it. STOP ends after the
# dry run. Connection comes from the PG* environment; never pass a password
# as an argument.
# Usage: withdraw_address_insert.sh <plan.tsv> <work dir>
set -eu
plan=$1; work=$2
here=$(dirname "$0")
sum=$(sha256sum "$plan" | cut -d' ' -f1)
echo "plan sha256 $sum"
psql -XqtA -v ON_ERROR_STOP=1 -f "$here/withdraw_address_insert.sql" < "$plan"
n=0
until test -e "$work/APPLY"; do
  test ! -e "$work/STOP" || { echo "stopped after the dry run"; exit 0; }
  n=$((n+1)); test "$n" -le 1440 || { echo "no apply signal after the dry run"; exit 71; }
  sleep 5
done
test "$(tr -d ' \n' < "$work/APPLY")" = "$sum" || { echo "APPLY does not name this plan's sha256"; exit 72; }
echo apply
psql -XqtA -v ON_ERROR_STOP=1 -v apply=1 -f "$here/withdraw_address_insert.sql" < "$plan"
echo done
