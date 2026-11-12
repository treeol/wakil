package proxy

// Data-policy enforcement for outgoing chat-completions requests.
//
// The policy answers one question for every outgoing model request right
// before it is sent: does this host accept the configured privacy settings,
// and if so what must change on the wire? It is a pure function of (target
// host, model, zdr mode, region, trusted local hosts). The transport (Exec)
// applies the returned host and body, records the decision, and performs the
// single auto-mode retry; the retry itself is transport control flow, not
// policy logic, so it lives outside this file.
//
// Modes are the config spellings: "off" (default, send unchanged), "auto"
// (try to enforce, fall back to an unenforced send when the provider has no
// ZDR endpoint for the model), "on" (enforce or fail before sending). Region
// is "off" (default), "eu", or "us".

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Host classes for policy classification.
const (
	hostOpenRouter = "openrouter"
	hostLocal      = "local"
	hostOther      = "other"
)

// PolicyInput is one policy evaluation. TrustedLocalHosts is the configured
// list of non-IP host names treated as local (no DNS lookup is ever made for
// classification; a name is local only when listed verbatim, case-insensitively).
type PolicyInput struct {
	Host              string
	Model             string
	ZDR               string // off | auto | on
	Region            string // off | eu | us
	TrustedLocalHosts []string
}

// PolicyOutput is the outcome of one evaluation. Host and Body are the
// possibly changed send targets (byte-identical to the input when nothing
// changes). Decision is the human-readable record kept for /zdr and
// /mode status. WithZDRFields reports whether the returned body carries the
// provider ZDR fields (so the transport can tell a first auto attempt from a
// fallback send without re-parsing the body).
type PolicyOutput struct {
	Host           string
	Body           []byte
	Decision       string
	WithZDRFields  bool
	HostClass      string
	EnforcementErr error
}

// PolicyError is returned when the configured policy cannot be enforced on
// the target host. The message names the host and says Wakil cannot enforce
// the policy there. It is returned before any byte is sent.
type PolicyError struct {
	Host string
	What string
}

func (e *PolicyError) Error() string {
	return "wakil cannot enforce " + e.What + " on host " + e.Host
}

// classifyHost buckets a target host for policy purposes. OpenRouter is the
// three exact hosts (never a subdomain — attribution headers accept any
// *.openrouter.ai, but only the exact hosts get the policy treatment).
// Local is loopback, RFC 1918, link-local and ULA IP literals plus any
// trusted_local_hosts name. Everything else is "other".
func classifyHost(host string, trusted []string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return hostOther
	}
	switch h {
	case "openrouter.ai", "eu.openrouter.ai", "us.openrouter.ai":
		return hostOpenRouter
	case "localhost":
		return hostLocal
	}
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || isULA(ip) {
			return hostLocal
		}
		return hostOther
	}
	for _, t := range trusted {
		if strings.EqualFold(strings.TrimSpace(t), h) {
			return hostLocal
		}
	}
	return hostOther
}

// isULA reports whether ip is in the IPv6 unique-local range fc00::/7.
func isULA(ip net.IP) bool {
	if ip.To4() != nil {
		return false
	}
	return ip[0]&0xfe == 0xfc
}

// regionalHost is the EU/US OpenRouter host for a region, or "" when off.
func regionalHost(region string) string {
	switch region {
	case "eu":
		return "eu.openrouter.ai"
	case "us":
		return "us.openrouter.ai"
	}
	return ""
}

// withZDRFields merges "zdr": true and "data_collection": "deny" into the
// body's "provider" object, preserving any existing fields, and returns the
// new body. Every other byte of the request is preserved by re-serializing
// the top-level object with the provider object replaced; field order of the
// remaining keys follows encoding/json map semantics, which is acceptable
// because only the provider object is consulted by the provider.
func withZDRFields(body []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("data policy: parse request body: %w", err)
	}
	provider := map[string]json.RawMessage{}
	if raw, ok := top["provider"]; ok && len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &provider); err != nil {
			return nil, fmt.Errorf("data policy: parse existing provider object: %w", err)
		}
	}
	provider["zdr"] = json.RawMessage("true")
	provider["data_collection"] = json.RawMessage(`"deny"`)
	merged, err := json.Marshal(provider)
	if err != nil {
		return nil, fmt.Errorf("data policy: encode provider object: %w", err)
	}
	top["provider"] = merged
	out, err := json.Marshal(top)
	if err != nil {
		return nil, fmt.Errorf("data policy: encode request body: %w", err)
	}
	return out, nil
}

// withoutZDRFields removes the two provider ZDR fields from a body that
// carries them, keeping any other provider fields.
func withoutZDRFields(body []byte) []byte {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body
	}
	raw, ok := top["provider"]
	if !ok {
		return body
	}
	provider := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &provider); err != nil {
		return body
	}
	delete(provider, "zdr")
	delete(provider, "data_collection")
	// A provider object that the fallback emptied entirely is removed, not
	// left as "provider":{} — it was added by withZDRFields, so its absence
	// is the true pre-policy shape.
	if len(provider) == 0 {
		delete(top, "provider")
	} else {
		merged, err := json.Marshal(provider)
		if err != nil {
			return body
		}
		top["provider"] = merged
	}
	out, err := json.Marshal(top)
	if err != nil {
		return body
	}
	return out
}

// ApplyPolicy evaluates the data policy for one outgoing request.
// With both switches off the body and host are returned unchanged and the
// decision is empty, so the bytes on the wire are identical to the
// unguarded path.
func ApplyPolicy(in PolicyInput, body []byte) PolicyOutput {
	// Host comparison is case-insensitive (a BaseURL may carry any case);
	// the wire host is the lowercased canonical form.
	in.Host = strings.ToLower(strings.TrimSpace(in.Host))
	out := PolicyOutput{Host: in.Host, Body: body}

	// Unknown mode spellings are a configuration error, never a silent pass:
	// refusing beats sending unenforced traffic under a typo.
	if err := validateZDRMode(in.ZDR); err != nil {
		out.EnforcementErr = err
		return out
	}
	if err := validateRegionMode(in.Region); err != nil {
		out.EnforcementErr = err
		return out
	}

	if in.ZDR == "" {
		in.ZDR = "off"
	}
	if in.Region == "" {
		in.Region = "off"
	}
	if in.ZDR == "off" && in.Region == "off" {
		return out
	}

	out.HostClass = classifyHost(in.Host, in.TrustedLocalHosts)

	switch out.HostClass {
	case hostLocal:
		// Local hosts are always allowed; the policy adds nothing there.
		out.Decision = "n/a (local)"
		return out
	case hostOpenRouter:
		// Region pinning only applies from the global host: a regional host
		// in the config is used verbatim, never rewritten. The decision
		// always states both halves (region and ZDR) so /mode status
		// never hides one behind the other.
		var decs []string
		if in.Region != "off" {
			if in.Host == "openrouter.ai" {
				out.Host = regionalHost(in.Region)
				decs = append(decs, "region "+in.Region+" pinned to "+out.Host)
			} else {
				decs = append(decs, "region "+in.Region+" (already regional host "+in.Host+")")
			}
		}
		if in.ZDR == "on" || in.ZDR == "auto" {
			newBody, err := withZDRFields(body)
			if err != nil {
				out.EnforcementErr = err
				return out
			}
			out.Body = newBody
			out.WithZDRFields = true
			decs = append(decs, "zdr enforced")
		}
		out.Decision = strings.Join(decs, " · ")
		return out
	default:
		// Other hosts: the policy can only refuse or pass through unenforced.
		if in.ZDR == "on" {
			out.EnforcementErr = &PolicyError{Host: in.Host, What: "ZDR"}
			return out
		}
		if in.Region != "off" {
			out.EnforcementErr = &PolicyError{Host: in.Host, What: "region " + in.Region}
			return out
		}
		// zdr auto on an unknown host: proceed, nothing enforced.
		out.Decision = "not enforced (" + in.Host + ")"
		return out
	}
}

// validateZDRMode enforces the config spelling at policy time as well as at
// load time, so a programmatically built Client (tests, subagents) cannot
// smuggle an invalid mode past the gate.
func validateZDRMode(m string) error {
	switch m {
	case "", "off", "auto", "on":
		return nil
	}
	return fmt.Errorf("data policy: invalid zdr mode %q (want off|auto|on)", m)
}

// validateRegionMode enforces the config spelling for the region switch.
func validateRegionMode(m string) error {
	switch m {
	case "", "off", "eu", "us":
		return nil
	}
	return fmt.Errorf("data policy: invalid openrouter_region %q (want off|eu|us)", m)
}

// ValidateDataPolicy checks the data-policy switch spellings. The config
// package validates the same spellings at LoadConfig (so a typo fails the
// load with the field name); the policy re-validates per request via the
// internal validators, so a programmatically built Client cannot smuggle an
// invalid mode past the gate.

// hostOfBaseURL extracts the host from a base URL ("https://openrouter.ai/api"
// → "openrouter.ai"). An unparsable or host-less base URL is returned as-is so
// classification falls through to "other" rather than masquerading as local.
func hostOfBaseURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return baseURL
	}
	return u.Hostname()
}

// withHost swaps the host of a base URL, preserving scheme and path prefix.
func withHost(baseURL, host string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return baseURL
	}
	u.Host = host
	return u.String()
}
