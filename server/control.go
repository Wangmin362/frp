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

package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/fatedier/golib/control/shutdown"
	"github.com/fatedier/golib/crypto"
	"github.com/fatedier/golib/errors"

	"github.com/fatedier/frp/pkg/auth"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/consts"
	frpErr "github.com/fatedier/frp/pkg/errors"
	"github.com/fatedier/frp/pkg/msg"
	plugin "github.com/fatedier/frp/pkg/plugin/server"
	"github.com/fatedier/frp/pkg/util/util"
	"github.com/fatedier/frp/pkg/util/version"
	"github.com/fatedier/frp/pkg/util/xlog"
	"github.com/fatedier/frp/server/controller"
	"github.com/fatedier/frp/server/metrics"
	"github.com/fatedier/frp/server/proxy"
)

type ControlManager struct {
	// controls indexed by run id
	// TODO key为frps分配给frpc的RunID, Control为每个frpc的控制逻辑
	ctlsByRunID map[string]*Control

	mu sync.RWMutex
}

func NewControlManager() *ControlManager {
	return &ControlManager{
		ctlsByRunID: make(map[string]*Control),
	}
}

func (cm *ControlManager) Add(runID string, ctl *Control) (oldCtl *Control) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	oldCtl, ok := cm.ctlsByRunID[runID]
	if ok {
		oldCtl.Replaced(ctl)
	}
	cm.ctlsByRunID[runID] = ctl
	return
}

// we should make sure if it's the same control to prevent delete a new one
func (cm *ControlManager) Del(runID string, ctl *Control) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if c, ok := cm.ctlsByRunID[runID]; ok && c == ctl {
		delete(cm.ctlsByRunID, runID)
	}
}

func (cm *ControlManager) GetByID(runID string) (ctl *Control, ok bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	ctl, ok = cm.ctlsByRunID[runID]
	return
}

type Control struct {
	// all resource managers and controllers
	// TODO 资源控制器控制着什么资源？是对于什么东西的抽象
	rc *controller.ResourceController

	// proxy manager
	// 代理管理器，代理管理器的代理肯定是由frpc注册上来的
	pxyManager *proxy.Manager

	// plugin manager
	// TODO 插件管理器有哪些？在哪个阶段发生作用？
	pluginManager *plugin.Manager

	// verifies authentication based on selected method
	// 认证器
	authVerifier auth.Verifier

	// login message
	// 每个frpc注册到frps的时候会发送注册消息
	loginMsg *msg.Login

	// control connection
	// frps和frpc之间的连接
	conn net.Conn

	// put a message in this channel to send it over control connection to client
	// 写入到这个channel当中的消息会被发送给frpc
	sendCh chan msg.Message

	// read from this channel to get the next message sent by client
	// frps从这个channel中读取frpc发送的消息
	readCh chan msg.Message

	// work connections
	// 每个frpc还会和frpc之间建立一个工作连接，也就是代理真实数据的连接
	// frps与frpc之间的连接池，大小为 poolCount + 10
	workConnCh chan net.Conn

	// proxies in one client
	// 这里的代理内容肯定是frpc注册上来的
	// TODO 这个属性和ProxyManager有何区别？
	proxies map[string]proxy.Proxy

	// pool count
	// TODO 连接池的大小
	poolCount int

	// ports used, for limitations
	// TODO 端口使用数量，似乎这里会限制frps监听的端口数量
	portsUsedNum int

	// last time got the Ping message
	// frps收到的frpc发送的心跳包的时间，每收到一次心跳包，就会更新这个时间
	lastPing time.Time

	// A new run id will be generated when a new client login.
	// If run id got from login message has same run id, it means it's the same client, so we can
	// replace old controller instantly.
	runID string

	// control status
	status string

	readerShutdown  *shutdown.Shutdown
	writerShutdown  *shutdown.Shutdown
	managerShutdown *shutdown.Shutdown
	allShutdown     *shutdown.Shutdown

	mu sync.RWMutex

	// Server configuration information
	// Server的配置
	serverCfg config.ServerCommonConf

	xl  *xlog.Logger
	ctx context.Context
}

func NewControl(
	ctx context.Context,
	rc *controller.ResourceController,
	pxyManager *proxy.Manager,
	pluginManager *plugin.Manager,
	authVerifier auth.Verifier,
	ctlConn net.Conn,
	loginMsg *msg.Login,
	serverCfg config.ServerCommonConf,
) *Control {
	poolCount := loginMsg.PoolCount
	if poolCount > int(serverCfg.MaxPoolCount) {
		poolCount = int(serverCfg.MaxPoolCount)
	}
	return &Control{
		rc:              rc,
		pxyManager:      pxyManager,
		pluginManager:   pluginManager,
		authVerifier:    authVerifier,
		conn:            ctlConn,
		loginMsg:        loginMsg,
		sendCh:          make(chan msg.Message, 10),
		readCh:          make(chan msg.Message, 10),
		workConnCh:      make(chan net.Conn, poolCount+10),
		proxies:         make(map[string]proxy.Proxy),
		poolCount:       poolCount,
		portsUsedNum:    0,
		lastPing:        time.Now(),
		runID:           loginMsg.RunID,
		status:          consts.Working,
		readerShutdown:  shutdown.New(),
		writerShutdown:  shutdown.New(),
		managerShutdown: shutdown.New(),
		allShutdown:     shutdown.New(),
		serverCfg:       serverCfg,
		xl:              xlog.FromContextSafe(ctx),
		ctx:             ctx,
	}
}

// Start send a login success message to client and start working.
func (ctl *Control) Start() {
	loginRespMsg := &msg.LoginResp{
		Version:       version.Full(),
		RunID:         ctl.runID,
		ServerUDPPort: ctl.serverCfg.BindUDPPort, // frps通过这个端口监听udp协议数据
		Error:         "",
	}
	// Controller启动后，立马回复frpc注册响应
	_ = msg.WriteMsg(ctl.conn, loginRespMsg)

	// 读取sendCh通道中的数据，发送给frpc
	go ctl.writer()
	// 实际上还是建立了多个连接，只不过在开启TCP_MUX的情况下，复用了一个连接
	for i := 0; i < ctl.poolCount; i++ {
		// 根据连接池的配置大小发送ReqWorkConn消息 frpc接收到该消息后，每来一个ReqWorkConn消息，frpc就和frps之间建立一个连接
		// 发消息给frpc，让frpc和自己建立一个连接
		ctl.sendCh <- &msg.ReqWorkConn{}
	}

	// 主要是处理frpc发送给frps的消息
	go ctl.manager()
	// 从连接当中读取frpc发送给frps的消息，并把消息写入到readCh chan当中
	go ctl.reader()
	go ctl.stoper()
}

func (ctl *Control) RegisterWorkConn(conn net.Conn) error {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	select {
	case ctl.workConnCh <- conn:
		xl.Debug("new work connection registered")
		return nil
	default:
		xl.Debug("work connection pool is full, discarding")
		return fmt.Errorf("work connection pool is full, discarding")
	}
}

// When frps get one user connection, we get one work connection from the pool and return it.
// If no workConn available in the pool, send message to frpc to get one or more
// and wait until it is available.
// return an error if wait timeout
func (ctl *Control) GetWorkConn() (workConn net.Conn, err error) {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	var ok bool
	for {
		// get a work connection from the pool
		select {
		// 如果连接池中取不到新的连接，就会进入default
		// TODO 如果连接池中的连接已经被关闭，因此丢弃当前连接
		// TODO 如何检测一个连接已经被关闭
		case workConn, ok = <-ctl.workConnCh:
			if !ok {
				err = frpErr.ErrCtlClosed
				return
			}
			xl.Debug("get work connection from pool, now usable conn poll size is %d", len(ctl.workConnCh))
		default:
			// no work connections available in the poll, send message to frpc to get more
			if err = errors.PanicToError(func() {
				// 没有连接的话就请求建立一个连接
				ctl.sendCh <- &msg.ReqWorkConn{}
			}); err != nil {
				return nil, fmt.Errorf("control is already closed")
			}

			select {
			// fprc收到ReqWorkConn消息之后，一定会和frps建立连接，frps建立好的连接必须要放到workConnCh当中
			case workConn, ok = <-ctl.workConnCh:
				if !ok {
					err = frpErr.ErrCtlClosed
					xl.Warn("no work connections available, %v", err)
					return
				}

			case <-time.After(time.Duration(ctl.serverCfg.UserConnTimeout) * time.Second):
				err = fmt.Errorf("timeout trying to get work connection")
				xl.Warn("%v", err)
				return
			}
		}

		if ctl.serverCfg.TCPMux {
			break
		}

		buf := make([]byte, 1)
		_ = workConn.SetReadDeadline(time.Now().Add(10 * time.Microsecond))
		if _, err := workConn.Read(buf); err != nil {
			inErr, ok := err.(*net.OpError)
			if ok && inErr.Timeout() {
				_ = workConn.SetReadDeadline(time.Time{})
				break
			} else {
				workConn.Close()
			}
		}
		_ = workConn.SetReadDeadline(time.Time{})
	}

	// When we get a work connection from pool, replace it with a new one.
	// 当frps从连接池中取出一个连接之后，会要求frpc重新和frps之间建立一个连接
	_ = errors.PanicToError(func() {
		ctl.sendCh <- &msg.ReqWorkConn{}
	})
	return
}

func (ctl *Control) Replaced(newCtl *Control) {
	xl := ctl.xl
	xl.Info("Replaced by client [%s]", newCtl.runID)
	ctl.runID = ""
	ctl.allShutdown.Start()
}

func (ctl *Control) writer() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.writerShutdown.Done()

	encWriter, err := crypto.NewWriter(ctl.conn, []byte(ctl.serverCfg.Token))
	if err != nil {
		xl.Error("crypto new writer error: %v", err)
		ctl.allShutdown.Start()
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

func (ctl *Control) reader() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.readerShutdown.Done()

	encReader := crypto.NewReader(ctl.conn, []byte(ctl.serverCfg.Token))
	for {
		m, err := msg.ReadMsg(encReader)
		if err != nil {
			if err == io.EOF {
				xl.Debug("control connection closed")
				return
			}
			xl.Warn("read error: %v", err)
			ctl.conn.Close()
			return
		}

		ctl.readCh <- m
	}
}

func (ctl *Control) stoper() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	ctl.allShutdown.WaitStart()

	ctl.conn.Close()
	ctl.readerShutdown.WaitDone()

	close(ctl.readCh)
	ctl.managerShutdown.WaitDone()

	close(ctl.sendCh)
	ctl.writerShutdown.WaitDone()

	ctl.mu.Lock()
	defer ctl.mu.Unlock()

	close(ctl.workConnCh)
	for workConn := range ctl.workConnCh {
		workConn.Close()
	}

	for _, pxy := range ctl.proxies {
		pxy.Close()
		ctl.pxyManager.Del(pxy.GetName())
		metrics.Server.CloseProxy(pxy.GetName(), pxy.GetConf().GetBaseInfo().ProxyType)

		notifyContent := &plugin.CloseProxyContent{
			User: plugin.UserInfo{
				User:  ctl.loginMsg.User,
				Metas: ctl.loginMsg.Metas,
				RunID: ctl.loginMsg.RunID,
			},
			CloseProxy: msg.CloseProxy{
				ProxyName: pxy.GetName(),
			},
		}
		go func() {
			_ = ctl.pluginManager.CloseProxy(notifyContent)
		}()
	}

	ctl.allShutdown.Done()
	xl.Info("client exit success")
	metrics.Server.CloseClient()
}

// block until Control closed
func (ctl *Control) WaitClosed() {
	ctl.allShutdown.WaitDone()
}

func (ctl *Control) manager() {
	xl := ctl.xl
	defer func() {
		if err := recover(); err != nil {
			xl.Error("panic error: %v", err)
			xl.Error(string(debug.Stack()))
		}
	}()

	defer ctl.allShutdown.Start()
	defer ctl.managerShutdown.Done()

	var heartbeatCh <-chan time.Time
	if ctl.serverCfg.TCPMux || ctl.serverCfg.HeartbeatTimeout <= 0 {
		// Don't need application heartbeat here.
		// yamux will do same thing.
	} else {
		// 如果没有使用yamux的话，就需要手动发送心跳包
		heartbeat := time.NewTicker(time.Second)
		defer heartbeat.Stop()
		heartbeatCh = heartbeat.C
	}

	for {
		select {
		case <-heartbeatCh: // 如果启用了tcp_mux，那么由tcp_mux完成心跳，这里就不需要管心跳包了
			// 如果超过90秒都没有收到心跳包，说明连接出现了异常
			if time.Since(ctl.lastPing) > time.Duration(ctl.serverCfg.HeartbeatTimeout)*time.Second {
				xl.Warn("heartbeat timeout")
				return
			}
		case rawMsg, ok := <-ctl.readCh: // 读取frpc发送给frps的消息
			if !ok {
				return
			}

			switch m := rawMsg.(type) {
			case *msg.NewProxy: // frps接收到frpc发送的代理服务数据包，frps接收到后应该需要开启对应的端口进行监听
				content := &plugin.NewProxyContent{
					User: plugin.UserInfo{
						User:  ctl.loginMsg.User,
						Metas: ctl.loginMsg.Metas,
						RunID: ctl.loginMsg.RunID,
					},
					NewProxy: *m,
				}
				var remoteAddr string
				// 添加一个新的代理
				retContent, err := ctl.pluginManager.NewProxy(content)
				if err == nil {
					m = &retContent.NewProxy
					// 注册代理，最主要的步骤就是监听代理所指定的remote_port端口
					remoteAddr, err = ctl.RegisterProxy(m)
				}

				// register proxy in this control
				// 注册完frpc发送过来的代理消息，frps开始监听remote_port相关信息，同时通过控制通道回复frpc NewProxyResp消息
				resp := &msg.NewProxyResp{
					ProxyName: m.ProxyName,
				}
				if err != nil {
					xl.Warn("new proxy [%s] type [%s] error: %v", m.ProxyName, m.ProxyType, err)
					resp.Error = util.GenerateResponseErrorString(fmt.Sprintf("new proxy [%s] error", m.ProxyName), err, ctl.serverCfg.DetailedErrorsToClient)
				} else {
					resp.RemoteAddr = remoteAddr
					xl.Info("new proxy [%s] type [%s] success", m.ProxyName, m.ProxyType)
					metrics.Server.NewProxy(m.ProxyName, m.ProxyType)
				}
				ctl.sendCh <- resp
			case *msg.CloseProxy:
				_ = ctl.CloseProxy(m)
				xl.Info("close proxy [%s] success", m.ProxyName)
			case *msg.Ping:
				// frpc发过来的心跳包
				content := &plugin.PingContent{
					User: plugin.UserInfo{
						User:  ctl.loginMsg.User,
						Metas: ctl.loginMsg.Metas,
						RunID: ctl.loginMsg.RunID,
					},
					Ping: *m,
				}
				retContent, err := ctl.pluginManager.Ping(content)
				if err == nil {
					m = &retContent.Ping
					err = ctl.authVerifier.VerifyPing(m)
				}
				if err != nil {
					xl.Warn("received invalid ping: %v", err)
					ctl.sendCh <- &msg.Pong{
						Error: util.GenerateResponseErrorString("invalid ping", err, ctl.serverCfg.DetailedErrorsToClient),
					}
					return
				}
				ctl.lastPing = time.Now()
				xl.Debug("receive heartbeat")
				ctl.sendCh <- &msg.Pong{}
			}
		}
	}
}

func (ctl *Control) RegisterProxy(pxyMsg *msg.NewProxy) (remoteAddr string, err error) {
	var pxyConf config.ProxyConf
	// Load configures from NewProxy message and check.
	// 根据frpc发送过来的代理消息，通过反射找到对应的代理配置
	pxyConf, err = config.NewProxyConfFromMsg(pxyMsg, ctl.serverCfg)
	if err != nil {
		return
	}

	// User info
	userInfo := plugin.UserInfo{
		User:  ctl.loginMsg.User,
		Metas: ctl.loginMsg.Metas,
		RunID: ctl.runID,
	}

	// NewProxy will return a interface Proxy.
	// In fact it create different proxies by different proxy type, we just call run() here.
	// 根据代理配置实例化新的代理
	pxy, err := proxy.NewProxy(ctl.ctx, userInfo, ctl.rc, ctl.poolCount, ctl.GetWorkConn, pxyConf, ctl.serverCfg, ctl.loginMsg)
	if err != nil {
		return remoteAddr, err
	}

	// Check ports used number in each client
	if ctl.serverCfg.MaxPortsPerClient > 0 {
		ctl.mu.Lock()
		if ctl.portsUsedNum+pxy.GetUsedPortsNum() > int(ctl.serverCfg.MaxPortsPerClient) {
			ctl.mu.Unlock()
			err = fmt.Errorf("exceed the max_ports_per_client")
			return
		}
		ctl.portsUsedNum += pxy.GetUsedPortsNum()
		ctl.mu.Unlock()

		defer func() {
			if err != nil {
				ctl.mu.Lock()
				ctl.portsUsedNum -= pxy.GetUsedPortsNum()
				ctl.mu.Unlock()
			}
		}()
	}

	// 监听代理服务所设置的remote_port端口，等待用户的真实连接
	remoteAddr, err = pxy.Run()
	if err != nil {
		return
	}
	defer func() {
		if err != nil {
			pxy.Close()
		}
	}()

	err = ctl.pxyManager.Add(pxyMsg.ProxyName, pxy)
	if err != nil {
		return
	}

	ctl.mu.Lock()
	ctl.proxies[pxy.GetName()] = pxy
	ctl.mu.Unlock()
	return
}

func (ctl *Control) CloseProxy(closeMsg *msg.CloseProxy) (err error) {
	ctl.mu.Lock()
	pxy, ok := ctl.proxies[closeMsg.ProxyName]
	if !ok {
		ctl.mu.Unlock()
		return
	}

	if ctl.serverCfg.MaxPortsPerClient > 0 {
		ctl.portsUsedNum -= pxy.GetUsedPortsNum()
	}
	pxy.Close()
	ctl.pxyManager.Del(pxy.GetName())
	delete(ctl.proxies, closeMsg.ProxyName)
	ctl.mu.Unlock()

	metrics.Server.CloseProxy(pxy.GetName(), pxy.GetConf().GetBaseInfo().ProxyType)

	notifyContent := &plugin.CloseProxyContent{
		User: plugin.UserInfo{
			User:  ctl.loginMsg.User,
			Metas: ctl.loginMsg.Metas,
			RunID: ctl.loginMsg.RunID,
		},
		CloseProxy: msg.CloseProxy{
			ProxyName: pxy.GetName(),
		},
	}
	go func() {
		_ = ctl.pluginManager.CloseProxy(notifyContent)
	}()

	return
}
