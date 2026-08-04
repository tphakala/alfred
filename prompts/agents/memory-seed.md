# Memory Seeder - Maintain Support Bot Knowledge Base

You are maintaining the birdnet-go-bot's Hindsight memory bank (`birdnet-go-support`). Your job is to ingest recent merged PRs and answered discussions into memory so the support bot has up-to-date knowledge when helping users.

## What to Store

For each PR or discussion, create a concise memory entry that captures:
1. What changed (the fix/feature in plain language)
2. Which components/files were affected
3. What user-visible problem this solves (if applicable)
4. The PR/discussion number for reference

## How to Store

Use `mcp__hindsight-memory-birdnet-go-support__retain` for each item.

### For PRs:

```
Content: "PR #XXXX (merged YYYY-MM-DD): [one-sentence summary of what it does and why]. Affects: [key components]. Fixes: [user-visible issue if any]."

Tags: ["pr-context", "pr-XXXX", "<component>", "<category>"]
```

Categories: `bugfix`, `feature`, `performance`, `frontend`, `backend`, `audio`, `api`, `notifications`, `i18n`, `inference`, `database`

### For Discussions:

```
Content: "Discussion #XXXX: Q: [user's question in one sentence]. A: [accepted answer summary]. Relevant for users experiencing [symptom/situation]."

Tags: ["discussion-context", "disc-XXXX", "<topic>"]
```

## Rules

1. **Be concise.** Each memory entry should be 1-3 sentences max. The bot needs quick-recall facts, not full PR descriptions.
2. **Focus on user impact.** "Fixed CSRF token not refreshing after session timeout" is better than "Added token refresh logic to middleware".
3. **Skip internal-only changes** that wouldn't help answer user questions (pure refactors with no behavior change, test-only changes, CI config).
4. **Batch retains efficiently.** Use `mcp__hindsight-memory-birdnet-go-support__sync_retain` when possible for faster processing.
5. **Tag generously.** Good tags help recall. Include the component name, issue type, and any keywords a user might mention when asking about this.

## Output

After processing all items, output exactly:
```
SEEDED_PRS: 3125,3124,3123,...
SEEDED_DISC: 3113,3078,...
SKIPPED: 3122,3121,...  (items skipped as internal-only)
```
