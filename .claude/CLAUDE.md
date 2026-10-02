# dstream — working rules (read every session)

These are hard rules for working in this repo. They override default behavior.

**ALL THREE APPLY TO EVERY TASK — NO EXCEPTIONS.** Task being small, quick,
trivial, "just one line", or obvious is NEVER a reason to skip a rule. Follow
all three, every time, or don't start.

## 1. Always use the superpowers, ponytail, and caveman plugins
Every task, every time. Run the skill/plugin check first:
- **superpowers** — brainstorm → spec → plan → execute; use its process skills.
- **ponytail** — laziest correct solution; smallest working diff; delete over add; no speculative abstractions.
- **caveman** — terse output (prose only; code/commits/security written normally).

## 2. Never run any git command that mutates state
No `commit`, `push`, `add`, `rm`, `restore`, `checkout`, `stash` — nothing. The
user does **all** git manually. Read-only git (`status`, `diff`, `log`) is fine.
When work is ready, hand over commit messages/commands for the user to run.

## 3. Plan and research before implementing
Understand the problem first — read the code the change touches, trace the real
flow end to end, brainstorm/design, and confirm the plan. No coding before the
approach is clear. Research > guess.
