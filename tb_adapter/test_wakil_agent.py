"""
Unit tests for the Wakil Terminal-Bench adapter.

Run with:  python -m pytest tb_adapter/test_wakil_agent.py -v
No TB installation required — the tests mock TB types directly.
"""

import json
import os
import types
import unittest
from unittest.mock import MagicMock, patch


# ---------------------------------------------------------------------------
# Minimal TB type stubs — keeps tests independent of TB being installed.
# ---------------------------------------------------------------------------

class _FailureMode:
    NONE                      = "none"
    AGENT_INSTALLATION_FAILED = "agent_installation_failed"
    UNKNOWN_AGENT_ERROR       = "unknown_agent_error"


class _AgentResult:
    def __init__(self, total_input_tokens=0, total_output_tokens=0, failure_mode=_FailureMode.NONE):
        self.total_input_tokens = total_input_tokens
        self.total_output_tokens = total_output_tokens
        self.failure_mode = failure_mode


# Patch _tb_types so every test uses the stubs above.
import tb_adapter.wakil_agent as _wa
_wa._tb_types = lambda: (_AgentResult, None, _FailureMode)


from tb_adapter.wakil_agent import (
    _allow_external,
    _build_config,
    _container_env,
    _parse_result,
    _EXIT_OK,
    _EXIT_DECLINED,
    _EXIT_GAPS,
    _EXIT_ERROR,
)


# ---------------------------------------------------------------------------
# _build_config
# ---------------------------------------------------------------------------

class TestBuildConfig(unittest.TestCase):

    def test_minimal_required(self):
        with patch.dict(os.environ, {"ILM_BASE_URL": "http://proxy:11400"}, clear=False):
            cfg = _build_config()
        self.assertEqual(cfg["base_url"], "http://proxy:11400")
        self.assertEqual(cfg["exec_mode"], "direct")
        self.assertNotIn("api_key", cfg)
        self.assertNotIn("backend", cfg)

    def test_missing_base_url_raises(self):
        env = {k: v for k, v in os.environ.items() if k != "ILM_BASE_URL"}
        with patch.dict(os.environ, env, clear=True):
            with self.assertRaises(RuntimeError, msg="ILM_BASE_URL must be set"):
                _build_config()

    def test_optional_fields_included_when_set(self):
        env = {
            "ILM_BASE_URL": "http://proxy:11400",
            "ILM_API_KEY": "sk-test",
            "ILM_BACKEND": "openrouter",
            "ILM_EXTERNAL_BACKENDS": '["openrouter","together"]',
        }
        with patch.dict(os.environ, env, clear=False):
            cfg = _build_config()
        self.assertEqual(cfg["api_key"], "sk-test")
        self.assertEqual(cfg["backend"], "openrouter")
        self.assertEqual(cfg["external_backends"], ["openrouter", "together"])

    def test_malformed_external_backends_omitted(self):
        env = {
            "ILM_BASE_URL": "http://proxy:11400",
            "ILM_EXTERNAL_BACKENDS": "not-json",
        }
        with patch.dict(os.environ, env, clear=False):
            cfg = _build_config()
        self.assertNotIn("external_backends", cfg)


# ---------------------------------------------------------------------------
# _allow_external
# ---------------------------------------------------------------------------

class TestAllowExternal(unittest.TestCase):

    def _check(self, val, expected):
        with patch.dict(os.environ, {"WAKIL_ALLOW_EXTERNAL": val}, clear=False):
            self.assertEqual(_allow_external(), expected, f"value={val!r}")

    def test_truthy_values(self):
        for v in ("1", "true", "yes", "True", "YES"):
            self._check(v, True)

    def test_falsy_values(self):
        for v in ("0", "false", "no", ""):
            self._check(v, False)

    def test_unset_is_false(self):
        env = {k: v for k, v in os.environ.items() if k != "WAKIL_ALLOW_EXTERNAL"}
        with patch.dict(os.environ, env, clear=True):
            self.assertFalse(_allow_external())


# ---------------------------------------------------------------------------
# _parse_result — the core mapping from JSON-lines to AgentResult
# ---------------------------------------------------------------------------

def _make_transcript(*events: dict) -> str:
    return "\n".join(json.dumps(e) for e in events)


class TestParseResult(unittest.TestCase):

    def _parse(self, transcript: str, exit_code: int = _EXIT_OK) -> _AgentResult:
        return _parse_result(transcript, exit_code, _AgentResult, _FailureMode)

    # --- token extraction ---

    def test_token_event_populated(self):
        transcript = _make_transcript(
            {"type": "done", "outcome": "pass"},
            {"type": "tokens", "input": 1234, "output": 567},
        )
        r = self._parse(transcript)
        self.assertEqual(r.total_input_tokens, 1234)
        self.assertEqual(r.total_output_tokens, 567)

    def test_token_event_missing_gives_zero(self):
        transcript = _make_transcript({"type": "done", "outcome": "pass"})
        r = self._parse(transcript)
        self.assertEqual(r.total_input_tokens, 0)
        self.assertEqual(r.total_output_tokens, 0)

    # --- outcome mapping ---

    def test_pass_outcome_is_none(self):
        transcript = _make_transcript({"type": "done", "outcome": "pass"})
        r = self._parse(transcript)
        self.assertEqual(r.failure_mode, _FailureMode.NONE)

    def test_gaps_outcome_is_none(self):
        # "gaps" means the workflow ran but flagged unmet criteria — TB tests decide.
        transcript = _make_transcript({"type": "done", "outcome": "gaps"})
        r = self._parse(transcript)
        self.assertEqual(r.failure_mode, _FailureMode.NONE)

    def test_declined_outcome_is_agent_error(self):
        transcript = _make_transcript({"type": "done", "outcome": "declined", "reason": "destructive command declined"})
        r = self._parse(transcript)
        self.assertEqual(r.failure_mode, _FailureMode.UNKNOWN_AGENT_ERROR)

    def test_error_event_is_agent_error(self):
        transcript = _make_transcript({"type": "error", "message": "stream reset"})
        r = self._parse(transcript)
        self.assertEqual(r.failure_mode, _FailureMode.UNKNOWN_AGENT_ERROR)

    def test_exit_code_fallback_when_no_done_event(self):
        # Truncated output (crash before done emitted) — exit code is the truth.
        r = self._parse("", exit_code=_EXIT_ERROR)
        self.assertEqual(r.failure_mode, _FailureMode.UNKNOWN_AGENT_ERROR)

    def test_exit_code_ok_with_no_done_event_is_none(self):
        # Unusual, but if exit 0 and no done event, treat as pass.
        r = self._parse("", exit_code=_EXIT_OK)
        self.assertEqual(r.failure_mode, _FailureMode.NONE)

    def test_declined_exit_code_without_done(self):
        r = self._parse("", exit_code=_EXIT_DECLINED)
        self.assertEqual(r.failure_mode, _FailureMode.UNKNOWN_AGENT_ERROR)

    def test_malformed_lines_ignored(self):
        transcript = "not json\n" + json.dumps({"type": "done", "outcome": "pass"}) + "\nalso not json"
        r = self._parse(transcript)
        self.assertEqual(r.failure_mode, _FailureMode.NONE)

    def test_full_happy_path(self):
        transcript = _make_transcript(
            {"type": "output", "line": "· running task"},
            {"type": "done", "outcome": "pass"},
            {"type": "tokens", "input": 8000, "output": 2000},
        )
        r = self._parse(transcript)
        self.assertEqual(r.failure_mode, _FailureMode.NONE)
        self.assertEqual(r.total_input_tokens, 8000)
        self.assertEqual(r.total_output_tokens, 2000)


# ---------------------------------------------------------------------------
# WakilAgent.perform_task — integration smoke test with mocked session
# ---------------------------------------------------------------------------

class TestWakilAgentPerformTask(unittest.TestCase):

    def _make_session(self, transcript: str, wakil_exit: int = 0) -> MagicMock:
        """Return a mock TmuxSession whose container.exec_run simulates a run."""
        session = MagicMock()

        wakil_result = MagicMock()
        wakil_result.exit_code = wakil_exit

        cat_result = MagicMock()
        cat_result.output = transcript.encode()

        # First exec_run call = wakil run; second = cat transcript.
        session.container.exec_run.side_effect = [wakil_result, cat_result]
        return session

    def _run(self, session, instruction="fix the bug", env=None):
        from tb_adapter.wakil_agent import WakilAgent
        base_env = {
            "ILM_BASE_URL": "http://proxy:11400",
            "WAKIL_BIN": "/usr/local/bin/wakil",
        }
        if env:
            base_env.update(env)
        with patch.dict(os.environ, base_env, clear=False):
            agent = WakilAgent()
            # Patch _install so we don't need real file-copy infra.
            agent._install = MagicMock()
            return agent.perform_task(instruction, session)

    def test_happy_path_returns_tokens(self):
        transcript = _make_transcript(
            {"type": "done", "outcome": "pass"},
            {"type": "tokens", "input": 5000, "output": 1500},
        )
        session = self._make_session(transcript)
        result = self._run(session)
        self.assertEqual(result.failure_mode, _FailureMode.NONE)
        self.assertEqual(result.total_input_tokens, 5000)
        self.assertEqual(result.total_output_tokens, 1500)

    def test_install_failure_returns_install_failed(self):
        from tb_adapter.wakil_agent import WakilAgent
        session = MagicMock()
        with patch.dict(os.environ, {"ILM_BASE_URL": "http://proxy:11400", "WAKIL_BIN": "/usr/local/bin/wakil"}):
            agent = WakilAgent()
            agent._install = MagicMock(side_effect=RuntimeError("binary not found"))
            result = agent.perform_task("task", session)
        self.assertEqual(result.failure_mode, _FailureMode.AGENT_INSTALLATION_FAILED)

    def test_allow_external_flag_included_in_command(self):
        """When WAKIL_ALLOW_EXTERNAL=1 the --allow-external flag appears in exec_run cmd."""
        transcript = _make_transcript({"type": "done", "outcome": "pass"})

        captured_cmds = []
        def fake_exec(cmd, **kwargs):
            captured_cmds.append(cmd)
            r = MagicMock()
            if cmd[0] == "cat":
                r.output = transcript.encode()
                r.exit_code = 0
            else:
                r.exit_code = 0
            return r

        session = MagicMock()
        session.container.exec_run.side_effect = fake_exec

        env = {
            "ILM_BASE_URL": "http://proxy:11400",
            "WAKIL_BIN": "/usr/local/bin/wakil",
            "WAKIL_ALLOW_EXTERNAL": "1",
        }
        with patch.dict(os.environ, env, clear=False):
            from tb_adapter.wakil_agent import WakilAgent
            a = WakilAgent()
            a._install = MagicMock()
            a.perform_task("task", session)

        run_cmd = next((c for c in captured_cmds if c and c[0] == "wakil"), None)
        self.assertIsNotNone(run_cmd, "no wakil run command captured")
        self.assertIn("--allow-external", run_cmd)

    def test_no_allow_external_flag_without_env(self):
        """Without WAKIL_ALLOW_EXTERNAL the flag must not appear."""
        transcript = _make_transcript({"type": "done", "outcome": "pass"})

        captured_cmds = []
        def fake_exec(cmd, **kwargs):
            captured_cmds.append(cmd)
            r = MagicMock()
            r.output = transcript.encode() if cmd and cmd[0] == "cat" else b""
            r.exit_code = 0
            return r

        session = MagicMock()
        session.container.exec_run.side_effect = fake_exec

        env = {
            "ILM_BASE_URL": "http://proxy:11400",
            "WAKIL_BIN": "/usr/local/bin/wakil",
        }
        stripped = {k: v for k, v in os.environ.items() if k != "WAKIL_ALLOW_EXTERNAL"}
        stripped.update(env)
        with patch.dict(os.environ, stripped, clear=True):
            from tb_adapter.wakil_agent import WakilAgent
            a = WakilAgent()
            a._install = MagicMock()
            a.perform_task("task", session)

        run_cmd = next((c for c in captured_cmds if c and c[0] == "wakil"), None)
        self.assertIsNotNone(run_cmd)
        self.assertNotIn("--allow-external", run_cmd)


if __name__ == "__main__":
    unittest.main()
