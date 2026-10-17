# baseline skill set

A skill set is a folder of Claude Code skills (`<skill>/SKILL.md`). Name it
in a routing entry (`skills: baseline`) and the daemon mounts this folder
read-only as the worker's `~/.claude/skills` (and `~/.codex/skills`).

This set is intentionally empty: it is the control for comparisons.
