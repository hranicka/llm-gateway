# Delegation

When a task is split into work items and handed to subagents:

- Route by risk, not size: `scout` (Haiku, read-only) for searching and research; `implementer` (Haiku) for well-specified, low-risk items following existing patterns; `implementer-pro` (Sonnet, high effort, Opus advisor) for new patterns, transactions/concurrency, migrations, auth/security, daemons, deployment, or anything the plan leaves open. Default to `implementer`; use `implementer-pro` when a wrong call would be costly.
- Give each subagent a self-contained prompt: goal, relevant files, constraints, how to verify, commit expectations.
- Run implementers sequentially when they share a checkout, a test database or other mutable state; parallelize only with isolated worktrees and isolated state.
- Review every hand-back before the next dispatch: check the commits exist (`git log`, `git show --stat`), the tree is clean, and read the risky hunks. Follow up on flagged risks before moving on.

# Advisor

The advisor is Opus. It re-reads the full conversation on every call, so use it only at these checkpoints:

- Before locking a multi-file plan, especially one touching auth, schema/migrations or public contracts.
- When the same test or compiler error fails twice: stop and ask whether you are fixing the root cause.
- Before declaring a task done or committing: review the full diff for regressions and broken repo rules.

Skip it for tasks of a few steps, single-file edits, reads and routine bash. Before a done-check in a long session, `/compact` first. If the main session runs on Opus, do not call it; review hand-backs yourself.

Subagents: `implementer` (Haiku) consults it when the task is ambiguous and before declaring done. `implementer-pro` (Sonnet) consults it before committing to an approach and before declaring done. `scout` never does.
