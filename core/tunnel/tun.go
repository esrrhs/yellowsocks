package tunnel

import (
	"io"
)

// Device TUN 虚拟网卡抽象接口
type Device interface {
	io.ReadWriteCloser
	Name() string
}

// TunConfig 虚拟网卡参数
type TunConfig struct {
	Name    string
	IP      string // 如 10.255.0.2
	Gateway string // 如 10.255.0.1
	Mask    string // 如 255.255.255.0
	MTU     int
}
