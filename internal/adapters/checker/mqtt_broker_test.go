package checker

import "testing"

func TestMQTTBrokerURL(t *testing.T) {
	tests := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"broker wins", map[string]any{"broker": "mqtt://a:1883", "url": "mqtt://b:1883"}, "mqtt://a:1883"},
		{"url fallback", map[string]any{"url": "wss://h:8084/mqtt"}, "wss://h:8084/mqtt"},
		{"hostname+port", map[string]any{"hostname": "broker.local", "port": float64(1883)}, "tcp://broker.local:1883"},
		{"host default port", map[string]any{"host": "broker.local"}, "tcp://broker.local:1883"},
		{"empty", map[string]any{}, ""},
		{"ws path in url", map[string]any{"broker": "wss://mqtt.example.com:8084/mqtt"}, "wss://mqtt.example.com:8084/mqtt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mqttBrokerURL(tt.cfg); got != tt.want {
				t.Errorf("mqttBrokerURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMQTTChecker_Validate(t *testing.T) {
	c := MQTTChecker{}
	if err := c.Validate(map[string]any{}); err == nil {
		t.Fatal("Validate empty should fail")
	}
	if err := c.Validate(map[string]any{"url": "mqtt://localhost:1883"}); err != nil {
		t.Fatalf("Validate url alias: %v", err)
	}
	if err := c.Validate(map[string]any{"broker": "wss://x/mqtt"}); err != nil {
		t.Fatalf("Validate broker: %v", err)
	}
}
