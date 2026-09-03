package client

import (
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/remote-assist/tool/internal/proto"
)

// 心跳循环必须绑定「这一代连接」。share 全程复用同一个 *Client，reconnectWithBackoff 每轮
// Close() 之后立刻 Connect()，而 Connect 第一行就把 closed 复位成 false——两者之间只隔几
// 微秒。旧循环若只靠 IsClosed() 收敛，就几乎不可能在那个窗口里醒来做检查，于是挂着新连接
// 继续发心跳，每重连一次叠加一个，永不退出。
//
// 这里用心跳速率把叠加暴露出来：连了 5 代之后，单位时间内到达 relay 的心跳数应该还是一代
// 的量，而不是五倍。
func TestHeartbeatLoopDoesNotAccumulateAcrossReconnects(t *testing.T) {
	var beats atomic.Int64

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				dec := json.NewDecoder(conn)
				for {
					var msg proto.Message
					if err := dec.Decode(&msg); err != nil {
						return
					}
					if msg.Type == proto.MsgHeartbeat {
						beats.Add(1)
					}
				}
			}(conn)
		}
	}()

	const (
		interval   = 50 * time.Millisecond
		reconnects = 5
		window     = time.Second
	)
	expectPerGeneration := int64(window / interval) // 单代连接在观测窗口内应发的次数

	c := NewClient(&Config{ServerAddr: ln.Addr().String()})
	defer c.Close()
	for i := 0; i < reconnects; i++ {
		if i > 0 {
			c.Close() // 模拟 reconnectWithBackoff：断开后立刻重连
		}
		if err := c.Connect(); err != nil {
			t.Fatalf("第 %d 次连接失败: %v", i+1, err)
		}
		c.StartHeartbeatLoop(interval)
	}

	time.Sleep(3 * interval) // 让重连期间的瞬时抖动过去
	beats.Store(0)
	time.Sleep(window)
	got := beats.Load()

	if got == 0 {
		t.Fatalf("观测窗口内一次心跳都没收到，测试前提不成立（预期约 %d 次）", expectPerGeneration)
	}
	// 泄漏时是 5 代同时发（≈5 倍），阈值取 2 倍单代量，两种情形隔得很开。
	if got > 2*expectPerGeneration {
		t.Fatalf("观测窗口内收到 %d 次心跳，单代连接应约 %d 次：旧心跳 goroutine 没有随 Close 退出，正挂着新连接继续发",
			got, expectPerGeneration)
	}
}

// Close 之后心跳循环必须停下，不能继续往（已经换掉的）连接上写。
func TestHeartbeatLoopStopsAfterClose(t *testing.T) {
	var beats atomic.Int64

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				dec := json.NewDecoder(conn)
				for {
					var msg proto.Message
					if err := dec.Decode(&msg); err != nil {
						return
					}
					if msg.Type == proto.MsgHeartbeat {
						beats.Add(1)
					}
				}
			}(conn)
		}
	}()

	const interval = 50 * time.Millisecond
	c := NewClient(&Config{ServerAddr: ln.Addr().String()})
	if err := c.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	c.StartHeartbeatLoop(interval)
	// 故意停在两次 tick 之间（半个周期的偏移）再 Close：若睡整数个周期，Close 会和某次
	// tick 撞在一起，旧循环恰好在那一刻看到 IsClosed()==true 而自行退出，泄漏就被掩盖了。
	time.Sleep(4*interval + interval/2)
	if beats.Load() == 0 {
		t.Fatalf("关闭前应当收到心跳，测试前提不成立")
	}

	c.Close()

	// 重新连上（但不再启动心跳）：如果旧循环还活着，它会往这条新连接上发。
	if err := c.Connect(); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer c.Close()
	beats.Store(0)
	time.Sleep(6 * interval)

	if got := beats.Load(); got != 0 {
		t.Fatalf("Close 后本不该再有心跳，却收到 %d 次：旧循环仍在用新连接发送", got)
	}
}

// StartHeartbeatLoop 在没有连接时应当直接返回，不留下永不退出的 goroutine。
func TestStartHeartbeatLoopWithoutConnectionIsNoop(t *testing.T) {
	c := NewClient(&Config{ServerAddr: "127.0.0.1:1"})
	c.StartHeartbeatLoop(time.Millisecond) // 未 Connect
	c.Close()
	c.StartHeartbeatLoop(time.Millisecond) // 已 Close
}
