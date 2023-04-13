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
	"crypto/tls"
	"fmt"
	"io"
	"math/rand"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatedier/golib/crypto"
	libdial "github.com/fatedier/golib/net/dial"
	fmux "github.com/hashicorp/yamux"
	quic "github.com/quic-go/quic-go"

	"github.com/fatedier/frp/assets"
	"github.com/fatedier/frp/pkg/auth"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/transport"
	"github.com/fatedier/frp/pkg/util/log"
	frpNet "github.com/fatedier/frp/pkg/util/net"
	"github.com/fatedier/frp/pkg/util/util"
	"github.com/fatedier/frp/pkg/util/version"
	"github.com/fatedier/frp/pkg/util/xlog"
)

func init() {
	crypto.DefaultSalt = "frp"
	// TODO: remove this when we drop support for go1.19
	rand.Seed(time.Now().UnixNano())
}

// Service is a client service.
type Service struct {
	// uniq id got from frps, attach it in loginMsg
	runID string

	// manager control connection with server
	ctl   *Control
	ctlMu sync.RWMutex

	// Sets authentication based on selected method
	authSetter auth.Setter

	cfg         config.ClientCommonConf
	pxyCfgs     map[string]config.ProxyConf
	visitorCfgs map[string]config.VisitorConf
	cfgMu       sync.RWMutex

	// The configuration file used to initialize this client, or an empty
	// string if no configuration file was used.
	cfgFile string

	// This is configured by the login response from frps
	serverUDPPort int

	exit uint32 // 0 means not exit

	// service context
	ctx context.Context
	// call cancel to stop service
	cancel context.CancelFunc
}

func NewService(
	cfg config.ClientCommonConf,
	pxyCfgs map[string]config.ProxyConf,
	visitorCfgs map[string]config.VisitorConf,
	cfgFile string,
) (svr *Service, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	svr = &Service{
		authSetter:  auth.NewAuthSetter(cfg.ClientConfig),
		cfg:         cfg,
		cfgFile:     cfgFile,
		pxyCfgs:     pxyCfgs,
		visitorCfgs: visitorCfgs,
		exit:        0,
		ctx:         xlog.NewContext(ctx, xlog.New()),
		cancel:      cancel,
	}
	return
}

func (svr *Service) GetController() *Control {
	svr.ctlMu.RLock()
	defer svr.ctlMu.RUnlock()
	return svr.ctl
}

func (svr *Service) Run() error {
	// TODO logger参数什么时候放到context当中的?
	xl := xlog.FromContextSafe(svr.ctx)

	// set custom DNSServer  使用用户指定的DNS
	if svr.cfg.DNSServer != "" {
		dnsAddr := svr.cfg.DNSServer
		if _, _, err := net.SplitHostPort(dnsAddr); err != nil {
			dnsAddr = net.JoinHostPort(dnsAddr, "53")
		}
		// Change default dns server for frpc
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return net.Dial("udp", dnsAddr)
			},
		}
	}

	// login to frps
	for {
		// frp client启动时，首先就需要注册到frp server,在注册的同时frpc和frps之间建立了一个IO多路复用的Socket
		conn, cm, err := svr.login()
		if err != nil {
			xl.Warn("login to server failed: %v", err)

			// if login_fail_exit is true, just exit this program
			// otherwise sleep a while and try again to connect to server
			if svr.cfg.LoginFailExit {
				return err
			}
			util.RandomSleep(10*time.Second, 0.9, 1.1)
		} else {
			// login success
			ctl := NewControl(svr.ctx, svr.runID, conn, cm, svr.cfg, svr.pxyCfgs, svr.visitorCfgs, svr.serverUDPPort, svr.authSetter)
			// Controller中实现了真正的代理业务
			ctl.Run()
			svr.ctlMu.Lock()
			svr.ctl = ctl
			svr.ctlMu.Unlock()
			break
		}
	}

	// 如果frpc和frps之间的连接断开了,这里会尝试重新建立连接
	// TODO 这里的逻辑和上面逻辑很类似，能否优化这一块代码
	go svr.keepControllerWorking()

	// 开启HTTP服务，当启动frpc的web界面时，web界面可以通过API接口查询相应的数据
	if svr.cfg.AdminPort != 0 {
		// Init admin server assets
		assets.Load(svr.cfg.AssetsDir)

		address := net.JoinHostPort(svr.cfg.AdminAddr, strconv.Itoa(svr.cfg.AdminPort))
		err := svr.RunAdminServer(address)
		if err != nil {
			log.Warn("run admin server error: %v", err)
		}
		log.Info("admin server listen on %s:%d", svr.cfg.AdminAddr, svr.cfg.AdminPort)
	}
	<-svr.ctx.Done()
	return nil
}

// keepControllerWorking TODO 这个函数主要是在干嘛？
func (svr *Service) keepControllerWorking() {
	// 从context中取出logger
	xl := xlog.FromContextSafe(svr.ctx)
	maxDelayTime := 20 * time.Second
	delayTime := time.Second

	// if frpc reconnect frps, we need to limit retry times in 1min
	// current retry logic is sleep 0s, 0s, 0s, 1s, 2s, 4s, 8s, ...
	// when exceed 1min, we will reset delay and counts
	cutoffTime := time.Now().Add(time.Minute)
	reconnectDelay := time.Second
	reconnectCounts := 1

	for {
		// 这个channel啥时候会被关闭?
		<-svr.ctl.ClosedDoneCh()
		if atomic.LoadUint32(&svr.exit) != 0 {
			return
		}

		// the first three retry with no delay
		if reconnectCounts > 3 {
			util.RandomSleep(reconnectDelay, 0.9, 1.1)
			xl.Info("wait %v to reconnect", reconnectDelay)
			reconnectDelay *= 2
		} else {
			util.RandomSleep(time.Second, 0, 0.5)
		}
		reconnectCounts++

		now := time.Now()
		if now.After(cutoffTime) {
			// reset
			cutoffTime = now.Add(time.Minute)
			reconnectDelay = time.Second
			reconnectCounts = 1
		}

		for {
			if atomic.LoadUint32(&svr.exit) != 0 {
				return
			}

			xl.Info("try to reconnect to server...")
			conn, cm, err := svr.login()
			if err != nil {
				xl.Warn("reconnect to server error: %v, wait %v for another retry", err, delayTime)
				util.RandomSleep(delayTime, 0.9, 1.1)

				delayTime *= 2
				if delayTime > maxDelayTime {
					delayTime = maxDelayTime
				}
				continue
			}
			// reconnect success, init delayTime
			delayTime = time.Second

			ctl := NewControl(svr.ctx, svr.runID, conn, cm, svr.cfg, svr.pxyCfgs, svr.visitorCfgs, svr.serverUDPPort, svr.authSetter)
			ctl.Run()
			svr.ctlMu.Lock()
			if svr.ctl != nil {
				svr.ctl.Close()
			}
			svr.ctl = ctl
			svr.ctlMu.Unlock()
			break
		}
	}
}

// login creates a connection to frps and registers it self as a client
// conn: control connection
// session: if it's not nil, using tcp mux
func (svr *Service) login() (conn net.Conn, cm *ConnectionManager, err error) {
	xl := xlog.FromContextSafe(svr.ctx)
	// TODO 连接管理器是如何工作的？ 答：连接管理器主要是为了能够复用frpc到frps之间的连接，并且能够做到IO多路复用
	cm = NewConnectionManager(svr.ctx, &svr.cfg)

	// 建立frpc和frps之间的连接
	if err = cm.OpenConnection(); err != nil {
		return nil, nil, err
	}

	defer func() {
		if err != nil {
			cm.Close()
		}
	}()

	// 从连接管理器中取出生成一个连接，这个连接肯定和其它连接可以通过内部的数据结构区分
	conn, err = cm.Connect()
	if err != nil {
		return
	}

	loginMsg := &msg.Login{
		Arch:      runtime.GOARCH,
		Os:        runtime.GOOS,
		PoolCount: svr.cfg.PoolCount,
		User:      svr.cfg.User,
		Version:   version.Full(),
		Timestamp: time.Now().Unix(),
		RunID:     svr.runID,
		Metas:     svr.cfg.Metas, // meta参数可以传递业务相关的参数
	}

	// Add auth 设置认证器,发送认证相关的参数,目前看来frp认证可以采用token也可采用oidc
	// TODO TLS是干嘛用的? frpc和frps之间可以通过TLS进行双向认证么?
	if err = svr.authSetter.SetLogin(loginMsg); err != nil {
		return
	}

	// 向frps发送注册信息， frps会根据客户端发送的注册信息认证frpc
	if err = msg.WriteMsg(conn, loginMsg); err != nil {
		return
	}

	var loginRespMsg msg.LoginResp
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	// 读取frps返回的认证响应消息
	if err = msg.ReadMsgInto(conn, &loginRespMsg); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	if loginRespMsg.Error != "" {
		err = fmt.Errorf("%s", loginRespMsg.Error)
		xl.Error("%s", loginRespMsg.Error)
		return
	}

	svr.runID = loginRespMsg.RunID
	xl.ResetPrefixes()
	// 日志前缀全部是用当前frpc注册好的ID
	xl.AppendPrefix(svr.runID)

	// frp既可以代理TCP流量,也可以代理UDP流量
	svr.serverUDPPort = loginRespMsg.ServerUDPPort
	xl.Info("login to server success, get run id [%s], server udp port [%d]", loginRespMsg.RunID, loginRespMsg.ServerUDPPort)
	return
}

func (svr *Service) ReloadConf(pxyCfgs map[string]config.ProxyConf, visitorCfgs map[string]config.VisitorConf) error {
	svr.cfgMu.Lock()
	svr.pxyCfgs = pxyCfgs
	svr.visitorCfgs = visitorCfgs
	svr.cfgMu.Unlock()

	svr.ctlMu.RLock()
	ctl := svr.ctl
	svr.ctlMu.RUnlock()

	if ctl != nil {
		return svr.ctl.ReloadConf(pxyCfgs, visitorCfgs)
	}
	return nil
}

func (svr *Service) Close() {
	svr.GracefulClose(time.Duration(0))
}

func (svr *Service) GracefulClose(d time.Duration) {
	atomic.StoreUint32(&svr.exit, 1)

	svr.ctlMu.RLock()
	if svr.ctl != nil {
		svr.ctl.GracefulClose(d)
	}
	svr.ctlMu.RUnlock()

	svr.cancel()
}

// ConnectionManager TODO 是为了和frp server之间的多路复用  原理是啥？
type ConnectionManager struct {
	ctx context.Context
	// frp client的配置
	cfg *config.ClientCommonConf

	muxSession *fmux.Session
	quicConn   quic.Connection
}

func NewConnectionManager(ctx context.Context, cfg *config.ClientCommonConf) *ConnectionManager {
	return &ConnectionManager{
		ctx: ctx,
		cfg: cfg,
	}
}

// OpenConnection 用于在frpc和frps之间建立一个连接，这个连接可以做到IO多路复用
func (cm *ConnectionManager) OpenConnection() error {
	xl := xlog.FromContextSafe(cm.ctx)

	// special for quic quic连接本身就是多路复用的，只需要建立就好，不需要额外处理
	if strings.EqualFold(cm.cfg.Protocol, "quic") {
		var tlsConfig *tls.Config
		var err error
		sn := cm.cfg.TLSServerName
		if sn == "" {
			sn = cm.cfg.ServerAddr
		}
		if cm.cfg.TLSEnable {
			tlsConfig, err = transport.NewClientTLSConfig(
				cm.cfg.TLSCertFile,
				cm.cfg.TLSKeyFile,
				cm.cfg.TLSTrustedCaFile,
				sn)
		} else {
			tlsConfig, err = transport.NewClientTLSConfig("", "", "", sn)
		}
		if err != nil {
			xl.Warn("fail to build tls configuration, err: %v", err)
			return err
		}
		tlsConfig.NextProtos = []string{"frp"}

		conn, err := quic.DialAddr(
			net.JoinHostPort(cm.cfg.ServerAddr, strconv.Itoa(cm.cfg.ServerPort)),
			tlsConfig, &quic.Config{
				MaxIdleTimeout:     time.Duration(cm.cfg.QUICMaxIdleTimeout) * time.Second,
				MaxIncomingStreams: int64(cm.cfg.QUICMaxIncomingStreams),
				KeepAlivePeriod:    time.Duration(cm.cfg.QUICKeepalivePeriod) * time.Second,
			})
		if err != nil {
			return err
		}
		cm.quicConn = conn
		return nil
	}

	if !cm.cfg.TCPMux {
		return nil
	}

	// frpc和frps之间建立一个tcp连接
	conn, err := cm.realConnect()
	if err != nil {
		return err
	}

	// 创建一个基于tcp的io多路复用
	fmuxCfg := fmux.DefaultConfig()
	fmuxCfg.KeepAliveInterval = time.Duration(cm.cfg.TCPMuxKeepaliveInterval) * time.Second
	fmuxCfg.LogOutput = io.Discard
	// 创建session 通过这个session,可以复用连接
	session, err := fmux.Client(conn, fmuxCfg)
	if err != nil {
		return err
	}
	cm.muxSession = session
	return nil
}

func (cm *ConnectionManager) Connect() (net.Conn, error) {
	if cm.quicConn != nil {
		stream, err := cm.quicConn.OpenStreamSync(context.Background())
		if err != nil {
			return nil, err
		}
		return frpNet.QuicStreamToNetConn(stream, cm.quicConn), nil
	} else if cm.muxSession != nil {
		// 类似于HTTP2中的frame，可以认为就是一个连接，只不过是公用的连接
		stream, err := cm.muxSession.OpenStream()
		if err != nil {
			return nil, err
		}
		return stream, nil
	}

	return cm.realConnect()
}

func (cm *ConnectionManager) realConnect() (net.Conn, error) {
	// 从Context中取出Logger实例，用法非常类似java中的ThreadLocalMap
	xl := xlog.FromContextSafe(cm.ctx)
	var tlsConfig *tls.Config
	var err error
	if cm.cfg.TLSEnable { // frp client和frp server之间采用TLS认证
		sn := cm.cfg.TLSServerName
		if sn == "" { // 如果Frp Server没有指定域名，就直接使用IP地址 TODO 这一块和证书相关
			sn = cm.cfg.ServerAddr
		}

		// 实例化TLS配置
		tlsConfig, err = transport.NewClientTLSConfig(
			cm.cfg.TLSCertFile,
			cm.cfg.TLSKeyFile,
			cm.cfg.TLSTrustedCaFile,
			sn)
		if err != nil {
			xl.Warn("fail to build tls configuration, err: %v", err)
			return nil, err
		}
	}

	// 解析frp client的代理
	proxyType, addr, auth, err := libdial.ParseProxyURL(cm.cfg.HTTPProxy)
	if err != nil {
		xl.Error("fail to parse proxy url")
		return nil, err
	}
	var dialOptions []libdial.DialOption
	protocol := cm.cfg.Protocol
	if protocol == "websocket" {
		protocol = "tcp" // 本质上websocket协议就是tcp协议
		dialOptions = append(dialOptions, libdial.WithAfterHook(libdial.AfterHook{Hook: frpNet.DialHookWebsocket()}))
	}
	if cm.cfg.ConnectServerLocalIP != "" {
		dialOptions = append(dialOptions, libdial.WithLocalAddr(cm.cfg.ConnectServerLocalIP))
	}
	dialOptions = append(dialOptions,
		libdial.WithProtocol(protocol),                                               // 设置使用的协议
		libdial.WithTimeout(time.Duration(cm.cfg.DialServerTimeout)*time.Second),     // 设置连接超时时间
		libdial.WithKeepAlive(time.Duration(cm.cfg.DialServerKeepAlive)*time.Second), // TODO 如何理解KeepAlive
		libdial.WithProxy(proxyType, addr),                                           // 设置frpc的代理
		libdial.WithProxyAuth(auth),                                                  // 设置代理认证
		libdial.WithTLSConfig(tlsConfig),                                             // 设置 TLS
		libdial.WithAfterHook(libdial.AfterHook{
			Hook: frpNet.DialHookCustomTLSHeadByte(tlsConfig != nil, cm.cfg.DisableCustomTLSFirstByte),
		}),
	)
	// frpc和frps之间建立TCP连接
	conn, err := libdial.Dial(
		net.JoinHostPort(cm.cfg.ServerAddr, strconv.Itoa(cm.cfg.ServerPort)),
		dialOptions...,
	)
	return conn, err
}

func (cm *ConnectionManager) Close() error {
	if cm.quicConn != nil {
		_ = cm.quicConn.CloseWithError(0, "")
	}
	if cm.muxSession != nil {
		_ = cm.muxSession.Close()
	}
	return nil
}
