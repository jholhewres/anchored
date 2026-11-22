---
description: Save a memory to persistent cross-tool storage. Pass content as argument.
argument-hint: <content>
---

Save the following to Anchored memory: $ARGUMENTS

If the argument is empty, ask the user what to save.

Then call the `anchored_save` MCP tool. You MUST pick the best category from: `fact`, `preference`, `decision`, `event`, `learning`, `plan`, `summary`. Picking explicitly beats letting the regex auto-detect. Confirm to the user which category you chose and why, plus the project it was scoped to.

## Remote Save

Leave `remote` out. When the repository's remote has `auto_sync` on, the save is mirrored to it automatically, through the safety filter.

Pass `remote` only when the user asks to send this memory to a specific server (`""` or `"default"` for the default remote, or a remote's name). It runs the same filter as auto-sync: secrets, local paths, events, preferences and anything in the personal (`user`) scope never leave the machine, and the reply says why.

The save always succeeds locally first. If the remote save fails or is blocked, the local save is preserved and the reply says so.
