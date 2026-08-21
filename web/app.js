// wakild web console — app logic (card #148 P3)
// Vanilla JS, no framework. Speaks the Connect API via HTTP/JSON (fetch POST).
// Connect-Go uses protojson: all field names are camelCase in JSON.

(function () {
  'use strict';

  // --- Connect RPC helper -------------------------------------------------
  // Connect-Go serves HTTP/JSON at /<package>.<Service>/<Method>
  var API_BASE = '/wakil.v1alpha1';

  function rpc(service, method, body) {
    var url = API_BASE + '.' + service + '/' + method;
    return fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {}),
    }).then(function (resp) {
      if (!resp.ok) {
        return resp.text().then(function (text) {
          throw new Error('RPC ' + service + '/' + method + ' failed: ' + resp.status + ' ' + text);
        });
      }
      return resp.json();
    });
  }

  // --- State --------------------------------------------------------------
  var currentView = 'sessions';
  var pollTimer = null;
  var pollSessionId = null;
  var pollAfterSeq = 0;

  // --- Init ---------------------------------------------------------------
  document.addEventListener('DOMContentLoaded', function () {
    // Tab switching
    document.querySelectorAll('.tab').forEach(function (tab) {
      tab.addEventListener('click', function () {
        switchView(tab.dataset.view);
      });
    });

    // Viewer controls
    document.getElementById('viewer-connect').addEventListener('click', toggleViewer);

    // Load server info + sessions
    loadServerInfo();
    loadSessions();
  });

  function switchView(view) {
    currentView = view;
    document.querySelectorAll('.tab').forEach(function (t) {
      t.classList.toggle('active', t.dataset.view === view);
    });
    document.querySelectorAll('.view').forEach(function (v) {
      v.classList.remove('active');
    });
    document.getElementById('view-' + view).classList.add('active');
    if (view !== 'viewer') {
      stopPolling();
    }
  }

  // --- Server Info --------------------------------------------------------
  function loadServerInfo() {
    rpc('SystemService', 'GetServerInfo', {}).then(function (info) {
      var el = document.getElementById('server-info');
      el.textContent = 'API ' + info.apiVersion + ' · ' +
        (info.ephemeral ? 'ephemeral' : 'persistent') + ' · ' +
        info.authMethod + ' · caps: ' + (info.capabilities || []).join(', ');
    }).catch(function () {
      document.getElementById('server-info').textContent = 'server info unavailable';
    });
  }

  // --- Session List -------------------------------------------------------
  function loadSessions() {
    var container = document.getElementById('session-list');
    rpc('SessionService', 'ListSessions', {}).then(function (resp) {
      var sessions = resp.sessions || [];
      if (sessions.length === 0) {
        container.innerHTML = '<div class="empty-state">No sessions. Start one from the TUI (<code>wakil --daemon</code>).</div>';
        return;
      }
      container.innerHTML = '';
      sessions.forEach(function (s) {
        var card = document.createElement('div');
        card.className = 'session-card';
        card.innerHTML =
          '<div class="session-card-header">' +
          '<span class="session-state ' + s.state + '">' + s.state + '</span>' +
          '<span class="session-title">' + esc(s.title || 'untitled') + '</span>' +
          '</div>' +
          '<div class="session-id">' + esc(s.id) + '</div>' +
          '<div class="session-meta">lastSeq: ' + (s.lastSeq || 0) +
          ' · created: ' + fmtTime(s.createdAt) + '</div>';
        card.addEventListener('click', function () {
          switchView('viewer');
          document.getElementById('viewer-session-id').value = s.id;
          startPolling(s.id);
        });
        container.appendChild(card);
      });
    }).catch(function (e) {
      container.innerHTML = '<div class="empty-state">Failed to load sessions: ' + esc(e.message) + '</div>';
    });
  }

  // --- Live Viewer (polling) ----------------------------------------------
  function toggleViewer() {
    if (pollTimer) {
      stopPolling();
      return;
    }
    var sid = document.getElementById('viewer-session-id').value.trim();
    if (!sid) return;
    startPolling(sid);
  }

  function startPolling(sessionId) {
    stopPolling();
    pollSessionId = sessionId;
    pollAfterSeq = 0;
    document.getElementById('event-timeline').innerHTML = '';
    document.getElementById('viewer-connect').textContent = 'Disconnect';
    document.getElementById('viewer-connect').classList.add('connected');
    setStatus('connecting...');
    // Fetch snapshot first (gives us session metadata + full history)
    fetchSnapshot(sessionId).then(function () {
      setStatus('live · polling every 500ms');
      pollTimer = setInterval(function () { pollEvents(sessionId); }, 500);
    }).catch(function (e) {
      setStatus('error: ' + e.message);
      document.getElementById('viewer-connect').textContent = 'Connect';
      document.getElementById('viewer-connect').classList.remove('connected');
    });
  }

  function stopPolling() {
    if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
    pollSessionId = null;
    setStatus('');
    var btn = document.getElementById('viewer-connect');
    btn.textContent = 'Connect';
    btn.classList.remove('connected');
  }

  function setStatus(msg) {
    document.getElementById('viewer-status').textContent = msg;
  }

  function fetchSnapshot(sessionId) {
    return rpc('EventService', 'GetSessionSnapshot', { sessionId: sessionId }).then(function (snap) {
      if (snap.session) {
        setStatus('session: ' + (snap.session.title || 'untitled') + ' · state: ' + snap.session.state);
      }
      pollAfterSeq = snap.lastSeq || 0;
      var events = snap.events || [];
      events.forEach(function (ev) { renderEvent(ev); });
      if (events.length > 0) {
        setStatus('loaded ' + events.length + ' events · live · polling every 500ms');
      }
    });
  }

  function pollEvents(sessionId) {
    rpc('EventService', 'ListEvents', {
      sessionId: sessionId,
      afterSeq: pollAfterSeq,
      limit: 0,
    }).then(function (resp) {
      var events = resp.events || [];
      events.forEach(function (ev) {
        renderEvent(ev);
        if (ev.seq > pollAfterSeq) pollAfterSeq = ev.seq;
      });
    }).catch(function (e) {
      setStatus('poll error: ' + e.message);
    });
  }

  // --- Event Rendering ----------------------------------------------------
  // The Event envelope in protojson:
  //   { tenantId, sessionId, seq, ts, kind, <oneofField>: { ...payload... } }
  // The `kind` field is the discriminator (e.g. "session_created").
  // The oneof field name is the camelCase of the proto field name.

  // Map kind → oneof field name in protojson
  var KIND_TO_ONEOF = {
    session_created: 'sessionCreated',
    session_closed: 'sessionClosed',
    session_error: 'sessionError',
    turn_started: 'turnStarted',
    turn_completed: 'turnCompleted',
    message_delta: 'messageDelta',
    message_committed: 'messageCommitted',
    reasoning_delta: 'reasoningDelta',
    user_message_committed: 'userMessageCommitted',
    conversation_compacted: 'conversationCompacted',
    tool_call_started: 'toolCallStarted',
    tool_call_completed: 'toolCallCompleted',
    approval_requested: 'approvalRequested',
    approval_resolved: 'approvalResolved',
    subagent_spawned: 'subagentSpawned',
    subagent_progress: 'subagentProgress',
    subagent_completed: 'subagentCompleted',
    memory_proposed: 'memoryProposed',
    guard_triggered: 'guardTriggered',
    context_warning: 'contextWarning',
    workflow_turn_started: 'workflowTurnStarted',
    workflow_final_review: 'workflowFinalReview',
    workflow_outcome: 'workflowOutcome',
    workflow_warning: 'workflowWarning',
    async_job_started: 'asyncJobStarted',
    async_job_completed: 'asyncJobCompleted',
    async_job_progress: 'asyncJobProgress',
    side_question_completed: 'sideQuestionCompleted',
    side_question_progress: 'sideQuestionProgress',
    tok_rate: 'tokRate',
    learn_nudge: 'learnNudge',
    session_note: 'sessionNote',
  };

  function getPayload(ev) {
    var oneofField = KIND_TO_ONEOF[ev.kind];
    if (!oneofField) return {};
    return ev[oneofField] || {};
  }

  function renderEvent(ev) {
    var timeline = document.getElementById('event-timeline');
    var div = document.createElement('div');
    div.className = 'event ' + eventClass(ev.kind);
    div.innerHTML =
      '<span class="seq-label">#' + (ev.seq || 0) + '</span>' +
      '<div class="event-label">' + ev.kind.replace(/_/g, ' ') + '</div>' +
      '<div class="event-body">' + eventBody(ev) + '</div>';
    timeline.appendChild(div);
    // Auto-scroll to bottom
    window.scrollTo(0, document.body.scrollHeight);
  }

  function eventClass(kind) {
    var map = {
      session_created: 'session',
      session_closed: 'session',
      session_error: 'error',
      turn_started: 'turn',
      turn_completed: 'turn',
      message_delta: 'message-delta',
      message_committed: 'message',
      reasoning_delta: 'reasoning',
      user_message_committed: 'message',
      conversation_compacted: 'info',
      tool_call_started: 'tool-call',
      tool_call_completed: 'tool-call-completed',
      approval_requested: 'approval',
      approval_resolved: 'approval',
      subagent_spawned: 'subagent',
      subagent_progress: 'subagent',
      subagent_completed: 'subagent-completed',
      memory_proposed: 'info',
      guard_triggered: 'error',
      context_warning: 'info',
      workflow_turn_started: 'workflow',
      workflow_final_review: 'workflow',
      workflow_outcome: 'workflow',
      workflow_warning: 'workflow',
      async_job_started: 'async-job',
      async_job_completed: 'async-job',
      async_job_progress: 'async-job',
      side_question_completed: 'info',
      side_question_progress: 'info',
      tok_rate: 'info',
      learn_nudge: 'info',
      session_note: 'info',
    };
    return map[kind] || 'info';
  }

  function eventBody(ev) {
    var p = getPayload(ev);
    switch (ev.kind) {
      case 'message_committed':
        return esc(p.text || '');
      case 'message_delta':
        return esc(p.text || '');
      case 'reasoning_delta':
        return esc(p.text || '');
      case 'user_message_committed':
        return esc(p.text || '');
      case 'tool_call_started':
        return '<span class="tool-call-detail">tool: ' + esc(p.name || '?') +
          (p.argDigest ? ' · args: ' + esc(p.argDigest) : '') + '</span>';
      case 'tool_call_completed':
        return '<span class="tool-call-detail">tool: ' + esc(p.name || '?') +
          ' · status: ' + esc(p.status || '?') +
          (p.durationMs ? ' · ' + esc(p.durationMs) + 'ms' : '') + '</span>' +
          (p.resultPreview ? '<br>' + esc(p.resultPreview) : '');
      case 'approval_requested':
        return 'approval ' + esc(p.approvalId || '?') + ': ' + esc(p.headline || '') +
          (p.toolName ? ' (tool: ' + esc(p.toolName) + ')' : '') +
          (p.detail ? '<br>' + esc(p.detail) : '');
      case 'approval_resolved':
        return 'approval ' + esc(p.approvalId || '?') + ' → ' + esc(p.outcome || '?') +
          (p.reason ? ' — ' + esc(p.reason) : '') +
          (p.resolver ? ' by ' + esc(p.resolver) : '');
      case 'subagent_spawned':
        return 'subagent: ' + esc(p.task || '?') +
          (p.subagentId ? ' (' + esc(p.subagentId) + ')' : '') +
          (p.capability ? ' · ' + esc(p.capability) : '') +
          (p.backend ? ' · ' + esc(p.backend) : '') +
          (p.model ? ' · ' + esc(p.model) : '');
      case 'subagent_progress':
        return esc(p.text || 'progress...') +
          (p.finished ? ' · ' + esc(p.finishedStatus || 'done') : '');
      case 'subagent_completed':
        return 'subagent done: ' + esc(p.status || '?') +
          (p.summaryPreview ? ' — ' + esc(p.summaryPreview) : '') +
          (p.err ? ' · err: ' + esc(p.err) : '') +
          (p.costUsd ? ' · $' + esc(p.costUsd) : '') +
          (p.filesChanged && p.filesChanged.length ? ' · files: ' + p.filesChanged.map(esc).join(', ') : '');
      case 'turn_started':
        return 'turn ' + esc(p.turnId || '?') + (p.turnIndex ? ' #' + esc(p.turnIndex) : '');
      case 'turn_completed':
        return 'turn ' + esc(p.turnId || '?') + ' · ' + esc(p.outcome || '?') +
          (p.warn ? ' · warn: ' + esc(p.warn) : '');
      case 'session_created':
        return 'session ' + esc(ev.sessionId || '?') +
          ' · workspace: ' + esc(p.workspaceId || '?') +
          (p.agentName ? ' · agent: ' + esc(p.agentName) : '');
      case 'session_closed':
        return 'session closed' + (p.reason ? ' — ' + esc(p.reason) : '');
      case 'session_error':
        return esc(p.reason || '?') + (p.err ? ': ' + esc(p.err) : '');
      case 'context_warning':
        return esc(p.message || 'context limit approaching');
      case 'workflow_turn_started':
        return esc(p.userText || '');
      case 'workflow_final_review':
        return 'final review';
      case 'workflow_outcome':
        return esc(p.outcome || '?') + (p.reason ? ' — ' + esc(p.reason) : '');
      case 'workflow_warning':
        return esc(p.message || '');
      case 'async_job_started':
        return 'job: ' + esc(p.label || p.opId || '?');
      case 'async_job_completed':
        return 'job ' + esc(p.opId || '?') + ' done: ' + esc(p.status || '?') +
          (p.summaryPreview ? ' — ' + esc(p.summaryPreview) : '') +
          (p.err ? ' · ' + esc(p.err) : '');
      case 'async_job_progress':
        return esc(p.text || '');
      case 'tok_rate':
        return esc(p.rate || '?') + ' tok/s';
      case 'session_note':
        return esc(p.text || '');
      case 'learn_nudge':
        return esc(p.text || '');
      case 'conversation_compacted':
        return 'conversation compacted';
      case 'memory_proposed':
        return 'key: ' + esc(p.key || '?') + ' · kind: ' + esc(p.kind || '?') +
          (p.writer ? ' · by ' + esc(p.writer) : '');
      case 'guard_triggered':
        return esc(p.guard || '?') + ': ' + esc(p.message || '');
      default:
        return esc(JSON.stringify(p, null, 2));
    }
  }

  // --- Utils --------------------------------------------------------------
  function esc(s) {
    if (s == null) return '';
    var d = document.createElement('div');
    d.textContent = String(s);
    return d.innerHTML;
  }

  function fmtTime(ts) {
    if (!ts) return '?';
    // protojson encodes google.protobuf.Timestamp as an ISO 8601 string
    try {
      var d = new Date(ts);
      return d.toLocaleString();
    } catch (e) {
      return esc(String(ts));
    }
  }
})();
