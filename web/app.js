// wakild web console — app logic (card #148 P3/P5)
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
      credentials: 'same-origin', // send/receive session cookies (P5a)
      body: JSON.stringify(body || {}),
    }).then(function (resp) {
      if (!resp.ok) {
        return resp.text().then(function (text) {
          var msg = 'RPC ' + service + '/' + method + ' failed: ' + resp.status;
          try {
            var parsed = JSON.parse(text);
            if (parsed && parsed.message) msg += ' — ' + parsed.message;
          } catch (e) {
            if (text) msg += ' — ' + text.substring(0, 200);
          }
          throw new Error(msg);
        });
      }
      return resp.json();
    });
  }

  // --- State --------------------------------------------------------------
  var authenticated = false;
  var principal = null;
  var currentView = 'sessions';
  var pollTimer = null;
  var pollSessionId = null;
  var pollAfterSeq = 0;
  var viewerSession = null; // current session metadata

  // --- Init ---------------------------------------------------------------
  document.addEventListener('DOMContentLoaded', function () {
    // Login
    document.getElementById('login-btn').addEventListener('click', doLogin);
    document.getElementById('login-token').addEventListener('keydown', function (e) {
      if (e.key === 'Enter') doLogin();
    });
    document.getElementById('logout-btn').addEventListener('click', doLogout);

    // Tab switching
    document.querySelectorAll('.tab').forEach(function (tab) {
      tab.addEventListener('click', function () {
        switchView(tab.dataset.view);
      });
    });

    // Viewer controls
    document.getElementById('viewer-connect').addEventListener('click', toggleViewer);
    document.getElementById('btn-submit-input').addEventListener('click', submitInput);
    document.getElementById('btn-interrupt').addEventListener('click', interruptSession);
    document.getElementById('btn-close').addEventListener('click', closeSession);
    document.getElementById('btn-delete').addEventListener('click', deleteSession);
    document.getElementById('input-text').addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        submitInput();
      }
    });

    // New session form
    document.getElementById('new-session-btn').addEventListener('click', function () {
      document.getElementById('new-session-form').classList.remove('hidden');
      document.getElementById('new-session-workspace').focus();
    });
    document.getElementById('create-session-cancel').addEventListener('click', function () {
      document.getElementById('new-session-form').classList.add('hidden');
    });
    document.getElementById('create-session-confirm').addEventListener('click', createSession);

    // New backend form
    document.getElementById('new-backend-btn').addEventListener('click', function () {
      document.getElementById('new-backend-form').classList.remove('hidden');
      document.getElementById('new-backend-label').focus();
    });
    document.getElementById('create-backend-cancel').addEventListener('click', function () {
      document.getElementById('new-backend-form').classList.add('hidden');
    });
    document.getElementById('create-backend-confirm').addEventListener('click', createBackend);

    // New workspace form
    document.getElementById('new-workspace-btn').addEventListener('click', function () {
      document.getElementById('new-workspace-form').classList.remove('hidden');
      document.getElementById('new-workspace-name').focus();
    });
    document.getElementById('create-workspace-cancel').addEventListener('click', function () {
      document.getElementById('new-workspace-form').classList.add('hidden');
    });
    document.getElementById('create-workspace-confirm').addEventListener('click', createWorkspace);

    // New agent form
    document.getElementById('new-agent-btn').addEventListener('click', function () {
      document.getElementById('new-agent-form').classList.remove('hidden');
      document.getElementById('new-agent-name').focus();
    });
    document.getElementById('create-agent-cancel').addEventListener('click', function () {
      document.getElementById('new-agent-form').classList.add('hidden');
    });
    document.getElementById('create-agent-confirm').addEventListener('click', createAgent);

    // Start: check auth, then load
    checkAuth();
  });

  // --- Auth ---------------------------------------------------------------

  function checkAuth() {
    rpc('AuthService', 'WhoAmI', {}).then(function (resp) {
      if (resp.principal) {
        authenticated = true;
        principal = resp.principal;
        showMainApp();
      }
    }).catch(function () {
      // Unauthenticated — show login
      showLogin();
    });
  }

  function showLogin() {
    authenticated = false;
    principal = null;
    document.getElementById('view-login').classList.add('active');
    document.getElementById('view-main').classList.remove('active');
    document.getElementById('login-token').value = '';
    document.getElementById('login-error').textContent = '';
    document.getElementById('login-token').focus();
  }

  function showMainApp() {
    document.getElementById('view-login').classList.remove('active');
    document.getElementById('view-main').classList.add('active');
    // Show user info
    var ui = document.getElementById('user-info');
    if (principal) {
      ui.textContent = principal.userId + ' · ' + principal.role;
    }
    loadServerInfo();
    loadSessions();
  }

  function doLogin() {
    var token = document.getElementById('login-token').value.trim();
    var errEl = document.getElementById('login-error');
    if (!token) {
      errEl.textContent = 'Please enter a join token.';
      return;
    }
    errEl.textContent = '';
    var btn = document.getElementById('login-btn');
    btn.disabled = true;
    btn.textContent = 'Signing in…';

    rpc('AuthService', 'ExchangeJoinToken', { token: token }).then(function (resp) {
      authenticated = true;
      if (resp.principal) principal = resp.principal;
      btn.disabled = false;
      btn.textContent = 'Sign In';
      showMainApp();
    }).catch(function (e) {
      errEl.textContent = e.message;
      btn.disabled = false;
      btn.textContent = 'Sign In';
    });
  }

  function doLogout() {
    rpc('AuthService', 'Logout', {}).catch(function () {}).finally(function () {
      showLogin();
      stopPolling();
    });
  }

  function switchView(view) {
    currentView = view;
    document.querySelectorAll('.tab').forEach(function (t) {
      t.classList.toggle('active', t.dataset.view === view);
    });
    document.querySelectorAll('.view-tab').forEach(function (v) {
      v.classList.remove('active');
    });
    var el = document.getElementById('view-' + view);
    if (el) el.classList.add('active');
    if (view !== 'viewer') {
      stopPolling();
    }
    if (view === 'sessions') {
      loadSessions();
    } else if (view === 'backends') {
      loadBackends();
    } else if (view === 'workspaces') {
      loadWorkspaces();
    } else if (view === 'agents') {
      loadAgents();
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
        container.innerHTML = '<div class="empty-state">No sessions yet. Click <strong>+ New Session</strong> to start.</div>';
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

  // --- Create Session -----------------------------------------------------
  function createSession() {
    var ws = document.getElementById('new-session-workspace').value.trim();
    var title = document.getElementById('new-session-title').value.trim();
    var errEl = document.getElementById('create-session-error');
    if (!ws) {
      errEl.textContent = 'Workspace path is required.';
      return;
    }
    errEl.textContent = '';

    rpc('SessionService', 'CreateSession', {
      workspace: ws,
      title: title,
    }).then(function (session) {
      document.getElementById('new-session-form').classList.add('hidden');
      document.getElementById('new-session-workspace').value = '';
      document.getElementById('new-session-title').value = '';
      // Switch to viewer for the new session
      switchView('viewer');
      document.getElementById('viewer-session-id').value = session.id;
      startPolling(session.id);
    }).catch(function (e) {
      errEl.textContent = e.message;
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
    viewerSession = null;
    document.getElementById('event-timeline').innerHTML = '';
    document.getElementById('approval-area').innerHTML = '';
    document.getElementById('viewer-connect').textContent = 'Disconnect';
    document.getElementById('viewer-connect').classList.add('connected');
    document.getElementById('viewer-session-actions').classList.remove('hidden');
    document.getElementById('input-area').classList.remove('hidden');
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
    viewerSession = null;
    setStatus('');
    var btn = document.getElementById('viewer-connect');
    btn.textContent = 'Connect';
    btn.classList.remove('connected');
    document.getElementById('viewer-session-actions').classList.add('hidden');
    document.getElementById('input-area').classList.add('hidden');
    document.getElementById('approval-area').innerHTML = '';
  }

  function setStatus(msg) {
    document.getElementById('viewer-status').textContent = msg;
  }

  function fetchSnapshot(sessionId) {
    return rpc('EventService', 'GetSessionSnapshot', { sessionId: sessionId }).then(function (snap) {
      if (snap.session) {
        viewerSession = snap.session;
        setStatus('session: ' + (snap.session.title || 'untitled') + ' · state: ' + snap.session.state);
        updateSessionControls(snap.session);
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
        handleApprovalEvent(ev);
        if (ev.seq > pollAfterSeq) pollAfterSeq = ev.seq;
      });
      // Refresh session metadata occasionally to update state
      if (events.length > 0 && viewerSession) {
        rpc('SessionService', 'GetSession', { sessionId: sessionId }).then(function (s) {
          viewerSession = s;
          updateSessionControls(s);
        }).catch(function () {});
      }
    }).catch(function (e) {
      setStatus('poll error: ' + e.message);
    });
  }

  // --- Session write actions ---------------------------------------------

  function updateSessionControls(session) {
    if (!session) return;
    var inputArea = document.getElementById('input-area');
    var btnInterrupt = document.getElementById('btn-interrupt');
    var btnClose = document.getElementById('btn-close');
    var btnDelete = document.getElementById('btn-delete');
    var state = session.state;

    // Input is enabled for idle, error, and awaiting_approval states
    inputArea.classList.toggle('hidden', state === 'closed' || state === 'running');

    // Interrupt is only for running state
    btnInterrupt.disabled = state !== 'running';

    // Close is enabled for non-closed sessions
    btnClose.disabled = state === 'closed';

    // Delete is always enabled
    btnDelete.disabled = false;
  }

  function submitInput() {
    if (!pollSessionId) return;
    var text = document.getElementById('input-text').value.trim();
    if (!text) return;
    var readAction = document.getElementById('input-read-action').checked;

    rpc('SessionService', 'SubmitInput', {
      sessionId: pollSessionId,
      text: text,
      readAction: readAction,
    }).then(function (ack) {
      document.getElementById('input-text').value = '';
      setStatus('submitted turn ' + (ack.turnId || '?'));
    }).catch(function (e) {
      setStatus('submit error: ' + e.message);
    });
  }

  function interruptSession() {
    if (!pollSessionId) return;
    rpc('SessionService', 'Interrupt', { sessionId: pollSessionId }).then(function () {
      setStatus('interrupt sent');
    }).catch(function (e) {
      setStatus('interrupt error: ' + e.message);
    });
  }

  function closeSession() {
    if (!pollSessionId) return;
    if (!confirm('Close this session?')) return;
    rpc('SessionService', 'CloseSession', { sessionId: pollSessionId }).then(function () {
      setStatus('session closing...');
    }).catch(function (e) {
      setStatus('close error: ' + e.message);
    });
  }

  function deleteSession() {
    if (!pollSessionId) return;
    if (!confirm('Delete this session? This cannot be undone.')) return;
    rpc('SessionService', 'DeleteSession', { sessionId: pollSessionId }).then(function () {
      stopPolling();
      switchView('sessions');
    }).catch(function (e) {
      setStatus('delete error: ' + e.message);
    });
  }

  // --- Approval handling --------------------------------------------------
  // ApprovalRequested events carry approvalId, headline, toolName, detail.
  // We render buttons for allow once / allow reads once / deny.

  var pendingApprovals = {}; // approvalId → DOM element

  function handleApprovalEvent(ev) {
    if (ev.kind === 'approval_requested') {
      var p = getPayload(ev);
      renderApprovalButtons(p);
    } else if (ev.kind === 'approval_resolved') {
      var p2 = getPayload(ev);
      if (pendingApprovals[p2.approvalId]) {
        pendingApprovals[p2.approvalId].remove();
        delete pendingApprovals[p2.approvalId];
      }
    }
  }

  function renderApprovalButtons(approval) {
    var area = document.getElementById('approval-area');
    var div = document.createElement('div');
    div.className = 'approval-card';
    div.innerHTML =
      '<div class="approval-headline">Approval requested: ' + esc(approval.headline || '') + '</div>' +
      (approval.toolName ? '<div class="approval-detail">Tool: ' + esc(approval.toolName) + '</div>' : '') +
      (approval.detail ? '<div class="approval-detail">' + esc(approval.detail) + '</div>' : '') +
      '<div class="approval-buttons">' +
      '<button class="btn-primary btn-approve" data-outcome="allow_once">Allow Once</button>' +
      '<button class="btn-warn btn-approve" data-outcome="allow_reads_once">Allow Reads Once</button>' +
      '<button class="btn-danger btn-approve" data-outcome="deny">Deny</button>' +
      '</div>';

    var aid = approval.approvalId;
    pendingApprovals[aid] = div;

    div.querySelectorAll('.btn-approve').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var outcome = btn.dataset.outcome;
        var protoOutcome = 'APPROVAL_OUTCOME_' + outcome.toUpperCase();
        rpc('SessionService', 'RespondToApproval', {
          sessionId: pollSessionId,
          approvalId: aid,
          outcome: protoOutcome,
        }).then(function () {
          if (pendingApprovals[aid]) {
            pendingApprovals[aid].remove();
            delete pendingApprovals[aid];
          }
        }).catch(function (e) {
          setStatus('approval error: ' + e.message);
        });
      });
    });

    area.appendChild(div);
  }

  // --- Backends -----------------------------------------------------------

  function loadBackends() {
    var container = document.getElementById('backend-list');
    rpc('BackendService', 'ListBackends', {}).then(function (resp) {
      var backends = resp.backends || [];
      if (backends.length === 0) {
        container.innerHTML = '<div class="empty-state">No backends configured. Click <strong>+ New Backend</strong> to add one.</div>';
        return;
      }
      container.innerHTML = '';
      backends.forEach(function (b) {
        var card = document.createElement('div');
        card.className = 'backend-card';
        card.innerHTML =
          '<div class="backend-card-header">' +
          '<span class="backend-label">' + esc(b.label) + '</span>' +
          '<span class="backend-type">' + esc(b.backendType) + '</span>' +
          '</div>' +
          '<div class="backend-meta">' + esc(b.id) +
          ' · key: ••••' + esc(b.apiKeyLastFour || '????') +
          (b.baseUrl ? ' · ' + esc(b.baseUrl) : '') +
          '</div>' +
          '<div class="backend-actions">' +
          '<button class="btn-danger btn-small" data-backend-delete="' + esc(b.id) + '">Delete</button>' +
          '</div>';
        card.querySelector('[data-backend-delete]').addEventListener('click', function () {
          if (!confirm('Delete backend "' + b.label + '"?')) return;
          rpc('BackendService', 'DeleteBackend', { id: b.id }).then(function () {
            loadBackends();
          }).catch(function (e) {
            alert('Delete failed: ' + e.message);
          });
        });
        container.appendChild(card);
      });
    }).catch(function (e) {
      container.innerHTML = '<div class="empty-state">Failed to load backends: ' + esc(e.message) + '</div>';
    });
  }

  function createBackend() {
    var label = document.getElementById('new-backend-label').value.trim();
    var type = document.getElementById('new-backend-type').value.trim();
    var baseUrl = document.getElementById('new-backend-baseurl').value.trim();
    var apiKey = document.getElementById('new-backend-apikey').value.trim();
    var errEl = document.getElementById('create-backend-error');

    if (!label) { errEl.textContent = 'Label is required.'; return; }
    if (!type) { errEl.textContent = 'Type is required.'; return; }
    if (!apiKey) { errEl.textContent = 'API key is required.'; return; }
    errEl.textContent = '';

    rpc('BackendService', 'CreateBackend', {
      label: label,
      backendType: type,
      baseUrl: baseUrl,
      apiKey: apiKey,
    }).then(function () {
      document.getElementById('new-backend-form').classList.add('hidden');
      document.getElementById('new-backend-label').value = '';
      document.getElementById('new-backend-type').value = 'openai';
      document.getElementById('new-backend-baseurl').value = '';
      document.getElementById('new-backend-apikey').value = '';
      loadBackends();
    }).catch(function (e) {
      errEl.textContent = e.message;
    });
  }

  // --- Workspaces ---------------------------------------------------------

  function loadWorkspaces() {
    var container = document.getElementById('workspace-list');
    rpc('WorkspaceService', 'ListWorkspaces', {}).then(function (resp) {
      var workspaces = resp.workspaces || [];
      if (workspaces.length === 0) {
        container.innerHTML = '<div class="empty-state">No workspaces configured. Click <strong>+ New Workspace</strong> to add one.</div>';
        return;
      }
      container.innerHTML = '';
      workspaces.forEach(function (w) {
        var card = document.createElement('div');
        card.className = 'workspace-card';
        card.innerHTML =
          '<div class="workspace-card-header">' +
          '<span class="workspace-name">' + esc(w.name) + '</span>' +
          '</div>' +
          '<div class="workspace-meta">' + esc(w.id) +
          ' · ' + esc(w.hostPath) +
          (w.vcsRemote ? ' · ' + esc(w.vcsRemote) : '') +
          '</div>' +
          '<div class="workspace-actions">' +
          '<button class="btn-danger btn-small" data-ws-delete="' + esc(w.id) + '">Delete</button>' +
          '</div>';
        card.querySelector('[data-ws-delete]').addEventListener('click', function () {
          if (!confirm('Delete workspace "' + w.name + '"?')) return;
          rpc('WorkspaceService', 'DeleteWorkspace', { id: w.id }).then(function () {
            loadWorkspaces();
          }).catch(function (e) {
            alert('Delete failed: ' + e.message);
          });
        });
        container.appendChild(card);
      });
    }).catch(function (e) {
      container.innerHTML = '<div class="empty-state">Failed to load workspaces: ' + esc(e.message) + '</div>';
    });
  }

  function createWorkspace() {
    var name = document.getElementById('new-workspace-name').value.trim();
    var path = document.getElementById('new-workspace-path').value.trim();
    var vcs = document.getElementById('new-workspace-vcs').value.trim();
    var errEl = document.getElementById('create-workspace-error');

    if (!name) { errEl.textContent = 'Name is required.'; return; }
    if (!path) { errEl.textContent = 'Host path is required.'; return; }
    errEl.textContent = '';

    rpc('WorkspaceService', 'CreateWorkspace', {
      name: name,
      hostPath: path,
      vcsRemote: vcs,
    }).then(function () {
      document.getElementById('new-workspace-form').classList.add('hidden');
      document.getElementById('new-workspace-name').value = '';
      document.getElementById('new-workspace-path').value = '';
      document.getElementById('new-workspace-vcs').value = '';
      loadWorkspaces();
    }).catch(function (e) {
      errEl.textContent = e.message;
    });
  }

  // --- Agents -------------------------------------------------------------

  function loadAgents() {
    var container = document.getElementById('agent-list');
    rpc('AgentService', 'ListAgents', {}).then(function (resp) {
      var agents = resp.agents || [];
      if (agents.length === 0) {
        container.innerHTML = '<div class="empty-state">No agents configured. Click <strong>+ New Agent</strong> to add one.</div>';
        return;
      }
      container.innerHTML = '';
      agents.forEach(function (a) {
        var card = document.createElement('div');
        card.className = 'agent-card';
        card.innerHTML =
          '<div class="agent-card-header">' +
          '<span class="agent-name">' + esc(a.name) + '</span>' +
          (a.headRevId ? '<span class="agent-rev">rev: ' + esc(a.headRevId.substring(0, 12)) + '…</span>' : '<span class="agent-rev">no revisions</span>') +
          '</div>' +
          '<div class="agent-meta">' + esc(a.id) + '</div>' +
          '<div class="agent-actions">' +
          '<button class="btn-danger btn-small" data-agent-delete="' + esc(a.id) + '">Delete</button>' +
          '</div>';
        card.querySelector('[data-agent-delete]').addEventListener('click', function () {
          if (!confirm('Delete agent "' + a.name + '" and all its revisions?')) return;
          rpc('AgentService', 'DeleteAgent', { id: a.id }).then(function () {
            loadAgents();
          }).catch(function (e) {
            alert('Delete failed: ' + e.message);
          });
        });
        container.appendChild(card);
      });
    }).catch(function (e) {
      container.innerHTML = '<div class="empty-state">Failed to load agents: ' + esc(e.message) + '</div>';
    });
  }

  function createAgent() {
    var name = document.getElementById('new-agent-name').value.trim();
    var errEl = document.getElementById('create-agent-error');

    if (!name) { errEl.textContent = 'Name is required.'; return; }
    errEl.textContent = '';

    rpc('AgentService', 'CreateAgent', { name: name }).then(function () {
      document.getElementById('new-agent-form').classList.add('hidden');
      document.getElementById('new-agent-name').value = '';
      loadAgents();
    }).catch(function (e) {
      errEl.textContent = e.message;
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
          (p.err ? ' · err: ' + esc(p.err) : '');
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
