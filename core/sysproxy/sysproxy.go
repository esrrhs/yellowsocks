package sysproxy

// ProxyMode 系统代理模式
type ProxyMode int

const (
	ModeDirect ProxyMode = iota // 直连
	ModeGlobal                  // 全局系统代理
	ModePAC                     // PAC 智能代理
)
