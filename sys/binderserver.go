package sys

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/inconshreveable/log15"
	"github.com/oklog/ulid"
	"github.com/stephane-martin/skewer/utils"
)

type BinderConn struct {
	Uid  string
	Conn net.Conn
	Addr string
}

type BinderPacketConn struct {
	Uid  string
	Conn net.PacketConn
	Addr string
}

func BinderListen(ctx context.Context, schan chan *BinderConn, generator chan ulid.ULID, addr string) (net.Listener, error) {
	parts := strings.SplitN(addr, ":", 2)
	lnet := parts[0]
	laddr := parts[1]

	l, err := net.Listen(lnet, laddr)

	if err != nil {
		return nil, err
	}

	if lnet == "unix" || lnet == "unixpacket" {
		os.Chmod(laddr, 0777)
	}

	ctx, cancel := context.WithCancel(ctx)

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	go func() {
		for {
			c, err := l.Accept()
			if err == nil {
				uid := <-generator
				schan <- &BinderConn{Uid: uid.String(), Conn: c, Addr: addr}
			} else {
				fmt.Fprintf(os.Stderr, "accept error: %s\n", err.Error())
				cancel()
				return
			}
		}
	}()

	return l, nil
}

func BinderPacket(addr string) (net.PacketConn, error) {
	parts := strings.SplitN(addr, ":", 2)
	lnet := parts[0]
	laddr := parts[1]

	conn, err := net.ListenPacket(lnet, laddr)

	if err != nil {
		return nil, err
	}

	if lnet == "unixgram" {
		os.Chmod(laddr, 0777)
	}

	return conn, nil
}

func Binder(parentFD int) error {
	var msg string
	parentFile := os.NewFile(uintptr(parentFD), "parent_file")
	defer parentFile.Close()

	c, err := net.FileConn(parentFile)
	if err != nil {
		return err
	}
	defer c.Close()

	childConn := c.(*net.UnixConn)
	scanner := bufio.NewScanner(childConn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	generator := utils.Generator(ctx, log15.New())

	schan := make(chan *BinderConn)
	pchan := make(chan *BinderPacketConn)

	go func() {
		connections := map[string]net.Conn{}
		packetconnections := map[string]net.PacketConn{}
		connfiles := map[string]*os.File{}
		for {
			select {
			case <-ctx.Done():
				for _, conn := range packetconnections {
					conn.Close()
				}
				return
			case bc := <-pchan:
				if bc.Conn == nil {
					if len(bc.Uid) == 0 {
						for uid := range packetconnections {
							packetconnections[uid].Close()
							if f, ok := connfiles[uid]; ok {
								f.Close()
								delete(connfiles, uid)
							}
						}
						packetconnections = map[string]net.PacketConn{}
					} else {
						f, ok := connfiles[bc.Uid]
						if ok {
							delete(connfiles, bc.Uid)
							f.Close()
						}
						conn, ok := packetconnections[bc.Uid]
						if ok {
							delete(packetconnections, bc.Uid)
							conn.Close()
						}
					}
				} else {
					lnet := strings.SplitN(bc.Addr, ":", 2)[0]
					var connFile *os.File
					var err error
					if lnet == "unixgram" {
						conn := bc.Conn.(*net.UnixConn)
						connFile, err = conn.File()
					} else {
						conn := bc.Conn.(*net.UDPConn)
						connFile, err = conn.File()
					}
					if err == nil {
						packetconnections[bc.Uid] = bc.Conn
						connfiles[bc.Uid] = connFile
						rights := syscall.UnixRights(int(connFile.Fd()))
						msg := fmt.Sprintf("newconn %s %s\n", bc.Uid, bc.Addr)
						childConn.WriteMsgUnix([]byte(msg), rights, nil)
					}
				}
			case bc := <-schan:
				if bc.Conn == nil {
					if len(bc.Uid) == 0 {
						for uid := range connections {
							connections[uid].Close()
							if f, ok := connfiles[uid]; ok {
								f.Close()
								delete(connfiles, uid)
							}
						}
						connections = map[string]net.Conn{}
					} else {
						f, ok := connfiles[bc.Uid]
						if ok {
							delete(connfiles, bc.Uid)
							f.Close()
						}
						conn, ok := connections[bc.Uid]
						if ok {
							delete(connections, bc.Uid)
							conn.Close()
						}
					}
				} else {
					lnet := strings.SplitN(bc.Addr, ":", 2)[0]
					var connFile *os.File
					var err error
					if lnet == "unix" {
						conn := bc.Conn.(*net.UnixConn)
						connFile, err = conn.File()
					} else {
						conn := bc.Conn.(*net.TCPConn)
						connFile, err = conn.File()
					}
					if err == nil {
						connections[bc.Uid] = bc.Conn
						connfiles[bc.Uid] = connFile
						rights := syscall.UnixRights(int(connFile.Fd()))
						msg := fmt.Sprintf("newconn %s %s\n", bc.Uid, bc.Addr)
						childConn.WriteMsgUnix([]byte(msg), rights, nil)
					}
				}
			}
		}
	}()

	listeners := map[string]net.Listener{}
	for scanner.Scan() {
		msg = strings.Trim(scanner.Text(), " \r\n")
		command := strings.SplitN(msg, " ", 2)[0]
		args := strings.Trim(msg[len(command):], " \r\n")
		fmt.Fprintf(os.Stderr, "parent received: '%s'\n", msg)

		switch command {
		case "listen":
			fmt.Fprintf(os.Stderr, "will listen on: %s\n", args)
			for _, addr := range strings.Split(args, " ") {
				lnet := strings.SplitN(addr, ":", 2)[0]
				if IsStream(lnet) {
					l, err := BinderListen(ctx, schan, generator, addr)
					if err == nil {
						listeners[addr] = l
					} else {
						childConn.Write([]byte(fmt.Sprintf("error %s %s", addr, err.Error())))
					}
				} else {
					c, err := BinderPacket(addr)
					if err == nil {
						uid := <-generator
						pchan <- &BinderPacketConn{Addr: addr, Conn: c, Uid: uid.String()}
					} else {
						fmt.Fprintf(os.Stderr, "ListenPacket error for %s: %s\n", addr, err.Error())
						childConn.Write([]byte(fmt.Sprintf("error %s %s", addr, err.Error())))
					}
				}
			}
		case "closeconn":
			schan <- &BinderConn{Uid: args}
			pchan <- &BinderPacketConn{Uid: args}

		case "stoplisten":
			l, ok := listeners[args]
			if ok {
				l.Close()
				delete(listeners, args)
			}
			childConn.Write([]byte(fmt.Sprintf("stopped %s\n", args)))
		case "reset":
			for _, l := range listeners {
				l.Close()
			}
			listeners = map[string]net.Listener{}
			schan <- &BinderConn{}
			pchan <- &BinderPacketConn{}

		case "byebye":
			return nil

		default:
		}
	}
	err = scanner.Err()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	return nil
}