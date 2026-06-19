"""
Wakil adapter for Terminal-Bench.

Registration (no fork required):
    tb run --agent-import-path tb_adapter.wakil_agent:WakilAgent \\
           --task <task-id> [--timeout 900]

Required environment variables:
    ILM_BASE_URL            ilm-proxy URL reachable from *inside* TB's container.
                            For a proxy on the host, use the Docker bridge address,
                            e.g. http://host.docker.internal:11400 or http://172.17.0.1:11400

Optional environment variables:
    ILM_API_KEY             Bearer token if the proxy requires auth.
    ILM_BACKEND             Backend name to request via X-Ilm-Backend.
                            Empty = proxy default (no header sent).
    ILM_EXTERNAL_BACKENDS   JSON array of backend names that route externally,
                            e.g. '["openrouter","together"]'. Used to gate egress.
    WAKIL_BIN               Absolute path to the wakil binary on the host.
                            Default: resolved from PATH via shutil.which("wakil").
    WAKIL_CONFIG            Path to the host wakil config file to inherit into the
                            container. Default: ~/.config/wakil/config.json.
                            The adapter reads this file as the base config, then
                            overrides base_url=ILM_BASE_URL and exec_mode=direct.
                            This is how oracle/mashura/panel settings reach the
                            container without duplication.
    WAKIL_ALLOW_EXTERNAL    Set to "1" to pre-authorise external backends.
    WAKIL_AUTO_COUNSEL      Set to "1" to pass --auto-counsel to wakil run.
    WAKIL_MAX_COUNSEL       Number for --max-counsel (default 3).
    OPENROUTER_API_KEY      Required for fusion-mode mashura panels (auto-counsel
                            with the fusion panel routes through OpenRouter).
                            Set this in the shell before running tb:
                                export OPENROUTER_API_KEY="sk-or-..."
    ANTHROPIC_API_KEY       Required for single-model Anthropic mashura panels.

Container model:
    TB provides and manages the Docker container. Wakil runs in direct mode
    (no nested Docker; exec_mode=direct in config). All commands execute in
    TB's container filesystem.

Cost accounting:
    wakil run emits {"type":"tokens","input":N,"output":N} as the last JSON-lines
    event. This adapter reads it and populates AgentResult.total_input_tokens /
    total_output_tokens so TB's per-task token report doubles as a cost-sizing
    instrument before launching a full 89-task frontier run.
"""

from __future__ import annotations

import json
import os
import shlex
import shutil
from pathlib import Path
from typing import Any


# ---------------------------------------------------------------------------
# TB type imports.
# When TB is installed (the normal case) we inherit from the real BaseAgent so
# TB's issubclass check passes. When TB is absent (unit tests in the Wakil
# repo) we fall back to a stub so the module is still importable.
# ---------------------------------------------------------------------------

try:
    from terminal_bench.agents.base_agent import AgentResult, BaseAgent, FailureMode  # type: ignore[import]
    _TB_AVAILABLE = True
except ImportError:
    _TB_AVAILABLE = False

    class FailureMode:  # type: ignore[no-redef]
        NONE                      = "none"
        AGENT_INSTALLATION_FAILED = "agent_installation_failed"
        UNKNOWN_AGENT_ERROR       = "unknown_agent_error"

    class AgentResult:  # type: ignore[no-redef]
        def __init__(self, total_input_tokens=0, total_output_tokens=0, failure_mode=FailureMode.NONE):
            self.total_input_tokens = total_input_tokens
            self.total_output_tokens = total_output_tokens
            self.failure_mode = failure_mode

    class BaseAgent:  # type: ignore[no-redef]
        pass


def _tb_types() -> tuple[Any, Any, Any]:
    """Return (AgentResult, BaseAgent, FailureMode) — used by tests that patch this."""
    return AgentResult, BaseAgent, FailureMode


# ---------------------------------------------------------------------------
# Exit codes from cmd/wakil/run.go (must stay in sync).
# ---------------------------------------------------------------------------
_EXIT_OK       = 0  # task completed, tests will decide pass/fail
_EXIT_DECLINED = 1  # a tool or egress gate was declined
_EXIT_GAPS     = 2  # workflow completed but final review flagged gaps
_EXIT_ERROR    = 3  # runtime or stream error


class WakilAgent(BaseAgent):  # type: ignore[misc]
    """
    Terminal-Bench agent adapter for Wakil.

    Mirrors the AbstractInstalledAgent pattern: copy the binary, write a
    config (with exec_mode=direct), invoke wakil run --auto, parse JSON-lines output.
    """

    @staticmethod
    def name() -> str:
        return "wakil"

    # ------------------------------------------------------------------
    # TB entry point
    # ------------------------------------------------------------------

    def perform_task(
        self,
        instruction: str,
        session: Any,
        logging_dir: Path | None = None,
    ) -> Any:
        AgentResult, BaseAgent, FailureMode = _tb_types()

        try:
            self._install(session)
        except Exception as exc:
            # Treat install failure as a hard error — the task cannot run.
            return AgentResult(
                failure_mode=FailureMode.AGENT_INSTALLATION_FAILED,
            )

        transcript, exit_code = self._run(instruction, session)
        return _parse_result(transcript, exit_code, AgentResult, FailureMode)

    # ------------------------------------------------------------------
    # Install: copy binary + write config into TB's container
    # ------------------------------------------------------------------

    def _install(self, session: Any) -> None:
        """
        Copy the wakil binary from the host into /usr/local/bin/wakil inside
        TB's container and write ~/.config/wakil/config.json.

        Config strategy: start from the host's own wakil config file (so
        oracle/mashura/panel settings are inherited verbatim), then override
        base_url and exec_mode for the in-container environment.

        Raises RuntimeError when the binary cannot be found on the host.
        """
        wakil_bin = os.environ.get("WAKIL_BIN") or shutil.which("wakil")
        if not wakil_bin:
            raise RuntimeError(
                "Cannot find the wakil binary. "
                "Set WAKIL_BIN to its absolute path, or ensure it is in PATH."
            )

        session.copy_to_container(Path(wakil_bin), "/usr/local/bin/", "wakil")
        _exec(session, ["chmod", "+x", "/usr/local/bin/wakil"])

        # Write config. shlex.quote keeps the JSON safe from shell interpretation.
        cfg_json = json.dumps(_build_config(), separators=(",", ":"))
        _exec(session, [
            "sh", "-c",
            f"mkdir -p ~/.config/wakil && printf '%s' {shlex.quote(cfg_json)}"
            " > ~/.config/wakil/config.json",
        ])

    # ------------------------------------------------------------------
    # Run: invoke wakil run --exec direct --auto … via exec_run
    # ------------------------------------------------------------------

    def _run(self, instruction: str, session: Any) -> tuple[str, int]:
        """
        Execute `wakil run --exec direct --auto [--allow-external] <instruction>`
        inside the container via exec_run (not tmux — headless, no TUI).

        Returns (transcript_json_lines, exit_code).
        """
        transcript_path = "/tmp/wakil-tb-run.jsonl"

        # Note: exec_mode=direct is set in the config file written by _install(),
        # not via a CLI flag — wakil run has no --exec flag (that's TUI-only).
        cmd = ["wakil", "run", "--auto"]
        if _allow_external():
            cmd.append("--allow-external")
        if os.environ.get("WAKIL_AUTO_COUNSEL", "").lower() in ("1", "true", "yes"):
            cmd.append("--auto-counsel")
            max_c = os.environ.get("WAKIL_MAX_COUNSEL", "3").strip()
            cmd += ["--max-counsel", max_c]
        cmd += ["--transcript", transcript_path, instruction]

        result = session.container.exec_run(
            cmd,
            environment=_container_env(),
        )
        exit_code = result.exit_code if result.exit_code is not None else _EXIT_ERROR

        cat = session.container.exec_run(["cat", transcript_path])
        transcript = cat.output.decode("utf-8", errors="replace") if cat.output else ""

        return transcript, exit_code


# ---------------------------------------------------------------------------
# Helpers (module-level so they can be unit-tested without a WakilAgent instance)
# ---------------------------------------------------------------------------

def _build_config() -> dict:
    """
    Build the wakil config.json dict for the in-container process.

    Strategy: start from the host's own wakil config file (inheriting oracle,
    mashura_panels, mashura_tool_panels, costs, etc. verbatim), then override
    the fields that must change for the containerised environment:
      - base_url  → ILM_BASE_URL (the Docker-accessible proxy address)
      - exec_mode → "direct" (TB provides the container; no nested Docker)
      - api_key   → ILM_API_KEY (optional proxy auth)
      - backend   → ILM_BACKEND (optional backend routing)

    If no host config file is found the function falls back to a minimal dict
    so the adapter still works without a configured wakil installation.
    """
    # Load host config as the base (inherits oracle/mashura/panel settings).
    cfg: dict = {}
    host_cfg_path = os.environ.get(
        "WAKIL_CONFIG",
        os.path.expanduser("~/.config/wakil/config.json"),
    )
    if os.path.exists(host_cfg_path):
        try:
            with open(host_cfg_path) as f:
                cfg = json.load(f)
        except (json.JSONDecodeError, OSError):
            cfg = {}

    # Mandatory overrides for in-container execution.
    base_url = os.environ.get("ILM_BASE_URL", "").strip()
    if not base_url:
        raise RuntimeError(
            "ILM_BASE_URL must be set to the ilm-proxy address reachable "
            "from inside TB's container (e.g. http://192.168.1.135:11400)."
        )
    cfg["base_url"] = base_url
    cfg["exec_mode"] = "direct"

    # Optional proxy overrides.
    if api_key := os.environ.get("ILM_API_KEY", "").strip():
        cfg["api_key"] = api_key
    if backend := os.environ.get("ILM_BACKEND", "").strip():
        cfg["backend"] = backend

    # External backends list from env overrides any config-file value.
    raw_ext = os.environ.get("ILM_EXTERNAL_BACKENDS", "").strip()
    if raw_ext:
        try:
            ext_list = json.loads(raw_ext)
            if isinstance(ext_list, list) and ext_list:
                cfg["external_backends"] = ext_list
        except json.JSONDecodeError:
            pass

    return cfg


def _container_env() -> dict[str, str]:
    """
    Environment variables forwarded to the in-container wakil process.
    Wakil resolves ILM_BASE_URL and credentials from its config file, but
    passing them as env vars too covers cases where config resolution fails.
    HOME is required for ~/.config/wakil/config.json resolution.
    """
    env: dict[str, str] = {"HOME": "/root"}
    for key in ("ILM_BASE_URL", "ILM_API_KEY", "ILM_BACKEND", "ILM_EXTERNAL_BACKENDS"):
        val = os.environ.get(key, "").strip()
        if val:
            env[key] = val
    # Forward oracle/mashura API keys into the container.
    # Fusion mode (mashura_panels with mode="fusion") checks OPENROUTER_API_KEY
    # directly in mashuraPanelKeys — forward it unconditionally when present.
    # Single-provider panels check the env var named by oracle_api_key_env
    # (default ANTHROPIC_API_KEY) — forward that too.
    for key in ("OPENROUTER_API_KEY", "ANTHROPIC_API_KEY"):
        val = os.environ.get(key, "").strip()
        if val:
            env[key] = val
    # Also honour any custom oracle key env name.
    custom = os.environ.get("WAKIL_ORACLE_KEY_ENV", "").strip()
    if custom and custom not in env:
        val = os.environ.get(custom, "").strip()
        if val:
            env[custom] = val
    return env


def _allow_external() -> bool:
    return os.environ.get("WAKIL_ALLOW_EXTERNAL", "").lower() in ("1", "true", "yes")


def _exec(session: Any, cmd: list[str]) -> None:
    """Run a command in the container, raise on non-zero exit."""
    result = session.container.exec_run(cmd)
    if result.exit_code not in (None, 0):
        out = result.output.decode("utf-8", errors="replace") if result.output else ""
        raise RuntimeError(f"Command {cmd} exited {result.exit_code}: {out[:400]}")


def _parse_result(
    transcript: str,
    exit_code: int,
    AgentResult: Any,
    FailureMode: Any,
) -> Any:
    """
    Parse wakil's JSON-lines transcript and map to a TB AgentResult.

    Wakil exit codes (from cmd/wakil/run.go):
        0 (ExitOK)       → FailureMode.NONE       (task ran; tests decide pass/fail)
        1 (ExitDeclined) → FailureMode.UNKNOWN_AGENT_ERROR (gate declined a call)
        2 (ExitGaps)     → FailureMode.NONE        (workflow ran; tests decide)
        3 (ExitError)    → FailureMode.UNKNOWN_AGENT_ERROR (runtime/stream error)

    The {"type":"tokens","input":N,"output":N} event is always the last event
    emitted by wakil run (after the done/error event).
    """
    input_tokens = 0
    output_tokens = 0
    failure_mode = FailureMode.NONE
    saw_done = False

    for raw_line in transcript.splitlines():
        line = raw_line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue

        etype = event.get("type", "")

        if etype == "tokens":
            input_tokens = int(event.get("input", 0))
            output_tokens = int(event.get("output", 0))

        elif etype == "done":
            saw_done = True
            outcome = event.get("outcome", "pass")
            if outcome in ("pass", "gaps"):
                # "gaps" = workflow ran but flagged unmet criteria; TB's tests
                # decide the actual pass/fail — don't report it as an agent error.
                failure_mode = FailureMode.NONE
            else:
                # "declined" or unknown: agent-side abort, not a test failure.
                failure_mode = FailureMode.UNKNOWN_AGENT_ERROR

        elif etype == "error":
            saw_done = True
            failure_mode = FailureMode.UNKNOWN_AGENT_ERROR

    # Exit-code guard: if the transcript has no done event (truncated output,
    # crash before emit), fall back to the process exit code.
    if not saw_done and exit_code != _EXIT_OK:
        failure_mode = FailureMode.UNKNOWN_AGENT_ERROR

    return AgentResult(
        total_input_tokens=input_tokens,
        total_output_tokens=output_tokens,
        failure_mode=failure_mode,
    )
