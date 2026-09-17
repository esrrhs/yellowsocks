//go:build ios

package router

// ProcessInspector for iOS: iOS sandbox prevents inspecting system socket PIDs,
// so this provides a clean, safe no-op stub that removes dependencies on gopsutil/IOKit.
type ProcessInspector struct {
}

// NewProcessInspector creates an iOS no-op process inspector
func NewProcessInspector() *ProcessInspector {
	return &ProcessInspector{}
}

func (pi *ProcessInspector) AddBypassApp(appName string) {}

func (pi *ProcessInspector) RemoveBypassApp(appName string) {}

func (pi *ProcessInspector) IsBypassPort(srcPort int) bool {
	return false
}

func (pi *ProcessInspector) GetProcessByPort(srcPort int) (string, bool) {
	return "", false
}

func (pi *ProcessInspector) Close() {}
