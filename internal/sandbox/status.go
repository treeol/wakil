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

// StatusReporter is implemented by backends that can describe themselves.
type StatusReporter interface {
	SandboxStatus(ctx context.Context) Status
}
