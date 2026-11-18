package hisn

// policyFile mirrors the hisn-policy/v1 TOML schema (only what Wakil sets).
type policyFile struct {
	Version string        `toml:"version"`
	Session policySession `toml:"session"`
	Mounts  policyMounts  `toml:"mounts"`
	Env     policyEnv     `toml:"env"`
}

type policySession struct {
	Cwd      string `toml:"cwd"`
	Home     string `toml:"home"`
	Network  string `toml:"network"`
	Hostname string `toml:"hostname"`
	// 0.2.1: /dev/shm tmpfs (always on for Wakil sessions; Chrome/Node and
	// POSIX shm users need it) and the /tmp tmpfs size, both MiB.
	Shm     bool  `toml:"shm"`
	ShmSize int64 `toml:"shm_size"`
	TmpSize int64 `toml:"tmp_size"`
}

type policyMounts struct {
	GrantableRW []string `toml:"grantable_rw"`
	// Project additions (.wakil/sandbox.json); omitted when empty so a session
	// without overrides renders exactly the policy Wakil always rendered.
	RO   []string `toml:"ro,omitempty"`
	RW   []string `toml:"rw,omitempty"`
	Mask []string `toml:"mask,omitempty"`
}

type policyEnv struct {
	Pass []string          `toml:"pass"`
	Set  map[string]string `toml:"set,omitempty"`
}

// policyParams are the inputs of one rendered session policy.
type policyParams struct {
	Cwd, Home, Hostname string
	Network             string // "host" (default) or "open"
	GrantableRW         []string
	EnvPass             []string
	ShmSize, TmpSize    int64
	EnvSet              map[string]string
	MountsRO, MountsRW  []string
	Mask                []string
}

// policyFor builds the session policy. network is p.Network ("host" by default,
// "open" when the user opted in); "open" narrows outbound to public TCP only
// (hisn 0.6.0). shm is true by decision: /dev/shm is needed by real toolchains
// (Chrome, Node, POSIX shm), and hisn mounts it with /dev's flags, not /tmp's.
func policyFor(p policyParams) policyFile {
	net := p.Network
	if net == "" {
		net = "host"
	}
	pf := policyFile{
		Version: "hisn-policy/v1",
		Session: policySession{Cwd: p.Cwd, Home: p.Home, Network: net, Hostname: p.Hostname,
			Shm: true, ShmSize: p.ShmSize, TmpSize: p.TmpSize},
		Mounts: policyMounts{GrantableRW: nonNil(p.GrantableRW), RO: p.MountsRO, RW: p.MountsRW, Mask: p.Mask},
		Env:    policyEnv{Pass: nonNil(p.EnvPass), Set: p.EnvSet},
	}
	if len(pf.Env.Set) == 0 {
		pf.Env.Set = nil
	}
	return pf
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
