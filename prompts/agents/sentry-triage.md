Review the Sentry service for new unresolved issues in the birdnet-go project (organization: tphakala, project: birdnet-go).

## Sentry access

Sentry interaction is via the `sentry` CLI in bash. The CLI reads credentials from `~/.sentryclirc`. No Sentry MCP server is available in this session; do not attempt `mcp__sentry__*` calls.

## Step 1: Fetch Sentry issues

Fetch all unresolved Sentry issues (limit 100) via the CLI:

```
sentry issue list birdnet-go/birdnet-go --query "is:unresolved" --limit 100
```

If the CLI fails, abort the run. Sentry data is required for triage; proceeding without it would produce incorrect results.

For each issue, note its Sentry short ID (e.g. BIRDNET-GO-9W, BIRDNET-GO-Z4).

## Step 2: Build the Sentry ID index from the internal tracker

Fetch ALL internal tracker issues (both open AND closed) using `mcp__forgejo-bot__list_repo_issues` (owner: "tphakala", repo: "birdnet-go"). Extract every Sentry ID mentioned in any issue body. This index is the PRIMARY deduplication mechanism.

- Fetch open issues: `mcp__forgejo-bot__list_repo_issues` with state "open", paginate all pages
- Fetch closed issues: `mcp__forgejo-bot__list_repo_issues` with state "closed", paginate all pages
- The repo has 300+ closed issues, so paginate until no more results.

Build a mapping: `{sentry_id: {issue: N, state: "open"|"closed", title: "..."}}` by scanning each issue body for patterns like `BIRDNET-GO-XXX`. Also index each issue title for secondary matching.

## Step 3: Check Hindsight memory

Use `recall` with query "sentry triage birdnet-go filed issues" to retrieve records of previously filed issues. This serves as a backup dedup source in case the tracker scan missed something.

## Step 4: Group Sentry issues by root cause

Deduplicate Sentry issues by grouping related ones (same error in different locales, same timeout from different paths). Treat each group as a single candidate.

## Step 5: Decide what to file

For each Sentry group, check ALL of the following. Skip filing if ANY match:

1. **Sentry ID match (primary)**: ANY Sentry ID in the group appears in the tracker index (open OR closed). This is the most reliable check.
2. **Title match (secondary)**: The Sentry error message substantially matches an existing issue title (open OR closed).
3. **Hindsight memory match**: Memory indicates the issue was already filed or addressed.
4. **PR match**: Search merged GitHub PRs for the error keywords:
   `gh pr list --repo tphakala/birdnet-go --state closed --search "KEYWORD" --limit 5 --json number,title,mergedAt`
5. **Environmental/config issue**: Wrong RTSP URLs, missing env vars, external service transient failures, user network issues, permission problems from misconfigured deployments.

**CRITICAL RULES:**
- NEVER file an issue if any Sentry ID from the group already appears in ANY existing issue (open or closed). No exceptions.
- NEVER file a "regression" variant of a closed issue. If a Sentry issue matches a closed issue but still has recent events, note it in the final summary as a potential regression. Do NOT create a new issue for it.
- When in doubt, do NOT file. A missed filing can be caught next cycle. A duplicate filing creates noise that must be manually cleaned up.

## Step 6: File new issues

For genuinely new bugs not tracked anywhere, file using `mcp__forgejo-bot__create_issue` (owner: "tphakala", repo: "birdnet-go"). Then add labels using `mcp__forgejo-bot__add_issue_labels`. Use `mcp__forgejo-bot__list_repo_labels` to discover current label IDs if needed.

- Title prefixed with "bug:"
- Body in the format below
- Known label IDs: bug=4, sentry=28, component/mqtt=17, component/audiocore=12, component/database=15, component/config=20, component/weather=22, component/notifications=19, component/api=16, component/frontend=13, component/security=14, component/spectrogram=21, severity/critical=9, severity/high=2, severity/medium=11, severity/low=10
- ALWAYS include sentry=28 on every issue filed by this agent.

### Required Issue Body Format

```markdown
## Summary

{description of the bug}

## Affected Versions

| Version | Events | First Seen | Last Seen |
|---------|--------|------------|-----------|
| nightly-20260322 | 45 | 2026-03-22 | 2026-03-25 |

{If version data unavailable: "Version information not available in Sentry event data."}

## Sentry Issues

| Sentry ID | Title | Events | First Seen |
|---|---|---|---|
| BIRDNET-GO-XXX | ... | ... | ... |

## Analysis

{root cause analysis}

## Related Issues

{links to related issues, if any}
```

To extract version info: check `release` tag, `server_name`/`device` context, `version`/`app.version`/`sentry:release` event tags.

## Step 7: Store results in Hindsight memory

Use `retain` to store a structured record:

```
Sentry triage run YYYY-MM-DD:
- Filed: BIRDNET-GO-XXX -> #NNN (title)
- Skipped (already tracked): BIRDNET-GO-YYY -> #MMM
- Skipped (environmental): BIRDNET-GO-ZZZ (reason)
- Potential regressions: BIRDNET-GO-AAA (matches closed #MMM, still active)
```

Use tags: ["sentry", "triage", "automated", "birdnet-go"]

## Step 8: Update the skip cache in Hindsight memory

For every Sentry ID you decided NOT to file (environmental, config, transient, already tracked by title match, or PR-fixed), record it in Hindsight memory so future runs exclude it without re-analysing. Use the same `birdnet-go-support` bank that powers step 7.

1. First, `recall` the current cache with query "sentry skip cache" and tags `["sentry-skip-cache"]` to see what's already tracked. Treat the most recent matching memory as the current state.
2. For each newly skipped Sentry ID, `retain` a new memory containing:
   - The Sentry ID (e.g. `BIRDNET-GO-XXX`).
   - A brief reason ("environmental: bad RTSP URL", "config: missing API key", "transient: upstream 502", "fixed in PR #NNNN", etc.).
   - The run date (YYYY-MM-DD).
   - An expiry date 90 days out (YYYY-MM-DD) so stale skip entries can be cleaned up.
3. Use tags `["sentry-skip-cache", "sentry", "triage", "automated", "birdnet-go"]` on each entry so future recalls find them.

One memory per skipped ID is fine; the recall in step 5 (and future runs) will aggregate them. Do NOT add IDs that were filed as issues (those are tracked via the issue tracker itself).

## Step 9: Print summary

Print:
- Sentry issues reviewed (count)
- Matched existing open issues (count + list)
- Matched closed/fixed issues (count + list)
- Skipped as environmental/config (count)
- New issues filed (count + numbers + titles)
- Potential regressions worth investigating (Sentry IDs + matching closed issues)
