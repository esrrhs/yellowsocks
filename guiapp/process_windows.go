//go:build windows

package guiapp

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/esrrhs/gohome/loggo"
	"golang.org/x/sys/windows"
)

// processResolver attributes relayed connections to the OS process that
// opened them, using the system connection tables. Every proxy client comes
// in over loopback, so the owner of the client socket decides the strategy:
//
//   - Owner is this process: the client is the tun2socks capture engine, and
//     the app is the owner of a socket to the address the engine dialed (real
//     or fake) — the REMOTE-address row.
//   - Owner is another process: the client is the application itself (system
//     proxy), so that row's owner is the answer — the LOCAL-port row.
//
// Both can happen at once: users keep the system proxy on while TUN is up,
// and the tunnel state alone would attribute those domain-target system-proxy
// connections to nobody.
//
// Results are cached for a couple of seconds: segmented downloaders open
// dozens of connections to the same edge per second, and each miss costs a
// full table walk.
type processResolver struct {
	mu             sync.Mutex
	cache          map[string]cachedProcess
	icons          map[string]string // exe path -> data URL (unbounded: process count is small)
	tcpListenPorts map[uint16]bool   // our own listeners, excluded from local-port lookups
	// tunActive reports whether the capture adapter is up: it decides which
	// strategy FindProcess uses (remote-address row vs local-port row).
	tunActive func() bool
}

type cachedProcess struct {
	name string
	icon string
	at   time.Time
}

const processCacheTTL = 2 * time.Second

func newProcessResolver(tcpListenPorts []string, tunActive func() bool) *processResolver {
	ports := map[uint16]bool{}
	for _, a := range tcpListenPorts {
		if _, p, err := splitListenPort(a); err == nil && p > 0 {
			ports[p] = true
		}
	}
	return &processResolver{
		cache:          map[string]cachedProcess{},
		icons:          map[string]string{},
		tcpListenPorts: ports,
		tunActive:      tunActive,
	}
}

func splitListenPort(addr string) (string, uint16, error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	return h, parsePort(p), nil
}

func parsePort(p string) uint16 {
	var v uint16
	for _, ch := range p {
		if ch < '0' || ch > '9' {
			return 0
		}
		v = v*10 + uint16(ch-'0')
	}
	return v
}

// FindProcess implements proxy.ProcessResolver.
func (r *processResolver) FindProcess(srcIP net.IP, srcPort int, dstHost string, dstPort int) (string, string) {
	// A loopback client is either the application itself (system proxy) or
	// our own tun2socks engine (TUN capture) — and both are live at once when
	// the system proxy stays on under TUN. The owner of the client socket
	// tells them apart, so the tunnel state is only a fallback hint.
	if r.tcpListenPorts[uint16(srcPort)] {
		return "", "" // one of our own listeners — nothing to attribute
	}
	self := windows.GetCurrentProcessId()
	if owner, ok := findPIDByLocalPort(uint16(srcPort)); ok {
		if owner == self {
			return r.attributeByRemote(dstHost, dstPort)
		}
		return r.attributeByLocal(srcPort, owner)
	}
	// The client row is already gone (short-lived connection) or the table
	// read failed. With TUN up the client was most likely the capture
	// engine, so the dialed address is still worth a try.
	if r.tunActive != nil && r.tunActive() {
		return r.attributeByRemote(dstHost, dstPort)
	}
	return "", ""
}

// attributeByRemote finds the app by the socket it opened to the target the
// capture engine dialed. The engine replays the app's own destination, so the
// target is an address on this path: the initial call passes the literal
// target and retries pass the same literal target stored on the session.
func (r *processResolver) attributeByRemote(dstHost string, dstPort int) (string, string) {
	host := strings.Trim(dstHost, "[]")
	if net.ParseIP(host) == nil {
		return "", ""
	}
	key := fmt.Sprintf("R%s:%d", host, dstPort)
	if name, icon, ok := r.cached(key); ok {
		return name, icon
	}
	var pid uint32
	var ok bool
	if v4 := net.ParseIP(host).To4(); v4 != nil {
		pid, ok = findPIDByRemoteTCP(v4, uint16(dstPort))
	}
	return r.remember(key, pid, ok)
}

// attributeByLocal answers from the client socket's owner — the system-proxy
// path, where the app dialed our listener itself.
func (r *processResolver) attributeByLocal(srcPort int, pid uint32) (string, string) {
	key := fmt.Sprintf("L%d", srcPort)
	if name, icon, ok := r.cached(key); ok {
		return name, icon
	}
	return r.remember(key, pid, true)
}

func (r *processResolver) cached(key string) (string, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.cache[key]
	if !ok || time.Since(c.at) >= processCacheTTL {
		return "", "", false
	}
	return c.name, c.icon, true
}

// remember turns a lookup outcome into the cached answer; an unknown owner
// caches an empty answer so a burst of connections to the same target does
// not repeat the table walk.
func (r *processResolver) remember(key string, pid uint32, ok bool) (string, string) {
	if !ok || pid == 0 {
		r.storeCache(key, cachedProcess{at: time.Now()})
		return "", ""
	}
	name, icon := r.processIdentity(pid)
	r.storeCache(key, cachedProcess{name: name, icon: icon, at: time.Now()})
	if name != "" {
		loggo.Debug("[GUI] connection attributed to %s (pid %d)", name, pid)
	}
	return name, icon
}

func (r *processResolver) storeCache(key string, c cachedProcess) {
	r.mu.Lock()
	r.cache[key] = c
	r.mu.Unlock()
}

// UpdateListenPorts refreshes the listener-port exclusion set (the config
// can change them between engine restarts).
func (r *processResolver) UpdateListenPorts(addrs []string) {
	ports := map[uint16]bool{}
	for _, a := range addrs {
		if _, p, err := splitListenPort(a); err == nil && p > 0 {
			ports[p] = true
		}
	}
	r.mu.Lock()
	r.tcpListenPorts = ports
	r.mu.Unlock()
}

// processName is the cheap identity used to collapse several pids of one
// application: the executable file name only, no icon extraction.
func processName(pid uint32) string {
	path, err := processImagePath(pid)
	if err != nil || path == "" {
		return ""
	}
	return filepath.Base(path)
}

// processIdentity resolves the exe name and icon for a PID, caching by path.
func (r *processResolver) processIdentity(pid uint32) (string, string) {
	path, err := processImagePath(pid)
	if err != nil || path == "" {
		return "", ""
	}
	base := filepath.Base(path)
	r.mu.Lock()
	icon := r.icons[path]
	r.mu.Unlock()
	if icon == "" {
		icon = exeIconDataURL(path)
		r.mu.Lock()
		r.icons[path] = icon
		r.mu.Unlock()
	}
	return base, icon
}

// ---- Windows connection tables and process identity ----

var (
	modiphlpapi     = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtTCP   = modiphlpapi.NewProc("GetExtendedTcpTable")
	modshell32      = windows.NewLazySystemDLL("shell32.dll")
	procSHGetFileIW = modshell32.NewProc("SHGetFileInfoW")
	moduser32       = windows.NewLazySystemDLL("user32.dll")
	procGetIconInfo = moduser32.NewProc("GetIconInfo")
	modgdi32        = windows.NewLazySystemDLL("gdi32.dll")
	procGetDIBits   = modgdi32.NewProc("GetDIBits")
	procCreateDC    = modgdi32.NewProc("CreateCompatibleDC")
	procDeleteDC    = modgdi32.NewProc("DeleteDC")
	procDeleteObj   = modgdi32.NewProc("DeleteObject")
	modk32          = windows.NewLazySystemDLL("kernel32.dll")
	procQueryImg    = modk32.NewProc("QueryFullProcessImageNameW")
)

const (
	afInet              = 2
	tcpTableOwnerPIDAll = 5
	tcpStateEstab       = 5
	shgfiIcon           = 0x100
	shgfiLargeIcon      = 0x0
)

// tcpRow is MIB_TCPROW_OWNER_PID (24 bytes); the table is a DWORD entry
// count followed by the rows.
type tcpRow struct {
	state     uint32
	localArg  uint32
	localPt   uint32
	remoteArg uint32
	remotePt  uint32
	pid       uint32
}

// rowPort decodes the Win32 quirk: the DWORD port field carries the port in
// network byte order in its low word.
func rowPort(dw uint32) uint16 {
	return uint16(dw>>8&0xff) | uint16(dw&0xff)<<8
}

// rowIP decodes the DWORD address: its bytes sit in network order already.
func rowIP(dw uint32) net.IP {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], dw)
	return net.IP(b[:])
}

// tcpOwnerRows walks MIB_TCPROW_OWNER_PID and hands every established row
// to the visitor.
func tcpOwnerRows(visit func(localIP net.IP, localPort, remotePort uint16, remoteIP net.IP, pid uint32)) error {
	// GetExtendedTcpTable(pTcpTable, pdwSize, bOrder, ulAf, TableClass, Reserved):
	// the first call asks for the buffer size — it answers with
	// ERROR_INSUFFICIENT_BUFFER (122) and the needed byte count, which is
	// the success path; the second call fills the buffer and must return 0.
	var size uint32
	r1, _, callErr := procGetExtTCP.Call(0, uintptr(unsafe.Pointer(&size)), 0, afInet, tcpTableOwnerPIDAll, 0)
	if r1 != 122 || size < 4 {
		return fmt.Errorf("GetExtendedTcpTable size probe: r1=%d %v", r1, callErr)
	}
	buf := make([]byte, size)
	r1, _, callErr = procGetExtTCP.Call(uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)), 0, afInet, tcpTableOwnerPIDAll, 0)
	if r1 != 0 {
		return fmt.Errorf("GetExtendedTcpTable: %v", callErr)
	}
	if len(buf) < 4 {
		return fmt.Errorf("GetExtendedTcpTable: short buffer")
	}
	count := binary.LittleEndian.Uint32(buf[0:4])
	rows := (*[1 << 20]tcpRow)(unsafe.Pointer(&buf[4]))[:count]
	for _, row := range rows {
		if row.state != tcpStateEstab {
			continue
		}
		visit(rowIP(row.localArg), rowPort(row.localPt), rowPort(row.remotePt), rowIP(row.remoteArg), row.pid)
	}
	return nil
}

// tunLocalAddr is the source address every freshly tunnel-captured flow
// carries: the app's socket is bound to the TUN adapter's own address when the
// packet enters the tunnel. Sockets on the physical address to the same
// endpoint either bypassed the tunnel or are our own dials.
var tunLocalAddr = net.ParseIP(tunIPv4)

// findPIDByRemoteTCP finds the process behind a tunnel-captured flow: the
// sockets headed for the same remote endpoint whose traffic entered the TUN.
// One application commonly holds several of them (browser tabs, an IDE's
// helper processes), so the answer is unique when every owner is the same
// EXECUTABLE, not the same pid — the same app's pids collapse into one answer,
// two different apps on one endpoint mean the flow cannot be attributed
// without guessing.
func findPIDByRemoteTCP(ip net.IP, port uint16) (uint32, bool) {
	self := windows.GetCurrentProcessId()
	owners := map[uint32]bool{}
	collect := func(onTun bool) error {
		return tcpOwnerRows(func(lIP net.IP, _ uint16, rPort uint16, rIP net.IP, owner uint32) {
			if rPort != port || !rIP.Equal(ip) {
				return
			}
			if onTun != lIP.Equal(tunLocalAddr) {
				return
			}
			owners[owner] = true
		})
	}
	if err := collect(true); err != nil {
		return 0, false
	}
	if len(owners) == 0 {
		// A socket established before the tunnel came up keeps its physical
		// source address; its packets are still captured by destination.
		if err := collect(false); err != nil {
			return 0, false
		}
	}
	byName := map[string]uint32{}
	haveSelf := false
	for pid := range owners {
		if pid == self {
			haveSelf = true
			continue
		}
		name := processName(pid)
		if name == "" {
			continue
		}
		if _, ok := byName[name]; !ok {
			byName[name] = pid
		}
	}
	if len(byName) == 1 {
		for _, pid := range byName {
			return pid, true
		}
	}
	if len(byName) == 0 && haveSelf {
		// Our own dials (the self-check fetches, the attribution retries'
		// probes) are honestly attributed to this process.
		return self, true
	}
	return 0, false
}

// findPIDByLocalPort finds the owner of an established loopback socket by
// its local port — the system-proxy client path.
func findPIDByLocalPort(port uint16) (uint32, bool) {
	var pid uint32
	seen := map[uint32]bool{}
	err := tcpOwnerRows(func(lIP net.IP, lPort, _ uint16, _ net.IP, owner uint32) {
		if lPort == port && (lIP.IsLoopback() || lIP.IsUnspecified()) && !seen[owner] {
			seen[owner] = true
			pid = owner
		}
	})
	if err != nil || len(seen) != 1 {
		return 0, false
	}
	return pid, true
}

// processImagePath queries the full exe path of a process. Limited query
// rights suffice and work across elevation boundaries on the same user.
func processImagePath(pid uint32) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 1024)
	size := uint32(len(buf))
	r1, _, _ := procQueryImg.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r1 == 0 {
		return "", fmt.Errorf("QueryFullProcessImageName failed")
	}
	return windows.UTF16ToString(buf[:size]), nil
}

// ---- icon extraction ----

type shFileInfoW struct {
	hIcon         uintptr
	iIcon         int32
	dwAttributes  uint32
	szDisplayName [520]uint16
}

type iconInfoBitmaps struct {
	fIcon    uint32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

type bmiHeader struct {
	biSize        uint32
	biWidth       int32
	biHeight      int32 // negative: top-down
	biPlanes      uint16
	biBitCount    uint16
	biCompression uint32
	biSizeImage   uint32
	pad           [16]byte
}

// exeIconDataURL extracts the exe's shell icon and returns it as a PNG data
// URL, cached by the caller. Empty on any failure: the connections page
// then shows the bare process name.
func exeIconDataURL(path string) string {
	defer func() {
		if r := recover(); r != nil {
			loggo.Warn("[GUI] icon extraction panic for %s: %v", path, r)
		}
	}()
	u16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	var sfi shFileInfoW
	r1, _, _ := procSHGetFileIW.Call(uintptr(unsafe.Pointer(u16)), 0,
		uintptr(unsafe.Pointer(&sfi)), uintptr(unsafe.Sizeof(sfi)), shgfiIcon|shgfiLargeIcon)
	if r1 == 0 || sfi.hIcon == 0 {
		return ""
	}
	defer procDeleteObj.Call(sfi.hIcon)

	var ii iconInfoBitmaps
	if r1, _, _ := procGetIconInfo.Call(sfi.hIcon, uintptr(unsafe.Pointer(&ii))); r1 == 0 {
		return ""
	}
	defer func() {
		if ii.hbmMask != 0 {
			procDeleteObj.Call(ii.hbmMask)
		}
		if ii.hbmColor != 0 {
			procDeleteObj.Call(ii.hbmColor)
		}
	}()
	if ii.hbmColor == 0 {
		return ""
	}

	const side = 32
	hdc, _, _ := procCreateDC.Call(0, 0, 0, 0)
	if hdc == 0 {
		return ""
	}
	defer procDeleteDC.Call(hdc)

	bi := bmiHeader{biSize: uint32(unsafe.Sizeof(bmiHeader{})), biWidth: side, biHeight: -side, biPlanes: 1, biBitCount: 32}
	buf := make([]byte, side*side*4)
	r1, _, _ = procGetDIBits.Call(hdc, ii.hbmColor, 0, uintptr(side),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bi)), 0)
	if r1 == 0 {
		return ""
	}

	img := image.NewRGBA(image.Rect(0, 0, side, side))
	allZeroAlpha := true
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			i := (y*side + x) * 4 // top-down BGRA
			a := buf[i+3]
			if a != 0 {
				allZeroAlpha = false
			}
			off := y*img.Stride + x*4
			img.Pix[off+0] = buf[i+2]
			img.Pix[off+1] = buf[i+1]
			img.Pix[off+2] = buf[i+0]
			img.Pix[off+3] = a
		}
	}
	if allZeroAlpha {
		for i := 3; i < len(img.Pix); i += 4 {
			img.Pix[i] = 255
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return ""
	}
	return "data:image/png;base64," + base64Std(out.Bytes())
}

// base64Std is a tiny local encoder: the payload is well-formed 3n bytes
// plus padding, and the table is eight lines.
func base64Std(b []byte) string {
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out []byte
	for i := 0; i < len(b); i += 3 {
		var v uint32
		n := len(b) - i
		v = uint32(b[i]) << 16
		if n > 1 {
			v |= uint32(b[i+1]) << 8
		}
		if n > 2 {
			v |= uint32(b[i+2])
		}
		out = append(out, table[(v>>18)&63], table[(v>>12)&63])
		if n > 1 {
			out = append(out, table[(v>>6)&63])
		} else {
			out = append(out, '=')
		}
		if n > 2 {
			out = append(out, table[v&63])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}
