package mcp

import (
	"encoding/json"
	"testing"
)

// TestSchemasAreValidJSON InputSchema 是手写的 json.RawMessage，写错一个转义，
// tools/list 整个响应都会序列化失败——宿主看到的是 MCP server 没有任何工具。
func TestSchemasAreValidJSON(t *testing.T) {
	for _, s := range AllSchemas() {
		if !json.Valid(s.InputSchema) {
			t.Fatalf("%s 的 inputSchema 不是合法 JSON: %s", s.Name, s.InputSchema)
		}
	}
}

// TestConnectSchemaDeclaresMinProto connect 的 schema 设了 additionalProperties:false，
// 不声明 min_proto 的话，严格校验参数的宿主会在发出调用前就把它拦下。
func TestConnectSchemaDeclaresMinProto(t *testing.T) {
	for _, s := range AllSchemas() {
		if s.Name != "connect" {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(s.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		p, ok := schema.Properties["min_proto"]
		if !ok {
			t.Fatal("connect schema 缺少 min_proto")
		}
		if len(p.Enum) != 2 || p.Enum[0] != "1" || p.Enum[1] != "2" {
			t.Fatalf("min_proto 的 enum 应为 [1 2]，实际 %v", p.Enum)
		}
		return
	}
	t.Fatal("没有 connect 工具")
}
