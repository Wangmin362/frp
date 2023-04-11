// Copyright 2017 fatedier, fatedier@gmail.com
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"io"
	"net"
	"runtime/debug"
	"time"

	"github.com/fatedier/golib/control/shutdown"
	"github.com/fatedier/golib/crypto"

	"github.com/fatedier/frp/client/proxy"
	"github.com/fatedier/frp/pkg/auth"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/util/xlog"
)

// Control TODO Control是为了干嘛？
type Control struct {
	// uniq id got from frps, attach it in loginMsg
	// TODO runID是为了干嘛？仅仅用于区分不同的frpc，打印日志么
	runID string

	// manage all proxies
	pxyCfgs map[string]config.ProxyConf
	// TODO ProxyManager是如何工作的？
	pm *proxy.Manager

	// manage all visitors
	// TODO VisitorManager是用来干嘛的？
	vm *VisitorManager

	// control connection
	// TODO 如何理解这个连接? 这个连接就是frpc和frps之间的连接么？
	conn net.Conn

	// TODO 连接管理器用于维护frpc和frps之间的TCP连接，并且可以做到IO多路复用
	cm *ConnectionManager

	// put a message in this channel to send it over control connection to server
	// TODO 用于frpc发消息给frps
	sendCh chan msg.Message

	// read from this channel to get the next message sent by server
	// TODO 用于接受frps发送给frpc的消息
	readCh chan msg.Message

	// goroutines can block by reading from this channel, it will be closed only in reader() when control connection is closed
	closedCh chan struct{}

	closedDoneCh chan struct{}

	// last time got the Pong message
	lastPong time.Time

	// The client configuration
	// frpc的配置
	clientCfg config.ClientCommonConf

	readerShutdown     *shutdown.Shutdown
	writerShutdown     *shutdown.Shutdown
	msgHandlerShutdown *shutdown.Shutdown

	// The UDP port that the server is listening on
	// TODO frps为什么会监听这个端口？并且启用了UDP协议？
	serverUDPPort int

	xl *xlog.Logger

	// service context
	ctx context.Context

	// sets authentication based on selected method
	authSetter auth.Setter
}

func NewControl(
	ctx context.Context, runID string, conn net.Conn, cm *ConnectionManager,
	clientCfg config.ClientCommonConf,
	pxyCfgs map[string]config.ProxyConf,
	visitorCfgs map[string]config.VisitorConf,
	serverUDPPort int,
	authSetter auth.Setter,
) *Control {
	// new xlog instance
	ctl := &Control{
		runID:              runID,                       // frpc注册到frps的时候,frps给frpc分配的一个ID
		conn:               conn,                        // frpc和frps之间建立的一个连接
		cm:                 cm,                          // 连接管理器,用于创建frpc和frps之间的连接
		pxyCfgs:            pxyCfgs,                     // 需要代理的服务
		sendCh:             make(chan msg.Message, 100), // TODO
		readCh:             make(chan msg.Message, 100), // TODO
		closedCh:           make(chan struct{}),
		closedDoneCh:       make(chan struct{}),
		clientCfg:          clientCfg,
		readerShutdown:     shutdown.New(),
		writerShutdown:     shutdown.New(),
		msgHandlerShutdown: shutdown.New(),
		serverUDPPort:      serverUDPPort,             // UDP的代理端口
		xl:                 xlog.FromContextSafe(ctx), // 从Context中取出Logger
		ctx:                ctx,
		authSetter:         authSetter,
	}
	// 代理管理器
	ctl.pm = proxy.NewManager(ctl.ctx, ctl.sendCh, clientCfg, serverUDPPort)

	// TODO visitor相关的东西,可以暂时先不管
	ctl.vm = NewVisitorManager(ctl.ctx, ctl)
	ctl.vm.Reload(visitorCfgs)
	return ctl
}

func (ctl *Control) Run() {
	// TODO 主要是处理frpc和frps之间的消息交互
	go ctl.worker()

	// start all proxies
	// TODO 启动所有的代理
	ctl.pm.Reload(ctl.pxyCfgs)

	// start all visitors
	// TODO 暂时先不管
	go ctl.vm.Run()
}

// HandleReqWorkConn TODO 这玩意干嘛的?
func (ctl *Control) HandleReqWorkConn(inMsg *msg.ReqWorkConn) {
	xl := ctl.xl
	workConn, err := ctl.connectServer()
	if err != nil {
		xl.Warn("start new connection to server error: %v", err)
		return
	}

	m := &msg.NewWorkConn{
		RunID: ctl.runID,
	}
	if err = ctl.authSetter.SetNewWorkConn(m); err != nil {
		xl.Warn("error during NewWorkConn authentication: %v", err)
		return
	}
	if err = msg.WriteMsg(workConn, m); err != nil {
		xl.Warn("work connection write to server error: %v", err)
		workConn.Close()
		return
	}

	var startMsg msg.StartWorkConn
	if err = msg.ReadMsgInto(workConn, &startMsg); err != nil {
		xl.Trace("work connection closed before response StartWorkConn message: %v", err)
		workConn.Close()
		return
	}
	if startMsg.Error != "" {
		xl.Error("StartWorkConn contains error: %s", startMsg.Error)
		workConn.Close()
		return
	}

	// dispatch this work connection to related proxy
	ctl.pm.HandleWorkConn(startMsg.ProxyName, workConn, &startMsg)
}

func (ctl *Control) HandleNewProxyResp(inMsg *msg.NewProxyResp) {
	xl := ctl.xl
	// Server will return NewProxyResp message to each NewProxy message.
	// Start a new proxy handler if no error got
	err := ctl.pm.StartProxy(inMsg.ProxyName, inMsg.RemoteAddr, inMsg.Error)
	if err != nil {
		xl.Warn("[%s] start error: %v", inMsg.ProxyName, err)
	} else {
		xl.Info("[%s] start proxy success", inMsg.ProxyName)
	}
}

func (ctl *Control) Close() error {
	return ctl.GracefulClose(0)
}

func (ctl *Control) GracefulClose(d time.Duration) error {
	ctl.pm.Close()
	ctl.vm.Close()

	time.Sleep(d)

	ctl.conn.Close()
	ctl.cm.Close()
	return nil
}

// ClosedDoneCh returns a channel which will be closed after all resources are released
func (ctl *Control) ClosedDoneCh() <-chan struct{} {
	return ctl.closedDoneCh
}

// connectServer return a new connection to frps
func (ctl *Control) connectServer() (conn net.Conn, err error) {
	return ctl.cm.Connect()
}

// reader read all messages from frps and send to readCh
func (ctl *Control) reader() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()
	defer ctl.readerShutdown.Done()
	defer close(ctl.closedCh)

	encReader := crypto.NewReader(ctl.conn, []byte(ctl.clientCfg.Token))
	for {
		// 读取frps发送给frpc消息
		m, err := msg.ReadMsg(encReader)
		if err != nil {
			if err == io.EOF {
				xl.Debug("read from control connection EOF")
				return
			}
			xl.Warn("read error: %v", err)
			ctl.conn.Close()
			return
		}
		ctl.readCh <- m
	}
}

// writer writes messages got from sendCh to frps
func (ctl *Control) writer() {
	xl := ctl.xl
	defer ctl.writerShutdown.Done()
	encWriter, err := crypto.NewWriter(ctl.conn, []byte(ctl.clientCfg.Token))
	if err != nil {
		xl.Error("crypto new writer error: %v", err)
		ctl.conn.Close()
		return
	}
	for {
		m, ok := <-ctl.sendCh
		if !ok {
			xl.Info("control writer is closing")
			return
		}

		if err := msg.WriteMsg(encWriter, m); err != nil {
			xl.Warn("write message to control connection error: %v", err)
			return
		}
	}
}

// msgHandler handles all channel events and do corresponding operations.
func (ctl *Control) msgHandler() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()
	defer ctl.msgHandlerShutdown.Done()

	var hbSendCh <-chan time.Time // 声明变量
	// TODO(fatedier): disable heartbeat if TCPMux is enabled.
	// Just keep it here to keep compatible with old version frps.
	if ctl.clientCfg.HeartbeatInterval > 0 {
		hbSend := time.NewTicker(time.Duration(ctl.clientCfg.HeartbeatInterval) * time.Second)
		defer hbSend.Stop()
		hbSendCh = hbSend.C // 初始化变量
	}

	var hbCheckCh <-chan time.Time
	// Check heartbeat timeout only if TCPMux is not enabled and users don't disable heartbeat feature.
	if ctl.clientCfg.HeartbeatInterval > 0 && ctl.clientCfg.HeartbeatTimeout > 0 && !ctl.clientCfg.TCPMux {
		hbCheck := time.NewTicker(time.Second)
		defer hbCheck.Stop()
		hbCheckCh = hbCheck.C
	}

	ctl.lastPong = time.Now()
	for {
		select {
		case <-hbSendCh: // 默认每30秒一次心跳包
			// TODO frpc发送给frps心跳包
			// send heartbeat to server
			xl.Debug("send heartbeat to server")
			pingMsg := &msg.Ping{}
			if err := ctl.authSetter.SetPing(pingMsg); err != nil {
				xl.Warn("error during ping authentication: %v", err)
				return
			}
			// 发送Ping心跳包给frps
			ctl.sendCh <- pingMsg
		case <-hbCheckCh: // 默认每秒钟检查一次心跳是否正常
			if time.Since(ctl.lastPong) > time.Duration(ctl.clientCfg.HeartbeatTimeout)*time.Second {
				xl.Warn("heartbeat timeout")
				// let reader() stop
				ctl.conn.Close()
				return
			}
		case rawMsg, ok := <-ctl.readCh: // 接收从frps发送的消息
			if !ok { // 说明channel被关闭
				return
			}

			switch m := rawMsg.(type) {
			case *msg.ReqWorkConn:
				// TODO 这个数据包又是干嘛的呢?
				go ctl.HandleReqWorkConn(m)
			case *msg.NewProxyResp:
				// TODO frps啥时候会发送这个数据包呢?
				ctl.HandleNewProxyResp(m)
			case *msg.Pong:
				if m.Error != "" {
					xl.Error("Pong contains error: %s", m.Error)
					ctl.conn.Close()
					return
				}
				// 更新最后一次接收到frps响应心跳的时间
				ctl.lastPong = time.Now()
				xl.Debug("receive heartbeat from server")
			}
		}
	}
}

// If controler is notified by closedCh, reader and writer and handler will exit
func (ctl *Control) worker() {
	// TODO 这里主要做了以下几件事情: 1, 每隔30秒给frps发送一个Ping类型的心跳包 2, 每隔一秒中检查以下frpc和frps之间的心跳是否正常
	// TODO 3. 从readCh channel中读取消息(该消息是frps发送给frpc的消息), 可能是以下三种类型之一:
	// TODO 3.1, msg.ReqWorkConn
	// TODO 3.2, msg.NewProxyResp
	// TODO 3.3, msg.Pong  这个是frps返回的心跳响应
	go ctl.msgHandler()
	// TODO 读取frps发送给frpc的消息,并把消息发送到readCh channel当中
	go ctl.reader()
	// TODO 从writeCh channel中读取消息并发送给frps,实际上就是frpx需要发送给frps的Ping消息
	go ctl.writer()

	<-ctl.closedCh
	// close related channels and wait until other goroutines done
	close(ctl.readCh)
	ctl.readerShutdown.WaitDone()
	ctl.msgHandlerShutdown.WaitDone()

	close(ctl.sendCh)
	ctl.writerShutdown.WaitDone()

	ctl.pm.Close()
	ctl.vm.Close()

	close(ctl.closedDoneCh)
	ctl.cm.Close()
}

func (ctl *Control) ReloadConf(pxyCfgs map[string]config.ProxyConf, visitorCfgs map[string]config.VisitorConf) error {
	ctl.vm.Reload(visitorCfgs)
	ctl.pm.Reload(pxyCfgs)
	return nil
}
