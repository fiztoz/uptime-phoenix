package checker

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestRabbitMQURL(t *testing.T) {
	tests := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"url wins", map[string]any{"url": "amqp://u:p@rabbit:5672/%2F", "hostname": "other"}, "amqp://u:p@rabbit:5672/%2F"},
		{"connection string alias", map[string]any{"connection_string": "amqps://rabbit:5671/app"}, "amqps://rabbit:5671/app"},
		{"host defaults", map[string]any{"hostname": "rabbit.local"}, "amqp://rabbit.local:5672"},
		{"host tls", map[string]any{"host": "rabbit.local", "port": 5671.0, "username": "monitor", "password": "secret", "vhost": "prod"}, "amqps://monitor:secret@rabbit.local:5671/prod"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rabbitMQURL(tt.cfg); got != tt.want {
				t.Fatalf("rabbitMQURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRabbitMQChecker_Validate(t *testing.T) {
	checker := RabbitMQChecker{}
	if err := checker.Validate(map[string]any{"url": "amqp://localhost:5672"}); err != nil {
		t.Fatalf("Validate valid URL: %v", err)
	}
	if err := checker.Validate(map[string]any{"hostname": "localhost", "port": 5672.0}); err != nil {
		t.Fatalf("Validate host/port: %v", err)
	}
	if err := checker.Validate(map[string]any{"url": "http://localhost:5672"}); err == nil {
		t.Fatalf("Validate accepted non-AMQP URL")
	}
	if err := checker.Validate(map[string]any{}); err == nil {
		t.Fatalf("Validate accepted missing URL")
	}
}

func TestRabbitMQChecker_Check_Up(t *testing.T) {
	result, err := (RabbitMQChecker{}).Check(context.Background(), map[string]any{
		"url": rabbitMQTestBroker(t), "timeout": 1.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusUp || !strings.Contains(result.Message, "opened channel") {
		t.Fatalf("result = %+v; want connected and channel opened", result)
	}
}

func TestRabbitMQChecker_Check_Down(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	result, err := (RabbitMQChecker{}).Check(context.Background(), map[string]any{
		"url": "amqp://" + address, "timeout": 0.1,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown || !strings.Contains(result.Message, "connect failed") {
		t.Fatalf("result = %+v; want connection failure", result)
	}
}

type amqpFrame struct {
	frameType byte
	channel   uint16
	payload   []byte
}

func rabbitMQTestBroker(t *testing.T) string {
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
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		reader := bufio.NewReader(conn)

		protocol := make([]byte, 8)
		if _, err := io.ReadFull(reader, protocol); err != nil || string(protocol) != "AMQP\x00\x00\x09\x01" {
			return
		}
		if err := writeAMQPMethod(conn, 0, methodPayload(10, 10,
			[]byte{0, 9},
			amqpTable(nil),
			amqpLongstr("PLAIN"),
			amqpLongstr("en_US"),
		)); err != nil {
			return
		}
		if !readAMQPMethod(reader, 0, 10, 11) {
			return
		}
		if err := writeAMQPMethod(conn, 0, methodPayload(10, 30,
			amqpUint16(0),
			amqpUint32(131072),
			amqpUint16(0),
		)); err != nil {
			return
		}
		if !readAMQPMethod(reader, 0, 10, 31) || !readAMQPMethod(reader, 0, 10, 40) {
			return
		}
		if err := writeAMQPMethod(conn, 0, methodPayload(10, 41, amqpShortstr(""))); err != nil {
			return
		}
		if !readAMQPMethod(reader, 1, 20, 10) {
			return
		}
		if err := writeAMQPMethod(conn, 1, methodPayload(20, 11, amqpLongstr(""))); err != nil {
			return
		}

		for {
			frame, err := readAMQPFrame(reader)
			if err != nil {
				return
			}
			classID, methodID, ok := frameMethod(frame)
			if !ok {
				continue
			}
			switch {
			case frame.channel == 1 && classID == 20 && methodID == 40:
				_ = writeAMQPMethod(conn, 1, methodPayload(20, 41))
			case frame.channel == 0 && classID == 10 && methodID == 50:
				_ = writeAMQPMethod(conn, 0, methodPayload(10, 51))
				return
			}
		}
	}()

	return "amqp://guest:guest@" + listener.Addr().String() + "/%2F"
}

func readAMQPFrame(reader *bufio.Reader) (amqpFrame, error) {
	var header [7]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return amqpFrame{}, err
	}
	size := binary.BigEndian.Uint32(header[3:7])
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return amqpFrame{}, err
	}
	end, err := reader.ReadByte()
	if err != nil {
		return amqpFrame{}, err
	}
	if end != 0xCE {
		return amqpFrame{}, fmt.Errorf("invalid AMQP frame end: %#x", end)
	}
	return amqpFrame{frameType: header[0], channel: binary.BigEndian.Uint16(header[1:3]), payload: payload}, nil
}

func readAMQPMethod(reader *bufio.Reader, channel, classID, methodID uint16) bool {
	frame, err := readAMQPFrame(reader)
	if err != nil {
		return false
	}
	gotClassID, gotMethodID, ok := frameMethod(frame)
	return ok && frame.channel == channel && gotClassID == classID && gotMethodID == methodID
}

func frameMethod(frame amqpFrame) (uint16, uint16, bool) {
	if frame.frameType != 1 || len(frame.payload) < 4 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(frame.payload[0:2]), binary.BigEndian.Uint16(frame.payload[2:4]), true
}

func writeAMQPMethod(conn net.Conn, channel uint16, payload []byte) error {
	var header [7]byte
	header[0] = 1
	binary.BigEndian.PutUint16(header[1:3], channel)
	binary.BigEndian.PutUint32(header[3:7], uint32(len(payload)))
	if _, err := conn.Write(header[:]); err != nil {
		return err
	}
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	_, err := conn.Write([]byte{0xCE})
	return err
}

func methodPayload(classID, methodID uint16, parts ...[]byte) []byte {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint16(payload[0:2], classID)
	binary.BigEndian.PutUint16(payload[2:4], methodID)
	for _, part := range parts {
		payload = append(payload, part...)
	}
	return payload
}

func amqpUint16(v uint16) []byte {
	out := make([]byte, 2)
	binary.BigEndian.PutUint16(out, v)
	return out
}

func amqpUint32(v uint32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, v)
	return out
}

func amqpShortstr(v string) []byte {
	return append([]byte{byte(len(v))}, []byte(v)...)
}

func amqpLongstr(v string) []byte {
	out := make([]byte, 4, 4+len(v))
	binary.BigEndian.PutUint32(out, uint32(len(v)))
	return append(out, []byte(v)...)
}

func amqpTable(_ map[string]string) []byte {
	return amqpUint32(0)
}
