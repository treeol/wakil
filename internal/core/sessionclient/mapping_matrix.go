// Command/message mapping matrix for the agent-free facade (card #148, 7b3 m1).
//
// This file is documentation-only (all comments, no executable code). It maps
// every agent.Msg type the TUI currently handles to its agent-free replacement.
// The actual projection implementation lives in internal/wiring (7b3 m2).

package sessionclient

// COMMAND/MESSAGE MAPPING MATRIX
//
// This matrix maps every agent.Msg type the TUI currently handles (in
// tui_agent_msgs.go's handleAgentMsg switch) to its replacement in the
// agent-free architecture: either an event.Kind the TUI consumes via the
// event pump, or a ClientSnapshot field the TUI reads after re-fetching
// the snapshot.
//
// Legend:
//   EVT  → event.Kind (consumed via EventSubscription)
//   SNAP → ClientSnapshot field (read after snapshot re-fetch)
//   CR   → CommandResult field (produced by DispatchCommand)
//   ---  → no mapping needed (dropped or handled internally by the host)
//
// ┌────────────────────────────┬─────────────────────────────────────┬──────────────┐
// │ agent.Msg type             │ Replacement                          │ Category     │
// ├────────────────────────────┼─────────────────────────────────────┼──────────────┤
// │ StreamChunkMsg             │ KindMessageDelta                     │ EVT          │
// │ ReasoningChunkMsg          │ KindReasoningDelta                   │ EVT          │
// │ TokRateMsg                 │ KindTokRate                          │ EVT          │
// │ ConfirmReqMsg              │ KindApprovalRequested + ApprovalRequest │ EVT       │
// │ ToolStartMsg               │ KindToolCallStarted                  │ EVT          │
// │ ToolResultMsg              │ KindToolCallCompleted                │ EVT          │
// │ AgentDoneMsg               │ KindTurnCompleted                    │ EVT          │
// │ CompactedMsg               │ KindConversationCompacted            │ EVT          │
// │ SubagentStartMsg           │ KindSubagentSpawned                  │ EVT          │
// │ SubagentActiveMsg          │ KindSubagentSpawned (status update)  │ EVT          │
// │ SubagentChunkMsg           │ KindSubagentProgress                 │ EVT          │
// │ SubagentFinishedMsg        │ KindSubagentCompleted (partial)      │ EVT          │
// │ SubagentDoneMsg            │ KindSubagentCompleted                │ EVT          │
// │ AsyncJobStartMsg           │ KindAsyncJobStarted                 │ EVT          │
// │ AsyncJobChunkMsg           │ KindAsyncJobProgress                │ EVT          │
// │ AsyncJobDoneMsg            │ KindAsyncJobCompleted                │ EVT          │
// │ SideQuestionChunkMsg       │ KindSideQuestionProgress             │ EVT          │
// │ SideQuestionDoneMsg        │ KindSideQuestionCompleted            │ EVT          │
// │ SysNoteMsg                 │ Notice field on CommandResult        │ CR           │
// │ NewConvMsg                 │ Rotate *RotateRequest{Type:"new"}    │ CR           │
// │ HandoffMsg                 │ Rotate *RotateRequest{Type:"handoff"}│ CR           │
// │ OpenResumePickerMsg        │ Rotate *RotateRequest{Type:"resume"} │ CR           │
// │ LearnTurnMsg               │ Submit field on CommandResult        │ CR           │
// │ RememberTurnMsg            │ Submit or OpID (async)               │ CR           │
// │ RecallTurnMsg              │ Submit or OpID (async)                │ CR           │
// │ WFFinalReviewMsg           │ KindWorkflowFinalReview              │ EVT          │
// │ WFStartTurnMsg             │ KindWorkflowTurnStarted              │ EVT          │
// │ BackendCtxLimitMsg         │ ContextLimit field on ClientSnapshot │ SNAP         │
// │ ModelListUpdatedMsg        │ ModelList field on ClientSnapshot    │ SNAP         │
// │ MCPReconnectedMsg         │ Notice field on CommandResult        │ CR           │
// │ BatchMsg                   │ --- (internal to agent; no event)    │ ---          │
// │ ClipboardImageRequest      │ --- (TUI-internal; no event)         │ ---          │
// └────────────────────────────┴─────────────────────────────────────┴──────────────┘
//
// SLASH COMMAND MAPPING
//
// ┌─────────────────┬──────────────────────────────────────────┬──────────────────┐
// │ Command          │ CommandResult                            │ Notes            │
// ├─────────────────┼──────────────────────────────────────────┼──────────────────┤
// │ /help            │ Handled=true, Notice=text                │ display help     │
// │ /quit, /exit     │ Quit=true                                │ exit TUI         │
// │ /new             │ Rotate{Type:"new"}                       │ new conversation │
// │ /resume          │ Rotate{Type:"resume"}                    │ resume picker     │
// │ /handoff         │ Rotate{Type:"handoff"} or OpID (async)  │ fold + rotate    │
// │ /auto            │ Handled=true (mutates consent)           │ consent change   │
// │ /model           │ Handled=true (mutates snapshot)          │ model change     │
// │ /backend         │ Handled=true (mutates snapshot)          │ backend change   │
// │ /plan            │ Submit="..." or Rotate{handoff}          │ workflow         │
// │ /remember        │ OpID (async)                             │ detached job     │
// │ /recall          │ OpID (async)                              │ detached job     │
// │ /learn           │ Submit="..." (submit learn text)          │ learn turn       │
// │ /compact         │ Handled=true (triggers compaction)       │ compaction       │
// │ /info, /status   │ Handled=true, Notice=text                │ display info     │
// │ /save            │ Handled=true                             │ save session     │
// │ /subagent        │ Handled=true (mutates snapshot)           │ subagent config  │
// └─────────────────┴──────────────────────────────────────────┴──────────────────┘
