package hisn

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// The 0.3.1 surface of the fake hisn: profile validate|explain, run --profile
// (generated file or $HISN_PROFILES name), the risk-ack rules and the
// E_PROFILE_UNTRUSTED check, all modelled on what the real 0.3.1 binary does
// (checked offline against `hisn profile validate|explain`).

var riskOrder = map[string]int{"none": 0, "data": 1, "signing": 2, "host-exec": 3, "root-equivalent": 4, "kernel-surface": 5}

type fakeRisk struct {
	Capability string `json:"capability"`
	Label      string `json:"label"`
}

func sockLabel(sock string) string {
	switch {
	case strings.Contains(sock, "ssh-agent"):
		return "signing"
	case strings.Contains(sock, "docker.sock") || strings.Contains(sock, "podman.sock"):
		return "root-equivalent"
	case strings.Contains(sock, "dbus") || strings.Contains(sock, "system_bus_socket"):
		return "host-exec"
	}
	return ""
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func tables(doc map[string]any, k string) []map[string]any {
	var out []map[string]any
	if arr, ok := doc[k].([]map[string]any); ok {
		return arr
	}
	if arr, ok := doc[k].([]any); ok {
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// fakeProfileRisks decodes a profile and applies hisn's load-time rules.
func fakeProfileRisks(text string) ([]fakeRisk, error) {
	var doc map[string]any
	if _, err := toml.Decode(text, &doc); err != nil {
		return nil, fmt.Errorf("profile parse: %w", err)
	}
	if str(doc, "version") != "hisn-profile/v1" {
		return nil, fmt.Errorf("E_PROFILE_NOT_PROFILE: version must be hisn-profile/v1")
	}
	var risks []fakeRisk
	names := map[string]bool{}
	for _, s := range tables(doc, "services") {
		n := str(s, "name")
		if names[n] {
			return nil, fmt.Errorf("[[services]]: duplicate name %q", n)
		}
		names[n] = true
		if rd, ok := s["ready"].(map[string]any); ok {
			for k := range rd {
				if k != "socket" && k != "timeout" {
					return nil, fmt.Errorf("profile parse: unknown field `%s` in ready", k)
				}
			}
		}
		risks = append(risks, fakeRisk{"service:" + n, "none"})
	}
	for _, f := range tables(doc, "forward") {
		label := sockLabel(str(f, "socket"))
		if label == "" {
			label = str(f, "risk")
			if label == "" {
				return nil, fmt.Errorf("E_RISK_ACK_REQUIRED: [[forward]] %s: unknown socket %s — set `risk` explicitly", str(f, "name"), str(f, "socket"))
			}
		}
		if err := checkAck("[[forward]] "+str(f, "name"), label, str(f, "risk_ack"), label == "signing" || label == "host-exec" || label == "root-equivalent"); err != nil {
			return nil, err
		}
		risks = append(risks, fakeRisk{"forward:" + str(f, "name"), label})
	}
	for _, h := range tables(doc, "host_services") {
		label := sockLabel(str(h, "socket"))
		if r := str(h, "risk"); r != "" {
			label = r
		}
		if label == "" || riskOrder[label] < riskOrder["host-exec"] {
			label = "host-exec"
		}
		if err := checkAck("[[host_services]] "+str(h, "name"), label, str(h, "risk_ack"), true); err != nil {
			return nil, err
		}
		risks = append(risks, fakeRisk{"host_service:" + str(h, "name"), label})
	}
	if d, ok := doc["docker"].(map[string]any); ok && str(d, "tier") == "socket" {
		if err := checkAck("[docker]", "root-equivalent", str(d, "risk_ack"), true); err != nil {
			return nil, err
		}
		risks = append(risks, fakeRisk{"docker:socket", "root-equivalent"})
	}
	return risks, nil
}

func checkAck(what, label, ack string, required bool) error {
	switch {
	case required && ack == "":
		return fmt.Errorf("E_RISK_ACK_REQUIRED: %s requires risk_ack = %q", what, label)
	case ack != "" && ack != label && required:
		return fmt.Errorf("E_RISK_ACK_MISMATCH: %s risk_ack = %q but the computed label is %q", what, ack, label)
	}
	return nil
}

func highest(risks []fakeRisk) string {
	h := "none"
	for _, r := range risks {
		if riskOrder[r.Label] > riskOrder[h] {
			h = r.Label
		}
	}
	return h
}

// fakeResolveProfile maps --profile/explain arguments to a file: a path with a
// '/' is itself, a name is looked up in $HISN_PROFILES (built-ins do not exist
// next to the fake, like the bare release binary).
func fakeResolveProfile(spec string) (path string, byName bool, err error) {
	if strings.Contains(spec, "/") {
		if _, e := os.Stat(spec); e != nil {
			return "", false, fmt.Errorf("profile file not found: %s", spec)
		}
		return spec, false, nil
	}
	if d := os.Getenv("HISN_PROFILES"); d != "" {
		p := filepath.Join(d, spec+".toml")
		if _, e := os.Stat(p); e == nil {
			return p, true, nil
		}
	}
	return "", false, fmt.Errorf("E_PROFILE_NOTFOUND: profile %q not found in $HISN_PROFILES or built-in profiles/ next to the binary", spec)
}

func fakeProfileCmd(args []string) int {
	if len(args) < 2 {
		return 125
	}
	switch args[0] {
	case "validate", "explain":
		p, _, err := fakeResolveProfile(args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, "hisn:", err)
			return 125
		}
		b, _ := os.ReadFile(p)
		risks, err := fakeProfileRisks(string(b))
		if err != nil {
			fmt.Fprintf(os.Stderr, "hisn: parsing profile %s: %v\n", p, err)
			return 125
		}
		if args[0] == "validate" {
			fmt.Println(`version = "hisn-policy/v1"`)
			return 0
		}
		out, _ := json.Marshal(map[string]any{"schema": "hisn-profile-explain/v1", "version": "hisn-profile/v1",
			"capabilities": nonNilRisks(risks), "risk": highest(risks), "notes": []string{}})
		fmt.Println(string(out))
		return 0
	}
	return 125
}

func nonNilRisks(r []fakeRisk) []fakeRisk {
	if r == nil {
		return []fakeRisk{}
	}
	return r
}

// fakeRunProfile prepares `run --profile`: returns the policy text to store as
// the session's policy, its risks, or an exit code on failure.
func fakeRunProfile(state string, args []string) (text string, risks []fakeRisk, rc int) {
	spec := flagVal(args, "--profile")
	p, byName, err := fakeResolveProfile(spec)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hisn:", err)
		return "", nil, 125
	}
	b, _ := os.ReadFile(p)
	risks, err = fakeProfileRisks(string(b))
	if err != nil {
		fmt.Fprintf(os.Stderr, "hisn: parsing profile %s: %v\n", p, err)
		return "", nil, 125
	}
	if !byName { // E_PROFILE_UNTRUSTED: under the process cwd or the session cwd
		canon, _ := filepath.EvalSymlinks(p)
		pc, _ := os.Getwd()
		pcc, _ := filepath.EvalSymlinks(pc)
		sc, _ := filepath.EvalSymlinks(flagVal(args, "--cwd"))
		for _, root := range []string{pcc, sc} {
			if root != "" && (canon == root || strings.HasPrefix(canon, root+"/")) {
				fmt.Fprintf(os.Stderr, "hisn: E_PROFILE_UNTRUSTED: profile %s lies under %s\n", canon, root)
				return "", nil, 125
			}
		}
	}
	_ = os.WriteFile(filepath.Join(state, "last_proc_cwd"), []byte(mustGetwd()), 0o600)
	for _, r := range risks {
		if r.Label != "none" && !hasFlag(args, "--quiet-risk") {
			fmt.Fprintf(os.Stderr, "RISK: %s — %s\n", r.Label, r.Capability)
		}
	}
	return string(b), risks, 0
}

func mustGetwd() string { d, _ := os.Getwd(); return d }
