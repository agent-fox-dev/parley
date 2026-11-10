[Parley PRD](00_overview.md) · Appendix B

## Appendix B: CLI command index

Generated from the command headings of §3–§14: 186 commands. Flags, calls, output and exit codes are in the section named.

| Command | Section |
|---|---|
| `parley login` | [§3](03_platform.md) |
| `parley user show` | [§3](03_platform.md) |
| `parley user update` | [§3](03_platform.md) |
| `parley keys list` | [§3](03_platform.md) |
| `parley keys refresh` | [§3](03_platform.md) |
| `parley keys revoke` | [§3](03_platform.md) |
| `parley tokens list` | [§3](03_platform.md) |
| `parley tokens create` | [§3](03_platform.md) |
| `parley tokens show <token_id>` | [§3](03_platform.md) |
| `parley tokens revoke <token_id>` | [§3](03_platform.md) |
| `parley tokens replace <token_id>` | [§3](03_platform.md) |
| `parley tokens add <token_id>` | [§3](03_platform.md) |
| `parley tokens remove <token_id>` | [§3](03_platform.md) |
| `parley orgs list` | [§3](03_platform.md) |
| `parley orgs show <org>` | [§3](03_platform.md) |
| `parley orgs members <org>` | [§3](03_platform.md) |
| `parley admin users list` | [§3](03_platform.md) |
| `parley admin users show <user>` | [§3](03_platform.md) |
| `parley admin users create` | [§3](03_platform.md) |
| `parley admin users update <user>` | [§3](03_platform.md) |
| `parley admin users delete <user>` | [§3](03_platform.md) |
| `parley admin users promote <user>` | [§3](03_platform.md) |
| `parley admin users demote <user>` | [§3](03_platform.md) |
| `parley admin users block <user>` | [§3](03_platform.md) |
| `parley admin users unblock <user>` | [§3](03_platform.md) |
| `parley admin orgs list` | [§3](03_platform.md) |
| `parley admin orgs create` | [§3](03_platform.md) |
| `parley admin orgs update <org>` | [§3](03_platform.md) |
| `parley admin orgs delete <org>` | [§3](03_platform.md) |
| `parley admin orgs block <org>` | [§3](03_platform.md) |
| `parley admin orgs unblock <org>` | [§3](03_platform.md) |
| `parley admin orgs members list <org>` | [§3](03_platform.md) |
| `parley admin orgs members add <org> <user>` | [§3](03_platform.md) |
| `parley admin orgs members remove <org> <user>` | [§3](03_platform.md) |
| `parley admin keys list <user>` | [§3](03_platform.md) |
| `parley admin keys revoke <user> <key_id>` | [§3](03_platform.md) |
| `parley admin tokens list <user>` | [§3](03_platform.md) |
| `parley admin tokens revoke <user> <token_id>` | [§3](03_platform.md) |
| `parley version` | [§3](03_platform.md) |
| `parley help [command]` | [§3](03_platform.md) |
| `parley workspace create` | [§4](04_workspaces_and_forks.md) |
| `parley workspace list` | [§4](04_workspaces_and_forks.md) |
| `parley workspace get <slug>` | [§4](04_workspaces_and_forks.md) |
| `parley workspace update <slug>` | [§4](04_workspaces_and_forks.md) |
| `parley workspace archive <slug>` | [§4](04_workspaces_and_forks.md) |
| `parley workspace reactivate <slug>` | [§4](04_workspaces_and_forks.md) |
| `parley workspace sync <slug>` | [§4](04_workspaces_and_forks.md) |
| `parley workspace reclone <slug>` | [§4](04_workspaces_and_forks.md) |
| `parley workspace push <slug> <branch>` | [§4](04_workspaces_and_forks.md) |
| `parley workspace delete <slug>` | [§4](04_workspaces_and_forks.md) |
| `parley fork create <parent>` | [§4](04_workspaces_and_forks.md) |
| `parley fork list <parent>` | [§4](04_workspaces_and_forks.md) |
| `parley fork extend <slug>` | [§4](04_workspaces_and_forks.md) |
| `parley fork delete <slug>` | [§4](04_workspaces_and_forks.md) |
| `parley credential-helper <get|store|erase>` | [§5](05_git_server.md) |
| `parley scm refs <slug>` | [§6](06_source_control_api.md) |
| `parley scm log <slug>` | [§6](06_source_control_api.md) |
| `parley scm show <slug> <rev>` | [§6](06_source_control_api.md) |
| `parley scm ls <slug> [path]` | [§6](06_source_control_api.md) |
| `parley scm cat <slug> <path>` | [§6](06_source_control_api.md) |
| `parley scm diff <slug> <base> <head>` | [§6](06_source_control_api.md) |
| `parley scm compare <slug> <base> <head>` | [§6](06_source_control_api.md) |
| `parley scm blame <slug> <path>` | [§6](06_source_control_api.md) |
| `parley scm search <slug> <query>` | [§6](06_source_control_api.md) |
| `parley scm commit <slug>` | [§6](06_source_control_api.md) |
| `parley scm rebase <slug> <branch>` | [§6](06_source_control_api.md) |
| `parley change open <slug>` | [§7](07_changes_and_stacks.md) |
| `parley change list <slug>` | [§7](07_changes_and_stacks.md) |
| `parley change show <slug> <number>` | [§7](07_changes_and_stacks.md) |
| `parley change find <slug> <sha>` | [§7](07_changes_and_stacks.md) |
| `parley change edit <slug> <number>` | [§7](07_changes_and_stacks.md) |
| `parley change ready <slug> <number>` | [§7](07_changes_and_stacks.md) |
| `parley change draft <slug> <number>` | [§7](07_changes_and_stacks.md) |
| `parley change abandon <slug> <number>` | [§7](07_changes_and_stacks.md) |
| `parley change restore <slug> <number>` | [§7](07_changes_and_stacks.md) |
| `parley change restack <slug> <number>` | [§7](07_changes_and_stacks.md) |
| `parley change versions <slug> <number>` | [§7](07_changes_and_stacks.md) |
| `parley change diff <slug> <number>` | [§7](07_changes_and_stacks.md) |
| `parley change stack <slug> <number>` | [§7](07_changes_and_stacks.md) |
| `parley change wait <slug> <number>` | [§7](07_changes_and_stacks.md) |
| `parley change land <slug> <number>` | [§8](08_landing_and_policy.md) |
| `parley change lands <slug> <number>` | [§8](08_landing_and_policy.md) |
| `parley land list <slug>` | [§8](08_landing_and_policy.md) |
| `parley land show <slug> <land-id>` | [§8](08_landing_and_policy.md) |
| `parley change conflict <slug> <number>` | [§8](08_landing_and_policy.md) |
| `parley change resolve <slug> <number>` | [§8](08_landing_and_policy.md) |
| `parley change approve <slug> <number>` | [§8](08_landing_and_policy.md) |
| `parley change reject <slug> <number>` | [§8](08_landing_and_policy.md) |
| `parley change approvals <slug> <number>` | [§8](08_landing_and_policy.md) |
| `parley change check <slug> <number> <name>` | [§8](08_landing_and_policy.md) |
| `parley change checks <slug> <number>` | [§8](08_landing_and_policy.md) |
| `parley change revert <slug> <number>` | [§8](08_landing_and_policy.md) |
| `parley policy get <slug>` | [§8](08_landing_and_policy.md) |
| `parley policy set <slug>` | [§8](08_landing_and_policy.md) |
| `parley policy evaluate <slug>` | [§8](08_landing_and_policy.md) |
| `parley activity <slug>` | [§9](09_awareness.md) |
| `parley change overlaps <slug> <number>` | [§9](09_awareness.md) |
| `parley claim create <slug>` | [§9](09_awareness.md) |
| `parley claim list <slug>` | [§9](09_awareness.md) |
| `parley claim renew <slug> <id>` | [§9](09_awareness.md) |
| `parley claim release <slug> <id>` | [§9](09_awareness.md) |
| `parley patch add <workspace-slug>` | [§10](10_carry_patch.md) |
| `parley patch list <workspace-slug>` | [§10](10_carry_patch.md) |
| `parley patch show <workspace-slug> <patch-id>` | [§10](10_carry_patch.md) |
| `parley patch update <workspace-slug> <patch-id>` | [§10](10_carry_patch.md) |
| `parley patch remove <workspace-slug> <patch-id>` | [§10](10_carry_patch.md) |
| `parley patch restore <workspace-slug> <patch-id>` | [§10](10_carry_patch.md) |
| `parley patch reorder <workspace-slug> <patch-id>...` | [§10](10_carry_patch.md) |
| `parley patch reset-to-origin <workspace-slug> <patch-id>` | [§10](10_carry_patch.md) |
| `parley rebuild submit <workspace-slug>` | [§10](10_carry_patch.md) |
| `parley rebuild list <workspace-slug>` | [§10](10_carry_patch.md) |
| `parley rebuild status <workspace-slug> <rebuild-id>` | [§10](10_carry_patch.md) |
| `parley rebuild preview <workspace-slug>` | [§10](10_carry_patch.md) |
| `parley rebuild cancel <workspace-slug> <rebuild-id>` | [§10](10_carry_patch.md) |
| `parley rebuild requeue <workspace-slug> <rebuild-id>` | [§10](10_carry_patch.md) |
| `parley rebuild rollback <workspace-slug> <rebuild-id>` | [§10](10_carry_patch.md) |
| `parley rerere list <workspace-slug>` | [§10](10_carry_patch.md) |
| `parley rerere forget <workspace-slug> <id-or-path>` | [§10](10_carry_patch.md) |
| `parley workspace patch-status <workspace-slug>` | [§10](10_carry_patch.md) |
| `parley secrets create <KEY=VALUE[,KEY=VALUE…]>… | <KEY> --from-stdin` | [§11](11_secrets_and_variables.md) |
| `parley secrets list` | [§11](11_secrets_and_variables.md) |
| `parley secrets update <KEY=VALUE> | <KEY> --from-stdin` | [§11](11_secrets_and_variables.md) |
| `parley secrets delete <KEY>` | [§11](11_secrets_and_variables.md) |
| `parley vars create <KEY=VALUE[,KEY=VALUE…]>… | <KEY> --from-stdin` | [§11](11_secrets_and_variables.md) |
| `parley vars list` | [§11](11_secrets_and_variables.md) |
| `parley vars update <KEY=VALUE> | <KEY> --from-stdin` | [§11](11_secrets_and_variables.md) |
| `parley vars delete <KEY>` | [§11](11_secrets_and_variables.md) |
| `parley vars resolve <workspace-slug>` | [§11](11_secrets_and_variables.md) |
| `parley forge status <workspace-slug>` | [§12](12_forge.md) |
| `parley forge repository <workspace-slug>` | [§12](12_forge.md) |
| `parley forge webhook set <workspace-slug>` | [§12](12_forge.md) |
| `parley forge webhook unset <workspace-slug>` | [§12](12_forge.md) |
| `parley forge deliveries <workspace-slug>` | [§12](12_forge.md) |
| `parley issue list <workspace-slug>` | [§12](12_forge.md) |
| `parley issue get <workspace-slug> <number>` | [§12](12_forge.md) |
| `parley issue comments <workspace-slug> <number>` | [§12](12_forge.md) |
| `parley issue create <workspace-slug>` | [§12](12_forge.md) |
| `parley issue update <workspace-slug> <number>` | [§12](12_forge.md) |
| `parley issue close <workspace-slug> <number>` | [§12](12_forge.md) |
| `parley issue comment <workspace-slug> <number>` | [§12](12_forge.md) |
| `parley issue label <workspace-slug> <number> <label>...` | [§12](12_forge.md) |
| `parley issue unlabel <workspace-slug> <number> <label>` | [§12](12_forge.md) |
| `parley label create <workspace-slug> <name>` | [§12](12_forge.md) |
| `parley pr get <workspace-slug> <number>` | [§12](12_forge.md) |
| `parley pr state <workspace-slug> <number>` | [§12](12_forge.md) |
| `parley pr files <workspace-slug> <number>` | [§12](12_forge.md) |
| `parley pr checks <workspace-slug> <number>` | [§12](12_forge.md) |
| `parley pr reviews <workspace-slug> <number>` | [§12](12_forge.md) |
| `parley pr create <workspace-slug>` | [§12](12_forge.md) |
| `parley pr comment <workspace-slug> <number>` | [§12](12_forge.md) |
| `parley sandbox profiles` | [§13](13_sandboxes.md) |
| `parley sandbox create <workspace-slug> [-- command...]` | [§13](13_sandboxes.md) |
| `parley sandbox list <workspace-slug>` | [§13](13_sandboxes.md) |
| `parley sandbox get <workspace-slug> <id>` | [§13](13_sandboxes.md) |
| `parley sandbox exec <workspace-slug> <id> -- <command...>` | [§13](13_sandboxes.md) |
| `parley sandbox shell <workspace-slug> <id>` | [§13](13_sandboxes.md) |
| `parley sandbox logs <workspace-slug> <id>` | [§13](13_sandboxes.md) |
| `parley sandbox upload <workspace-slug> <id> <local-path> <sandbox-path>` | [§13](13_sandboxes.md) |
| `parley sandbox download <workspace-slug> <id> <sandbox-path> <local-path>` | [§13](13_sandboxes.md) |
| `parley sandbox checkpoint <workspace-slug> <id>` | [§13](13_sandboxes.md) |
| `parley sandbox stop <workspace-slug> <id>` | [§13](13_sandboxes.md) |
| `parley sandbox start <workspace-slug> <id>` | [§13](13_sandboxes.md) |
| `parley sandbox extend <workspace-slug> <id>` | [§13](13_sandboxes.md) |
| `parley sandbox delete <workspace-slug> <id>` | [§13](13_sandboxes.md) |
| `parley sandbox proposals <workspace-slug> <id>` | [§13](13_sandboxes.md) |
| `parley sandbox approve <workspace-slug> <id> <proposal-id>` | [§13](13_sandboxes.md) |
| `parley sandbox reject <workspace-slug> <id> <proposal-id>` | [§13](13_sandboxes.md) |
| `parley sandbox rules <workspace-slug>` | [§13](13_sandboxes.md) |
| `parley sandbox rule-delete <workspace-slug> <rule-id>` | [§13](13_sandboxes.md) |
| `parley session start <workspace-slug>` | [§14](14_sessions_audit_events.md) |
| `parley session usage <session-id>` | [§14](14_sessions_audit_events.md) |
| `parley session complete <session-id>` | [§14](14_sessions_audit_events.md) |
| `parley session get <session-id>` | [§14](14_sessions_audit_events.md) |
| `parley session list` | [§14](14_sessions_audit_events.md) |
| `parley audit query` | [§14](14_sessions_audit_events.md) |
| `parley events tail` | [§14](14_sessions_audit_events.md) |
| `parley events list` | [§14](14_sessions_audit_events.md) |
| `parley webhook create <workspace-slug>` | [§14](14_sessions_audit_events.md) |
| `parley webhook list <workspace-slug>` | [§14](14_sessions_audit_events.md) |
| `parley webhook get <workspace-slug> <webhook-id>` | [§14](14_sessions_audit_events.md) |
| `parley webhook update <workspace-slug> <webhook-id>` | [§14](14_sessions_audit_events.md) |
| `parley webhook delete <workspace-slug> <webhook-id>` | [§14](14_sessions_audit_events.md) |
| `parley webhook rotate-secret <workspace-slug> <webhook-id>` | [§14](14_sessions_audit_events.md) |
| `parley webhook ping <workspace-slug> <webhook-id>` | [§14](14_sessions_audit_events.md) |
| `parley webhook deliveries <workspace-slug> <webhook-id>` | [§14](14_sessions_audit_events.md) |
| `parley webhook redeliver <workspace-slug> <webhook-id> <delivery-id>` | [§14](14_sessions_audit_events.md) |
