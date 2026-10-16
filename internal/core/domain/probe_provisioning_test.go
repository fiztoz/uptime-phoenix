package domain

import "testing"

func TestProbeSSHHostSyntax(t *testing.T) {
	for _, address := range []string{"edge.example:22", "edge-1:2222", "127.0.0.1:22", "[2001:db8::1]:22"} {
		if !(ProbeSSHHost{Address: address, User: "ec2-user"}).Valid() {
			t.Errorf("valid target rejected: %q", address)
		}
	}
	for _, address := range []string{
		"", "edge", "edge:0", "edge:65536", "edge:022", "edge:+22", "edge:ssh", "user@edge:22", "https://edge:22",
		"edge:22/path", "edge:22\n", "[fe80::1%eth0]:22", "bad host:22", "bad_host:22", ".edge:22", "edge..test:22", "-edge:22",
	} {
		if (ProbeSSHHost{Address: address, User: "root"}).Valid() {
			t.Errorf("invalid target accepted: %q", address)
		}
	}
	for _, user := range []string{"", "root\n", "root;id", "root@host", "root/name", "root name"} {
		if (ProbeSSHHost{Address: "edge:22", User: user}).Valid() {
			t.Errorf("invalid user accepted: %q", user)
		}
	}
}
