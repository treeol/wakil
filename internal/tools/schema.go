package tools

// schema.go contains shared JSON schema property builders used by all tool
// definition functions (DefaultTools, DiscoveryTools, EditTools, GoogleTools,
// SearxngTools). Previously each function redefined these closures inline,
// leading to drift (e.g. google.go's obj always emitted "required" while
// tools.go's didn't).

import "encoding/json"

// StrProp returns a JSON schema property map for a string type with a description.
func StrProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

// IntProp returns a JSON schema property map for an integer type with a description.
func IntProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc}
}

// BoolProp returns a JSON schema property map for a boolean type with a description.
func BoolProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": desc}
}

// ArrProp returns a JSON schema property map for an array of strings with a description.
func ArrProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": desc}
}

// EnumProp returns a JSON schema property map for a string enum type with a
// description and the allowed values.
func EnumProp(desc string, values ...string) map[string]interface{} {
	return map[string]interface{}{
		"type":        "string",
		"description": desc,
		"enum":        values,
	}
}

// SchemaObj builds a JSON schema object parameter from a properties map and
// optional required field names. Only emits "required" when non-empty: a nil
// variadic marshals to "required": null, which the backend's template parser
// rejects. Returns json.RawMessage ready for use as a ToolFunction.Parameters.
func SchemaObj(props map[string]interface{}, required ...string) json.RawMessage {
	m := map[string]interface{}{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	b, _ := json.Marshal(m)
	return b
}
