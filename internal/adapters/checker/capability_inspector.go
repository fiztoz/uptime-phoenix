package checker

import "slices"

// CapabilityInspector exposes the compiled checker inventory as the
// ProbeAssignmentCapabilities port used by assignment-write validation. It
// reports what this build can execute, never what a remote probe advertises.
type CapabilityInspector struct{}

// RemoteCapable reports whether the monitor type is a pull checker installed in
// this build. Push stays local-only; an uninstalled checker is not remote work.
func (CapabilityInspector) RemoteCapable(monitorType string) bool {
	return slices.Contains(RegisteredPullTypes(), monitorType)
}

// ProxyCapable reports whether the monitor type's checker dials the scheduler's
// "_proxy" fragment. A proxy on any other type would be silently ignored, so a
// remote assignment must not pair them.
func (CapabilityInspector) ProxyCapable(monitorType string) bool {
	return ProxyCapable(monitorType)
}
