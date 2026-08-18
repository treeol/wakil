# `/plan` Workflow

Gather → plan → review → implement, with an optional AI counsel checkpoint
between phases.

## Phases

| Phase | Description |
|---|---|
| **Gather** | Read-only investigation — the agent surveys the codebase and writes findings. |
| **Plan** | The agent writes a numbered implementation plan. |
| **Review** | Optional AI counsel (Mashūra) reviews the plan before implementation. |
| **Present** | Plan shown to the user; waits for `/plan approve`. |
| **Implement** | The agent executes the plan step by step, logging each step. |
| **Verify** | After all steps, a final review checks whether the criteria are met. |

## Commands

```
/plan <task>         start a gather→plan→review→implement workflow for <task>
/plan --oracle=MODE  set per-run review schedule (every-step|on-deviation|phases-only)
/plan status         show current workflow phase and step
/plan approve        approve the plan; force-skip review (logged); advance past pauses
/plan review         retry the counsel plan review (when review is pending/unavailable)
/plan verify         re-run the final review (in verify state after gaps flagged)
/plan abort          cancel the active workflow
```

## Oracle review schedule

The `--oracle` flag controls when AI counsel is consulted:

| Mode | Behaviour |
|---|---|
| `every-step` | Counsel reviews every implementation step. |
| `on-deviation` | Counsel reviews when a step deviates from the plan. *(default)* |
| `phases-only` | Counsel reviews only at phase transitions (plan review + final review). |

The schedule can also be set via `mashura_mode` in the config file. See
[Configuration](configuration.md#config-only-fields).
