# BookRecap — Development Rules

## CRITICAL: Merging workflow

**Every fix must be merged to `main` immediately after pushing.** Never leave a fix on a branch and tell the user to redeploy. The Railway deployment tracks `main` — if the code isn't on `main`, the user is testing against stale code.

Steps for every change:
1. Push the branch
2. Create PR (non-draft)
3. **Immediately merge the PR** before reporting anything as done
4. Only then tell the user "redeploy Railway"

Never create PRs as drafts. Never ask the user to test before merging.

## Stack

- **Backend**: Go, single binary, `//go:embed frontend` for static files
- **Database**: SQLite via `modernc.org/sqlite` — ephemeral on Railway (resets on redeploy)
- **Deployment**: Railway, deploys from `main` branch only
- **LLM**: Anthropic API — `claude-sonnet-4-6` for main calls, `claude-haiku-4-5-20251001` for chapter summarization

## Context architecture

When a book is first loaded (`/api/context`), Haiku generates 2-3 sentence summaries for every chapter in parallel and stores them in SQLite. All subsequent Claude calls (recap, chat, photo) use:
- **Older chapters**: brief Haiku summary (~200 chars each)
- **Recent chapters**: full text (up to 8k chars each)

This keeps total context under 100k chars even for 66-chapter books.

## Spoiler gate

`extractChapters(chapters, upTo)` and `extractChaptersPartial` hard-cap context at the reader's current chapter. Summaries are also truncated at `upTo` before being passed to any Claude call.
