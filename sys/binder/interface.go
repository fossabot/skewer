package binder

import (
	"net"
	"time"
)

type Client interface {
	Listen(lnet string, laddr string) (net.Listener, error)
	ListenKeepAlive(lnet string, laddr string, period time.Duration) (net.Listener, error)
	ListenPacket(lnet string, laddr string, bytes int) (net.PacketConn, error)
	StopListen(addr string) error
	Quit() error
}
