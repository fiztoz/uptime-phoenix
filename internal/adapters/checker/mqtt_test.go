package checker

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func readMQTTPacket(conn net.Conn) (byte, []byte, error) {
	var fixed [1]byte
	if _, err := io.ReadFull(conn, fixed[:]); err != nil {
		return 0, nil, err
	}
	remaining := 0
	multiplier := 1
	for i := 0; i < 4; i++ {
		var encoded [1]byte
		if _, err := io.ReadFull(conn, encoded[:]); err != nil {
			return 0, nil, err
		}
		remaining += int(encoded[0]&127) * multiplier
		if encoded[0]&128 == 0 {
			payload := make([]byte, remaining)
			_, err := io.ReadFull(conn, payload)
			return fixed[0], payload, err
		}
		multiplier *= 128
	}
	return 0, nil, fmt.Errorf("invalid MQTT remaining length")
}

func mqttTestBroker(t *testing.T, connack, suback bool) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		packetType, _, readErr := readMQTTPacket(conn)
		if readErr != nil || packetType>>4 != 1 || !connack {
			<-time.After(500 * time.Millisecond)
			return
		}
		if _, err := conn.Write([]byte{0x20, 0x02, 0x00, 0x00}); err != nil {
			return
		}
		packetType, payload, readErr := readMQTTPacket(conn)
		if readErr != nil || packetType>>4 != 8 || len(payload) < 2 || !suback {
			<-time.After(500 * time.Millisecond)
			return
		}
		_, _ = conn.Write([]byte{0x90, 0x03, payload[0], payload[1], 0x01})
	}()
	return "tcp://" + listener.Addr().String()
}

func TestMQTTChecker_Check_Up(t *testing.T) {
	result, err := (MQTTChecker{}).Check(context.Background(), map[string]any{
		"broker": mqttTestBroker(t, true, true), "topic": "github.com/fiztoz/uptime-phoenix/health", "timeout": 1.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusUp || !strings.Contains(result.Message, "subscribed") {
		t.Fatalf("result = %+v; want connected and subscribed", result)
	}
}

func TestMQTTChecker_Check_Down(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	result, err := (MQTTChecker{}).Check(context.Background(), map[string]any{
		"broker": "tcp://" + address, "timeout": 0.1,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown || !strings.Contains(result.Message, "connect failed") {
		t.Fatalf("result = %+v; want connection failure", result)
	}
}

func TestMQTTChecker_Check_Timeout(t *testing.T) {
	result, err := (MQTTChecker{}).Check(context.Background(), map[string]any{
		"broker": mqttTestBroker(t, false, false), "timeout": 0.05,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown {
		t.Fatalf("status = %s; want DOWN", result.Status)
	}
	message := strings.ToLower(result.Message)
	if !strings.Contains(message, "timeout") && !strings.Contains(message, "deadline") {
		t.Fatalf("message = %q; want timeout diagnostic", result.Message)
	}
}
