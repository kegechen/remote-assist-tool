package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

// 节点列表相关字段的线上名字是新旧客户端与 relay 之间的契约，改名等于断掉兼容：这里把它们钉死。
func TestDirectoryFieldsWireNames(t *testing.T) {
	marshal := func(v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	zero := uint64(0)

	// directory_serial 用指针：没有缓存（0）也必须发出去，它同时是能力声明。
	for name, got := range map[string]string{
		"register": marshal(RegisterRequest{DirectorySerial: &zero, DirectoryKeySerial: 3}),
		"join":     marshal(JoinRequest{DirectorySerial: &zero, DirectoryKeySerial: 3}),
	} {
		if !strings.Contains(got, `"directory_serial":0`) || !strings.Contains(got, `"directory_key_serial":3`) {
			t.Errorf("%s 请求的线上字段不对: %s", name, got)
		}
	}
	// 旧客户端不声明：两个字段都不出现。
	for name, got := range map[string]string{
		"register": marshal(RegisterRequest{}),
		"join":     marshal(JoinRequest{}),
	} {
		if strings.Contains(got, "directory") {
			t.Errorf("%s 请求没声明能力时不应带列表字段: %s", name, got)
		}
	}

	env := json.RawMessage(`{"kind":"directory"}`)
	if got := marshal(RegisterResponse{Directory: env, NodeStatus: "draining"}); !strings.Contains(got, `"directory":{"kind":"directory"}`) || !strings.Contains(got, `"node_status":"draining"`) {
		t.Errorf("注册响应的线上字段不对: %s", got)
	}
	if got := marshal(JoinResponse{Directory: env}); !strings.Contains(got, `"directory":{"kind":"directory"}`) {
		t.Errorf("加入响应的线上字段不对: %s", got)
	}
	if got := marshal(RegisterResponse{}) + marshal(JoinResponse{}); strings.Contains(got, "directory") || strings.Contains(got, "node_status") {
		t.Errorf("没有列表时响应不应带列表字段: %s", got)
	}

	if ErrCodeNodeDraining != "NODE_DRAINING" || ErrCodeNodeDisabled != "NODE_DISABLED" {
		t.Errorf("错误码改名了: %q %q", ErrCodeNodeDraining, ErrCodeNodeDisabled)
	}
}
