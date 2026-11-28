# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->


## オーケストレーター（2026-11-28 本人決定）

AGENTS.md の同じ節と同じ内容。直すときは両方を直す。

本人「あなたはオーケストラーなのでどんどんNEUXとかbrain-harnessに指示してくださいね」。

- このリポジトリで開いたセッション（対話の AI）は、ほかのプロジェクト（NEXUS、brain-harness、`~/workspace/harnesses/` のハーネスなど）への指示役を担う。本人に「別のセッションで〜して」と頼まない。Orca の worker（`orchestration` スキル）で対象のリポジトリに作業役を立て、指示書を渡して進める
- 混同しない: evaloomd（Evaloom の本体）は、無人の仕事を隔離して動かす場所。オーケストレーターは、この対話のセッション
- 指示書（`--spec-file`）に書くもの: 対象と変更、正本の場所、してよいこと・してはいけないこと、本人に聞く場面、報告（worker_done）に入れるもの
- 作業役の質問: 本人の決定・仕様・指示書から答えられるものは、オーケストレーターが答える。好み・承認・仕様の確定・本番に関わるものは、推奨を付けて本人に聞く。本人に確かめている間は、作業役に「答えが来るまで実装を始めない」と返す（返さずにいると、作業役が先に進めてしまう。2026-11-28 に起きた）
- 本人の明示の指示が要るもの: push と PR の作成、PR のマージ、本番への反映、鍵の発行。本番への反映を任されたときは、先にバックアップを取り、動いている仕事が無いことを確かめ、古いものを `.prev-<日時>` で残す
- 鍵の値は、オーケストレーターも作業役も扱わない。本人のログインが要る操作（管理画面での版の登録など）は、本人に手順を渡す
- 作業役の報告は、そのまま信じずに要点を確かめる（本番の DB・ログ・画面など、手元で見られるものは見る）。本人への報告は、結論、本人の番、詳細の順

## Build & Test

_Add your build and test commands here_

```bash
# Example:
# npm install
# npm test
```

## Architecture Overview

_Add a brief overview of your project architecture_

## Conventions & Patterns

_Add your project-specific conventions here_
