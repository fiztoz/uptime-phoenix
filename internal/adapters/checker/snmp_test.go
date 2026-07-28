package checker

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func snmpTestAgent(t *testing.T, responder func([]byte) []byte) (string, float64) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		buffer := make([]byte, 4096)
		n, addr, readErr := conn.ReadFromUDP(buffer)
		if readErr != nil {
			return
		}
		response := responder(buffer[:n])
		if len(response) > 0 {
			_, _ = conn.WriteToUDP(response, addr)
		}
	}()
	address := conn.LocalAddr().(*net.UDPAddr)
	return address.IP.String(), float64(address.Port)
}

func TestSNMPChecker_Check_Up(t *testing.T) {
	host, port := snmpTestAgent(t, func(requestBytes []byte) []byte {
		decoder := &gosnmp.GoSNMP{Version: gosnmp.Version2c}
		request, err := decoder.SnmpDecodePacket(requestBytes)
		if err != nil {
			return nil
		}
		response := &gosnmp.SnmpPacket{
			Version: request.Version, Community: request.Community,
			PDUType: gosnmp.GetResponse, RequestID: request.RequestID,
			Variables: []gosnmp.SnmpPDU{{Name: ".1.3.6.1.2.1.1.1.0", Type: gosnmp.OctetString, Value: []byte("Phoenix Agent")}},
		}
		encoded, err := response.MarshalMsg()
		if err != nil {
			return nil
		}
		return encoded
	})
	result, err := (SNMPChecker{}).Check(context.Background(), map[string]any{
		"hostname": host, "port": port, "oid": ".1.3.6.1.2.1.1.1.0", "timeout": 1.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusUp || result.Metadata["value"] != "Phoenix Agent" {
		t.Fatalf("result = %+v; want decoded OID value", result)
	}
}

func TestSNMPChecker_Check_Down(t *testing.T) {
	host, port := snmpTestAgent(t, func([]byte) []byte { return []byte{0x30, 0x01, 0xff} })
	result, err := (SNMPChecker{}).Check(context.Background(), map[string]any{
		"hostname": host, "port": port, "oid": ".1.3.6.1.2.1.1.1.0", "timeout": 0.1,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown || !strings.Contains(result.Message, "snmp get failed") {
		t.Fatalf("result = %+v; want malformed response failure", result)
	}
}

func TestSNMPChecker_Check_Timeout(t *testing.T) {
	host, port := snmpTestAgent(t, func([]byte) []byte {
		time.Sleep(250 * time.Millisecond)
		return nil
	})
	result, err := (SNMPChecker{}).Check(context.Background(), map[string]any{
		"hostname": host, "port": port, "oid": ".1.3.6.1.2.1.1.1.0", "timeout": 0.05,
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
