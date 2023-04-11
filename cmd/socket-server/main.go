package main

import (
	"context"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/util/log"
	frpNet "github.com/fatedier/frp/pkg/util/net"
	"github.com/fatedier/frp/pkg/util/util"
	"github.com/fatedier/frp/pkg/util/version"
	"github.com/fatedier/frp/pkg/util/xlog"
	frpIo "github.com/fatedier/golib/io"
	"github.com/pkg/errors"
	"net"
	"time"
)

const (
	connReadTimeout       time.Duration = 10 * time.Second
	vhostReadWriteTimeout time.Duration = 30 * time.Second
	xTenantId             string        = "x-tenant-id"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:8000")
	if err != nil {
		log.Error("Listener for incoming connections from client closed")
		return
	}

	socketServer := &SocketServer{
		proxyTasks: make(chan *Proxy, 10000),
	}
	socketServer.HandleListener(ln)
}

type Proxy struct {
	tenantId   string
	originConn net.Conn
	frpcConn   net.Conn
}

type SocketServer struct {
	proxyTasks chan *Proxy
}

func (svr *SocketServer) HandleListener(l net.Listener) {
	// Listen for incoming connections from client.
	for {
		originConn, err := l.Accept()
		if err != nil {
			log.Warn("Listener for incoming connections from client closed")
			return
		}
		// inject xlog object into net.Conn context
		xl := xlog.New()
		ctx := context.Background()

		originConn = frpNet.NewContextConn(xlog.NewContext(ctx, xl), originConn)

		// Start a new goroutine to handle connection.
		go func(ctx context.Context, frpConn net.Conn) {
			svr.handleConnection(ctx, frpConn)
		}(ctx, originConn)
	}
}

func (svr *SocketServer) handleConnection(ctx context.Context, conn net.Conn) {
	var (
		rawMsg msg.Message
		err    error
	)

	_ = conn.SetReadDeadline(time.Now().Add(connReadTimeout))
	if rawMsg, err = msg.ReadMsg(conn); err != nil {
		log.Trace("Failed to read message: %v", err)
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	switch m := rawMsg.(type) {
	case *msg.Login:
		tenantId, ok := m.Metas[xTenantId]
		if !ok {
			err = errors.Errorf("%s not found", xTenantId)
			log.Warn("register control error: %v", err)
			_ = msg.WriteMsg(conn, &msg.LoginResp{
				Version: version.Full(),
				Error:   util.GenerateResponseErrorString("register control error", err, false),
			})
			conn.Close()
		}
		// TODO 通过租户ID和frps之间建立连接，直接通过SVC名字拼接出来即可
		frpcConn, err := net.Dial("tcp", "127.0.0.1:7001")
		if err != nil {
			err = errors.Wrapf(err, "dail frps[127.0.0.1:7001] error")
			log.Warn("register control error: %v", err)
			_ = msg.WriteMsg(conn, &msg.LoginResp{
				Version: version.Full(),
				Error:   util.GenerateResponseErrorString("register control error", err, false),
			})
			conn.Close()
		}
		svr.proxyTasks <- &Proxy{tenantId: tenantId, originConn: conn, frpcConn: frpcConn}

	default:
		err = errors.Errorf("%v msg not support", m)
		log.Warn("register control error: %v", err)
		_ = msg.WriteMsg(conn, &msg.LoginResp{
			Version: version.Full(),
			Error:   util.GenerateResponseErrorString("register control error", err, false),
		})
	}
}

func (svr *SocketServer) handleProxyTask() {
	for task := range svr.proxyTasks {
		if _, _, err := frpIo.Join(task.originConn, task.frpcConn); err != nil {
			log.Warn("register control error: %v", err)
		}
	}
}
