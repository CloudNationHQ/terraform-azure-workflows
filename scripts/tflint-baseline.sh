#!/usr/bin/env bash
# Run the shared tflint config (lint/.tflint.hcl) across all module repos and
# write a baseline report. Changes nothing in the module repos.
#
# Usage: scripts/tflint-baseline.sh [outdir]      (default outdir: ./baseline)
# Env:   ORG (CloudNationHQ), TOPIC (wam), TFLINT (tflint), REPOS (space-separated subset)
# Needs: gh (authenticated), jq, git, tflint.
# On Windows, use a short outdir: git clone fails when the path gets too long.
set -euo pipefail

ORG=${ORG:-CloudNationHQ}
TOPIC=${TOPIC:-wam}
TFLINT=${TFLINT:-tflint}
CONFIG="$(cd "$(dirname "$0")/../lint" && pwd)/.tflint.hcl"

outdir=${1:-./baseline}
mkdir -p "$outdir"
outdir=$(cd "$outdir" && pwd)
work="$outdir/repos"
raw="$outdir/raw"
mkdir -p "$work" "$raw"

# Materialize the list first so discovery failures are not hidden.
if [[ -n ${REPOS:-} ]]; then
  read -r -a repos <<< "$REPOS"
else
  repo_list=$(gh repo list "$ORG" --topic "$TOPIC" --limit 500 \
    --no-archived --json name --jq '.[].name' | sort)
  repos=()
  if [[ -n "$repo_list" ]]; then
    mapfile -t repos <<< "$repo_list"
  fi
fi

"$TFLINT" --init --config "$CONFIG"
version=$("$TFLINT" --version)
version=${version%%$'\n'*}
utc_date=$(date -u '+%Y-%m-%dT%H:%M:%SZ')

for repo in "${repos[@]}"; do
  dir="$work/$repo"
  json="$raw/$repo.json"
  stderr="$raw/$repo.stderr"
  status=0
  : > "$json"
  : > "$stderr"

  if [[ -d "$dir/.git" ]]; then
    git -C "$dir" pull --ff-only -q >/dev/null 2>&1 || true
  else
    gh repo clone "$ORG/$repo" "$dir" -- --depth 1 -q \
      > /dev/null 2> "$stderr" || status=$?
  fi

  # Capture failures explicitly; one repository must not stop the run.
  if (( status == 0 )); then
    if (cd "$dir" && "$TFLINT" --config "$CONFIG" --format json --force) \
      > "$json" 2> "$stderr"; then
      status=0
    else
      status=$?
    fi
  fi
  printf '%s\n' "$status" > "$raw/$repo.exit"

  # Require one result object; normalize missing/invalid output to one error.
  if ! jq -e -s '
    length == 1 and
    (.[0] | type == "object" and
      (.issues | type == "array") and (.errors | type == "array"))
  ' "$json" >/dev/null 2>&1; then
    jq -n --rawfile message "$stderr" --arg status "$status" '{
      issues: [],
      errors: [{
        severity: "error",
        message: (if $message == "" then
          "No valid TFLint JSON output (exit status \($status))."
          else $message end)
      }]
    }' > "$json.tmp"
    mv "$json.tmp" "$json"
  fi

  jq -r --arg repo "$repo" \
    '"\($repo): \(.issues | length) issues, \(.errors | length) errors"' "$json"
done

# Aggregate only this run's repositories, not stale raw files.
results=$(mktemp "$outdir/.results.XXXXXX")
trap 'rm -f "$results"' EXIT
{
  for repo in "${repos[@]}"; do
    jq --arg repo "$repo" '. + {repo: $repo}' "$raw/$repo.json"
  done
} | jq -s '.' > "$results"

jq '
  map({
    repo,
    issues: (.issues | length),
    errors: (.errors | length),
    by_rule: (
      [.issues[].rule.name] | sort | group_by(.) |
      map({key: .[0], value: length}) | from_entries
    )
  })
' "$results" > "$outdir/summary.json"

# Generate every report section from the aggregated raw results.
jq -r --arg date "$utc_date" --arg version "$version" '
  def md: tostring | gsub("\\|"; "\\|") | gsub("[\r\n]"; " ");
  def clean: (.issues | length) == 0 and (.errors | length) == 0;
  def top_rules:
    .issues | group_by(.rule.name) |
    sort_by(-length, .[0].rule.name) | .[:3] |
    map("\(.[0].rule.name) (\(length))") | join(", ");

  . as $all |
  "# TFLint baseline",
  "",
  "Date (UTC): \($date)",
  "TFLint version: \($version | md)",
  "Repositories: \($all | length)",
  "Clean repositories: \([$all[] | select(clean)] | length)",
  "",
  "## Findings per rule",
  "",
  "| Rule | Severity | Findings | Repos affected |",
  "| --- | --- | ---: | ---: |",
  ([
    $all[] | .repo as $repo | .issues[] |
    {repo: $repo, rule: .rule.name, severity: .rule.severity}
  ] | group_by(.rule) | map({
    rule: .[0].rule,
    severity: .[0].severity,
    findings: length,
    repos: (map(.repo) | unique | length)
  }) | sort_by(-.repos, .rule) | .[] |
    "| \(.rule | md) | \(.severity | md) | \(.findings) | \(.repos) |"),
  "",
  "## Findings per repo",
  "",
  "| Repo | Issues | Errors | Top 3 rules |",
  "| --- | ---: | ---: | --- |",
  ($all | sort_by(-(.issues | length), .repo) | .[] |
    select((.issues | length) > 0 or (.errors | length) > 0) |
    "| \(.repo | md) | \(.issues | length) | \(.errors | length) | \(top_rules | md) |"),
  "",
  "## Clean repos",
  "",
  ([$all[] | select(clean) | .repo] | sort | join(", ") |
    if . == "" then "None." else . end),
  "",
  "## Tool errors",
  "",
  (if ([$all[].errors[]] | length) == 0 then "None."
   else $all[] | .repo as $repo | .errors[] |
     "- \($repo | md) — \((.message // "" | split("\n")[0]) | md)"
   end)
' "$results" > "$outdir/report.md"

echo "report: $outdir/report.md"
