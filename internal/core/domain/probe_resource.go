package domain

// ProbeResourceBinding names a probe-local resource without exposing its endpoint.
type ProbeResourceBinding struct {
	BindingKey string
	Kind       string
}

// ProbeAssignmentBinding selects one Docker resource for a remote assignment.
type ProbeAssignmentBinding struct {
	ProbeID string
	ProbeResourceBinding
}

// ValidProbeResourceBinding enforces the V1 resource key and kind contract.
func ValidProbeResourceBinding(b ProbeResourceBinding) bool {
	if len(b.BindingKey) < 1 || len(b.BindingKey) > 128 || (b.Kind != "docker_socket" && b.Kind != "docker_api") {
		return false
	}
	for i, c := range b.BindingKey {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || i > 0 && (c == '-' || c == '_') {
			continue
		}
		return false
	}
	return true
}
