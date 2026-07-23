package tools

import (
	"encoding/json"
	"testing"
)

func TestStrProp(t *testing.T) {
	p := StrProp("a description")
	if p["type"] != "string" {
		t.Errorf("type = %v, want string", p["type"])
	}
	if p["description"] != "a description" {
		t.Errorf("description = %v, want 'a description'", p["description"])
	}
}

func TestIntProp(t *testing.T) {
	p := IntProp("count")
	if p["type"] != "integer" {
		t.Errorf("type = %v, want integer", p["type"])
	}
	if p["description"] != "count" {
		t.Errorf("description = %v, want 'count'", p["description"])
	}
}

func TestBoolProp(t *testing.T) {
	p := BoolProp("enabled")
	if p["type"] != "boolean" {
		t.Errorf("type = %v, want boolean", p["type"])
	}
	if p["description"] != "enabled" {
		t.Errorf("description = %v, want 'enabled'", p["description"])
	}
}

func TestArrProp(t *testing.T) {
	p := ArrProp("list of items")
	if p["type"] != "array" {
		t.Errorf("type = %v, want array", p["type"])
	}
	items, ok := p["items"].(map[string]interface{})
	if !ok {
		t.Fatal("items is not a map")
	}
	if items["type"] != "string" {
		t.Errorf("items.type = %v, want string", items["type"])
	}
	if p["description"] != "list of items" {
		t.Errorf("description = %v, want 'list of items'", p["description"])
	}
}

func TestEnumProp(t *testing.T) {
	p := EnumProp("mode", "a", "b", "c")
	if p["type"] != "string" {
		t.Errorf("type = %v, want string", p["type"])
	}
	enum, ok := p["enum"].([]string)
	if !ok {
		t.Fatalf("enum is not []string: %T", p["enum"])
	}
	if len(enum) != 3 || enum[0] != "a" || enum[1] != "b" || enum[2] != "c" {
		t.Errorf("enum = %v, want [a b c]", enum)
	}
}

func TestSchemaObj_WithRequired(t *testing.T) {
	raw := SchemaObj(map[string]interface{}{
		"key": StrProp("the key"),
	}, "key")
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["type"] != "object" {
		t.Errorf("type = %v, want object", m["type"])
	}
	props, ok := m["properties"].(map[string]interface{})
	if !ok {
		t.Fatal("properties is not a map")
	}
	if _, ok := props["key"]; !ok {
		t.Error("missing 'key' in properties")
	}
	req, ok := m["required"].([]interface{})
	if !ok {
		t.Fatal("required is not a slice")
	}
	if len(req) != 1 || req[0] != "key" {
		t.Errorf("required = %v, want [key]", req)
	}
}

func TestSchemaObj_WithoutRequired(t *testing.T) {
	raw := SchemaObj(map[string]interface{}{
		"opt": StrProp("optional"),
	})
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, exists := m["required"]; exists {
		t.Error("required should be absent when no required fields given")
	}
}
