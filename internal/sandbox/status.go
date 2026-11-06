package sandbox

import "context"

// Capability is one thing a sandbox profile widens, with hisn's risk label.
type Capability struct {
	Capability string `json:"capability"`
	Label      string `json:"label"`
}

// ServiceState is the live state of an in-sandbox service.
type ServiceState struct {
	Name   string `json:"name"`
	Socket string `json:"socket,omitempty"`
	State  string `json:"state,omitempty"`
}

// Status describes what a sandbox is, for the startup banner and /sandbox. It
// carries no secrets and never a risk acknowledgement's meaning: the label is
// what the profile DOES, as computed by the sandbox itself.
type Status struct {
	Backend string `json:"backend"`
	// Mode is how the sandbox is configured: "generated profile", "named
	// profile" or "bare policy".
	Mode        string `json:"mode,omitempty"`
	Profile     string `json:"profile,omitempty"`      // named profile, if any
	ProfilePath string `json:"profile_path,omitempty"` // generated profile of the running session
	// Risk is the highest label ("none" when nothing widens the sandbox).
	Risk         string         `json:"risk"`
	Capabilities []Capability   `json:"capabilities,omitempty"`
	Notes        []string       `json:"notes,omitempty"`
	Services     []ServiceState `json:"services,omitempty"`
	Forwards     []string       `json:"forwards,omitempty"`
	HostServices []string       `json:"host_services,omitempty"`
	EnvSet       []string       `json:"env_set,omitempty"` // names only
	Grants       []GrantEntry   `json:"grants,omitempty"`
	// KVR is one line about the in-sandbox staging store ("" = not applicable).
	KVR string `json:"kvr,omitempty"`
	// Reason explains a choice Wakil made (e.g. why the default stayed docker).
	Reason string `json:"reason,omitempty"`
}

// Banner is the one-line startup statement.
func (s Status) Banner() string {
	b := "sandbox: backend=" + s.Backend
	if s.Risk != "" {
		b += " · risk=" + s.Risk
	}
	return b
}

// KVRProvider is implemented by backends that run the staging store (kvr)
// INSIDE the sandbox and can say where its socket is (a host path; "" while it
// is not up).
type KVRProvider interface {
	KVRSocket() string
}

// KVRPlanner is implemented by backends that know, before any session exists,
// where their in-sandbox staging socket will be ("" = staging is off). The
// socket only answers once the session runs.
type KVRPlanner interface {
	PlannedKVRSocket() string
}

// StatusReporter is implemented by backends that can describe themselves.
type StatusReporter interface {
	SandboxStatus(ctx context.Context) Status
}

// CLIExecer is implemented by a backend whose long-lived host-side processes
// (the browser's in-session Chromium and its CDP tunnel) must carry the
// session id in their host argv, so Wakil can prove ownership by cmdline
// before signalling them. ExecCLI runs spec exactly like Exec but through the
// backend's CLI transport, whose host client argv names the session. Only the
// browser uses it; every tool call stays on Exec. Backends without it (direct,
// docker) keep their own ownership markers.
type CLIExecer interface {
	ExecCLI(ctx context.Context, spec ExecSpec) (ExecHandle, error)
	// ActiveSessionID is the id of the running session ("" when none).
	ActiveSessionID() string
}
