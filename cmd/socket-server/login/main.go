package main

import (
	"context"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/util/log"
	frpNet "github.com/fatedier/frp/pkg/util/net"
	"github.com/fatedier/frp/pkg/util/util"
	"github.com/fatedier/frp/pkg/util/version"
	"github.com/fatedier/frp/pkg/util/xlog"
	libdial "github.com/fatedier/golib/net/dial"
	fmux "github.com/hashicorp/yamux"
	"github.com/pkg/errors"
	"io"
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

	log.InitLog("console", "console", "trace", 10, false)

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
			fmuxCfg := fmux.DefaultConfig()
			fmuxCfg.KeepAliveInterval = time.Duration(15) * time.Second
			fmuxCfg.LogOutput = io.Discard
			session, err := fmux.Server(frpConn, fmuxCfg)
			if err != nil {
				log.Warn("Failed to create mux connection: %v", err)
				frpConn.Close()
				return
			}

			for {
				stream, err := session.AcceptStream()
				if err != nil {
					log.Debug("Accept new mux stream error: %v", err)
					session.Close()
					return
				}
				go svr.handleConnection(ctx, stream)
			}
			// TODO 这里必须要使用tcp mux的方式，否则无法从连接中获取到正确的消息
			//go svr.handleConnection(ctx, frpConn)
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
		var dialOptions []libdial.DialOption
		protocol := "tcp"
		dialOptions = append(dialOptions,
			libdial.WithProtocol(protocol),                         // 设置使用的协议
			libdial.WithTimeout(time.Duration(10)*time.Second),     // 设置连接超时时间
			libdial.WithKeepAlive(time.Duration(7200)*time.Second), // TODO 如何理解KeepAlive
			libdial.WithProxy("", ""),                              // 设置frpc的代理
			libdial.WithProxyAuth(nil),                             // 设置代理认证
			libdial.WithTLSConfig(nil),
		)
		// frpc和frps之间建立TCP连接
		frpcConn, err := libdial.Dial(
			net.JoinHostPort("127.0.0.1", "7001"),
			dialOptions...,
		)
		if err != nil {
			err = errors.Wrapf(err, "dail frps[127.0.0.1:7001] error")
			log.Warn("register control error: %v", err)
			_ = msg.WriteMsg(conn, &msg.LoginResp{
				Version: version.Full(),
				Error:   util.GenerateResponseErrorString("register control error", err, false),
			})
			conn.Close()
		}
		log.Info("%s register successful", tenantId)

		// 创建一个基于tcp的io多路复用
		fmuxCfg := fmux.DefaultConfig()
		fmuxCfg.KeepAliveInterval = time.Duration(60) * time.Second
		fmuxCfg.LogOutput = io.Discard
		// 创建session 通过这个session,可以复用连接
		session, err := fmux.Client(frpcConn, fmuxCfg)
		if err != nil {
			log.Warn("tcp mux get session error: %v", err)
		}
		stream, err := session.OpenStream()
		if err != nil {
			log.Warn("tcp mux get stream error: %v", err)
		}

		svr.proxyTasks <- &Proxy{tenantId: tenantId, originConn: conn, frpcConn: stream}

		// 把frpc的登录消息转发给frps
		if err = msg.WriteMsg(stream, m); err != nil {
			err = errors.Wrapf(err, "write msg to frps error")
			log.Warn("proxy login message error: %v", err)
			_ = msg.WriteMsg(conn, &msg.LoginResp{
				Version: version.Full(),
				Error:   util.GenerateResponseErrorString("proxy login message error", err, false),
			})
			frpcConn.Close()
			conn.Close()
		}
	default:
		err = errors.Errorf("%v msg not support", m)
		log.Warn("socket-server not support message type: %v", err)
		_ = msg.WriteMsg(conn, &msg.LoginResp{
			Version: version.Full(),
			Error:   util.GenerateResponseErrorString("socket-server not support message type", err, false),
		})
	}
}

func (svr *SocketServer) handleProxyTask() {
	for task := range svr.proxyTasks {
		task := task
		go func() {
			//if _, _, err := frpIo.Join(task.originConn, task.frpcConn); err != nil {
			//	log.Warn("register control error: %v", err)
			//}
			go io.Copy(task.originConn, task.frpcConn)
			go io.Copy(task.frpcConn, task.originConn)
		}()
	}
}
