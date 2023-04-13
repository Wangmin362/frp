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
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/fatedier/golib/net/mux"
	fmux "github.com/hashicorp/yamux"
	quic "github.com/quic-go/quic-go"

	"github.com/fatedier/frp/assets"
	"github.com/fatedier/frp/pkg/auth"
	"github.com/fatedier/frp/pkg/config"
	modelmetrics "github.com/fatedier/frp/pkg/metrics"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/nathole"
	plugin "github.com/fatedier/frp/pkg/plugin/server"
	"github.com/fatedier/frp/pkg/transport"
	"github.com/fatedier/frp/pkg/util/log"
	frpNet "github.com/fatedier/frp/pkg/util/net"
	"github.com/fatedier/frp/pkg/util/tcpmux"
	"github.com/fatedier/frp/pkg/util/util"
	"github.com/fatedier/frp/pkg/util/version"
	"github.com/fatedier/frp/pkg/util/vhost"
	"github.com/fatedier/frp/pkg/util/xlog"
	"github.com/fatedier/frp/server/controller"
	"github.com/fatedier/frp/server/group"
	"github.com/fatedier/frp/server/metrics"
	"github.com/fatedier/frp/server/ports"
	"github.com/fatedier/frp/server/proxy"
	"github.com/fatedier/frp/server/visitor"
)

const (
	connReadTimeout       time.Duration = 10 * time.Second
	vhostReadWriteTimeout time.Duration = 30 * time.Second
)

// Service Server service
type Service struct {
	// Dispatch connections to different handlers listen on same port
	// TODO frps是如何区分不同的frpc的呢，答案应该就是这个
	muxer *mux.Mux

	// Accept connections from client
	// 监听bind_addr:bind_port地址，等待客户端的连接
	listener net.Listener

	// Accept connections using kcp
	kcpListener net.Listener

	// Accept connections using quic
	quicListener quic.Listener

	// Accept connections using websocket
	websocketListener net.Listener

	// Accept frp tls connections
	tlsListener net.Listener

	// Manage all controllers
	ctlManager *ControlManager

	// Manage all proxies
	pxyManager *proxy.Manager

	// Manage all plugins
	pluginManager *plugin.Manager

	// HTTP vhost router
	httpVhostRouter *vhost.Routers

	// All resource managers and controllers
	rc *controller.ResourceController

	// Verifies authentication based on selected method
	authVerifier auth.Verifier

	tlsConfig *tls.Config

	cfg config.ServerCommonConf
}

func NewService(cfg config.ServerCommonConf) (svr *Service, err error) {
	// 构建TLS配置，如果没有配置证书，frp会自己生成证书
	tlsConfig, err := transport.NewServerTLSConfig(
		cfg.TLSCertFile,
		cfg.TLSKeyFile,
		cfg.TLSTrustedCaFile)
	if err != nil {
		return
	}

	svr = &Service{
		// ControlManager并不难，实际上就是一个Map,key为frpc的RunID,value为每个frpc的控制逻辑
		ctlManager: NewControlManager(),
		// 代理管理器，实际上也是一个map, key为代理名字，value为代理的配置内容
		pxyManager: proxy.NewManager(),
		// TODO 插件管理器 目前似乎只有HTTP插件 插件的执行发生在哪个生命周期
		pluginManager: plugin.NewManager(),
		// TODO 资源管理器
		rc: &controller.ResourceController{
			VisitorManager: visitor.NewManager(),
			// 用于管理端口，并判断某个端口当前是否可用
			TCPPortManager: ports.NewManager("tcp", cfg.ProxyBindAddr, cfg.AllowPorts),
			UDPPortManager: ports.NewManager("udp", cfg.ProxyBindAddr, cfg.AllowPorts),
		},
		// TODO HTTP路由 如果是HTTPS呢？ 也是使用这个路由么？
		httpVhostRouter: vhost.NewRouters(),
		// TODO 认证器
		authVerifier: auth.NewAuthVerifier(cfg.ServerConfig),
		tlsConfig:    tlsConfig,
		cfg:          cfg,
	}

	// Create tcpmux httpconnect multiplexer.
	// TODO TCP多路复用器,它是如何工作的？ 答：本质上是通过先发送HTTP CONNECT方法建立的隧道，然后在建立好的隧道之上传输数据
	if cfg.TCPMuxHTTPConnectPort > 0 {
		var l net.Listener
		address := net.JoinHostPort(cfg.ProxyBindAddr, strconv.Itoa(cfg.TCPMuxHTTPConnectPort))
		l, err = net.Listen("tcp", address)
		if err != nil {
			err = fmt.Errorf("create server listener error, %v", err)
			return
		}

		svr.rc.TCPMuxHTTPConnectMuxer, err = tcpmux.NewHTTPConnectTCPMuxer(l, cfg.TCPMuxPassthrough, vhostReadWriteTimeout)
		if err != nil {
			err = fmt.Errorf("create vhost tcpMuxer error, %v", err)
			return
		}
		log.Info("tcpmux httpconnect multiplexer listen on %s, passthough: %v", address, cfg.TCPMuxPassthrough)
	}

	// Init all plugins TODO 插件管理器这一块的内容暂时用不到
	pluginNames := make([]string, 0, len(cfg.HTTPPlugins))
	for n := range cfg.HTTPPlugins {
		pluginNames = append(pluginNames, n)
	}
	sort.Strings(pluginNames)

	for _, name := range pluginNames {
		svr.pluginManager.Register(plugin.NewHTTPPluginOptions(cfg.HTTPPlugins[name]))
		log.Info("plugin [%s] has been registered", name)
	}
	svr.rc.PluginManager = svr.pluginManager

	// Init group controller
	// TCP服务的负载均衡
	svr.rc.TCPGroupCtl = group.NewTCPGroupCtl(svr.rc.TCPPortManager)

	// Init HTTP group controller
	// HTTP服务负载均衡
	svr.rc.HTTPGroupCtl = group.NewHTTPGroupController(svr.httpVhostRouter)

	// Init TCP mux group controller
	// TCPMUX类型的负载均衡
	svr.rc.TCPMuxGroupCtl = group.NewTCPMuxGroupCtl(svr.rc.TCPMuxHTTPConnectMuxer)

	// Init 404 not found page
	vhost.NotFoundPagePath = cfg.Custom404Page

	var (
		httpMuxOn  bool
		httpsMuxOn bool
	)
	if cfg.BindAddr == cfg.ProxyBindAddr {
		if cfg.BindPort == cfg.VhostHTTPPort {
			httpMuxOn = true
		}
		if cfg.BindPort == cfg.VhostHTTPSPort {
			httpsMuxOn = true
		}
	}

	// Listen for accepting connections from client.
	// 监听frps配置的端口
	address := net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.BindPort))
	ln, err := net.Listen("tcp", address)
	if err != nil {
		err = fmt.Errorf("create server listener error, %v", err)
		return
	}

	svr.muxer = mux.NewMux(ln)
	svr.muxer.SetKeepAlive(time.Duration(cfg.TCPKeepAlive) * time.Second)
	go func() {
		// frps开始监听
		_ = svr.muxer.Serve()
	}()
	ln = svr.muxer.DefaultListener()

	svr.listener = ln
	log.Info("frps tcp listen on %s", address)

	// Listen for accepting connections from client using kcp protocol.
	if cfg.KCPBindPort > 0 {
		address := net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.KCPBindPort))
		svr.kcpListener, err = frpNet.ListenKcp(address)
		if err != nil {
			err = fmt.Errorf("listen on kcp udp address %s error: %v", address, err)
			return
		}
		log.Info("frps kcp listen on udp %s", address)
	}

	if cfg.QUICBindPort > 0 {
		address := net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.QUICBindPort))
		quicTLSCfg := tlsConfig.Clone()
		quicTLSCfg.NextProtos = []string{"frp"}
		svr.quicListener, err = quic.ListenAddr(address, quicTLSCfg, &quic.Config{
			MaxIdleTimeout:     time.Duration(cfg.QUICMaxIdleTimeout) * time.Second,
			MaxIncomingStreams: int64(cfg.QUICMaxIncomingStreams),
			KeepAlivePeriod:    time.Duration(cfg.QUICKeepalivePeriod) * time.Second,
		})
		if err != nil {
			err = fmt.Errorf("listen on quic udp address %s error: %v", address, err)
			return
		}
		log.Info("frps quic listen on quic %s", address)
	}

	// Listen for accepting connections from client using websocket protocol.
	websocketPrefix := []byte("GET " + frpNet.FrpWebsocketPath)
	websocketLn := svr.muxer.Listen(0, uint32(len(websocketPrefix)), func(data []byte) bool {
		return bytes.Equal(data, websocketPrefix)
	})
	svr.websocketListener = frpNet.NewWebsocketListener(websocketLn)

	// Create http vhost muxer.
	if cfg.VhostHTTPPort > 0 {
		rp := vhost.NewHTTPReverseProxy(vhost.HTTPReverseProxyOptions{
			ResponseHeaderTimeoutS: cfg.VhostHTTPTimeout,
		}, svr.httpVhostRouter)
		svr.rc.HTTPReverseProxy = rp

		address := net.JoinHostPort(cfg.ProxyBindAddr, strconv.Itoa(cfg.VhostHTTPPort))
		server := &http.Server{
			Addr:    address,
			Handler: rp,
		}
		var l net.Listener
		if httpMuxOn {
			l = svr.muxer.ListenHttp(1)
		} else {
			l, err = net.Listen("tcp", address)
			if err != nil {
				err = fmt.Errorf("create vhost http listener error, %v", err)
				return
			}
		}
		go func() {
			_ = server.Serve(l)
		}()
		log.Info("http service listen on %s", address)
	}

	// Create https vhost muxer.
	if cfg.VhostHTTPSPort > 0 {
		var l net.Listener
		if httpsMuxOn {
			l = svr.muxer.ListenHttps(1)
		} else {
			address := net.JoinHostPort(cfg.ProxyBindAddr, strconv.Itoa(cfg.VhostHTTPSPort))
			l, err = net.Listen("tcp", address)
			if err != nil {
				err = fmt.Errorf("create server listener error, %v", err)
				return
			}
			log.Info("https service listen on %s", address)
		}

		svr.rc.VhostHTTPSMuxer, err = vhost.NewHTTPSMuxer(l, vhostReadWriteTimeout)
		if err != nil {
			err = fmt.Errorf("create vhost httpsMuxer error, %v", err)
			return
		}
	}

	// frp tls listener
	// TODO 支持HTTPS
	svr.tlsListener = svr.muxer.Listen(2, 1, func(data []byte) bool {
		// tls first byte can be 0x16 only when vhost https port is not same with bind port
		return int(data[0]) == frpNet.FRPTLSHeadByte || int(data[0]) == 0x16
	})

	// Create nat hole controller.
	if cfg.BindUDPPort > 0 {
		var nc *nathole.Controller
		address := net.JoinHostPort(cfg.BindAddr, strconv.Itoa(cfg.BindUDPPort))
		nc, err = nathole.NewController(address, []byte(cfg.Token))
		if err != nil {
			err = fmt.Errorf("create nat hole controller error, %v", err)
			return
		}
		svr.rc.NatHoleController = nc
		log.Info("nat hole udp service listen on %s", address)
	}

	var statsEnable bool
	// Create dashboard web server.
	if cfg.DashboardPort > 0 {
		// Init dashboard assets
		assets.Load(cfg.AssetsDir)

		address := net.JoinHostPort(cfg.DashboardAddr, strconv.Itoa(cfg.DashboardPort))
		err = svr.RunDashboardServer(address)
		if err != nil {
			err = fmt.Errorf("create dashboard web server error, %v", err)
			return
		}
		log.Info("Dashboard listen on %s", address)
		statsEnable = true
	}
	if statsEnable {
		modelmetrics.EnableMem()
		if cfg.EnablePrometheus {
			modelmetrics.EnablePrometheus()
		}
	}
	return
}

func (svr *Service) Run() {
	if svr.rc.NatHoleController != nil {
		// 当用户指定了bind_udp_port参数时，就会通过NatHoleController来处理
		go svr.rc.NatHoleController.Run()
	}
	if svr.kcpListener != nil {
		go svr.HandleListener(svr.kcpListener)
	}
	if svr.quicListener != nil {
		go svr.HandleQUICListener(svr.quicListener)
	}

	go svr.HandleListener(svr.websocketListener)
	go svr.HandleListener(svr.tlsListener)

	// 基于TCP的监听
	svr.HandleListener(svr.listener)
}

func (svr *Service) handleConnection(ctx context.Context, conn net.Conn) {
	xl := xlog.FromContextSafe(ctx)

	var (
		rawMsg msg.Message
		err    error
	)

	_ = conn.SetReadDeadline(time.Now().Add(connReadTimeout))
	// 读取frpc发送过来的消息
	if rawMsg, err = msg.ReadMsg(conn); err != nil {
		log.Trace("Failed to read message: %v", err)
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	switch m := rawMsg.(type) {
	case *msg.Login:
		// server plugin hook
		content := &plugin.LoginContent{
			Login:         *m,
			ClientAddress: conn.RemoteAddr().String(),
		}
		// 默认情况下是不需要进行认证的，因为没有配置相关认证参数
		retContent, err := svr.pluginManager.Login(content)
		if err == nil {
			m = &retContent.Login
			// 每一个frpc认证通过之后，都会生成一个Controller负责与frpc交互
			err = svr.RegisterControl(conn, m)
		}

		// If login failed, send error message there.
		// Otherwise send success message in control's work goroutine.
		if err != nil {
			xl.Warn("register control error: %v", err)
			_ = msg.WriteMsg(conn, &msg.LoginResp{
				Version: version.Full(),
				Error:   util.GenerateResponseErrorString("register control error", err, svr.cfg.DetailedErrorsToClient),
			})
			conn.Close() // 注册失败的话关闭frpc和frps之间的连接
		}
	case *msg.NewWorkConn:
		// frpc什么时候会向frps发送这个类型的消息？ 答：当客户端需要访问内部穿透服务的时候,frps会发送ReqWorkConn消息，frpc收到之后
		// 会和代理程序建立连接，并且返回一个NewWorkConn消息
		// conn是frpc和frps之间新建立的连接
		// TODO 分析注册WorkConn流程
		if err := svr.RegisterWorkConn(conn, m); err != nil {
			conn.Close()
		}
	case *msg.NewVisitorConn: // TODO 这种类型的消息似乎和点对点通信相关
		if err = svr.RegisterVisitorConn(conn, m); err != nil {
			xl.Warn("register visitor conn error: %v", err)
			_ = msg.WriteMsg(conn, &msg.NewVisitorConnResp{
				ProxyName: m.ProxyName,
				Error:     util.GenerateResponseErrorString("register visitor conn error", err, svr.cfg.DetailedErrorsToClient),
			})
			conn.Close()
		} else {
			_ = msg.WriteMsg(conn, &msg.NewVisitorConnResp{
				ProxyName: m.ProxyName,
				Error:     "",
			})
		}
	default:
		log.Warn("Error message type for the new connection [%s]", conn.RemoteAddr().String())
		conn.Close()
	}
}

func (svr *Service) HandleListener(l net.Listener) {
	// Listen for incoming connections from client.
	for {
		// frps等待frpc建立连接
		c, err := l.Accept()
		if err != nil {
			log.Warn("Listener for incoming connections from client closed")
			return
		}
		// inject xlog object into net.Conn context
		xl := xlog.New()
		ctx := context.Background()

		c = frpNet.NewContextConn(xlog.NewContext(ctx, xl), c)

		log.Trace("start check TLS connection...")
		originConn := c
		var isTLS, custom bool
		// 通过读取连接的第一个字节来判断是否采用了TLS
		c, isTLS, custom, err = frpNet.CheckAndEnableTLSServerConnWithTimeout(c, svr.tlsConfig, svr.cfg.TLSOnly, connReadTimeout)
		if err != nil {
			log.Warn("CheckAndEnableTLSServerConnWithTimeout error: %v", err)
			originConn.Close()
			continue
		}
		log.Trace("check TLS connection success, isTLS: %v custom: %v", isTLS, custom)

		// Start a new goroutine to handle connection.
		go func(ctx context.Context, frpConn net.Conn) {
			if svr.cfg.TCPMux {
				fmuxCfg := fmux.DefaultConfig()
				fmuxCfg.KeepAliveInterval = time.Duration(svr.cfg.TCPMuxKeepaliveInterval) * time.Second
				fmuxCfg.LogOutput = io.Discard
				session, err := fmux.Server(frpConn, fmuxCfg)
				if err != nil {
					log.Warn("Failed to create mux connection: %v", err)
					frpConn.Close()
					return
				}

				for {
					// IO多路复用
					stream, err := session.AcceptStream()
					if err != nil {
						log.Debug("Accept new mux stream error: %v", err)
						session.Close()
						return
					}
					go svr.handleConnection(ctx, stream)
				}
			} else {
				svr.handleConnection(ctx, frpConn)
			}
		}(ctx, c)
	}
}

func (svr *Service) HandleQUICListener(l quic.Listener) {
	// Listen for incoming connections from client.
	for {
		c, err := l.Accept(context.Background())
		if err != nil {
			log.Warn("QUICListener for incoming connections from client closed")
			return
		}
		// Start a new goroutine to handle connection.
		go func(ctx context.Context, frpConn quic.Connection) {
			for {
				stream, err := frpConn.AcceptStream(context.Background())
				if err != nil {
					log.Debug("Accept new quic mux stream error: %v", err)
					_ = frpConn.CloseWithError(0, "")
					return
				}
				go svr.handleConnection(ctx, frpNet.QuicStreamToNetConn(stream, frpConn))
			}
		}(context.Background(), c)
	}
}

func (svr *Service) RegisterControl(ctlConn net.Conn, loginMsg *msg.Login) (err error) {
	// If client's RunID is empty, it's a new client, we just create a new controller.
	// Otherwise, we check if there is one controller has the same run id. If so, we release previous controller and start new one.
	if loginMsg.RunID == "" {
		loginMsg.RunID, err = util.RandID()
		if err != nil {
			return
		}
	}

	ctx := frpNet.NewContextFromConn(ctlConn)
	xl := xlog.FromContextSafe(ctx)
	xl.AppendPrefix(loginMsg.RunID)
	ctx = xlog.NewContext(ctx, xl)
	xl.Info("client login info: ip [%s] version [%s] hostname [%s] os [%s] arch [%s]",
		ctlConn.RemoteAddr().String(), loginMsg.Version, loginMsg.Hostname, loginMsg.Os, loginMsg.Arch)

	// Check client version. 对比协议版本
	if ok, msg := version.Compat(loginMsg.Version); !ok {
		err = fmt.Errorf("%s", msg)
		return
	}

	// Check auth. 认证
	if err = svr.authVerifier.VerifyLogin(loginMsg); err != nil {
		return
	}

	// 对当前的frpc生成一个Controller
	ctl := NewControl(ctx, svr.rc, svr.pxyManager, svr.pluginManager, svr.authVerifier, ctlConn, loginMsg, svr.cfg)
	if oldCtl := svr.ctlManager.Add(loginMsg.RunID, ctl); oldCtl != nil {
		oldCtl.allShutdown.WaitDone()
	}

	// 每一个frpc注册上来之后需要启动Controller
	ctl.Start()

	// for statistics
	metrics.Server.NewClient()

	go func() {
		// block until control closed
		ctl.WaitClosed()
		svr.ctlManager.Del(loginMsg.RunID, ctl)
	}()
	return
}

// RegisterWorkConn register a new work connection to control and proxies need it.
func (svr *Service) RegisterWorkConn(workConn net.Conn, newMsg *msg.NewWorkConn) error {
	xl := frpNet.NewLogFromConn(workConn)
	ctl, exist := svr.ctlManager.GetByID(newMsg.RunID)
	if !exist {
		xl.Warn("No client control found for run id [%s]", newMsg.RunID)
		return fmt.Errorf("no client control found for run id [%s]", newMsg.RunID)
	}
	// server plugin hook
	content := &plugin.NewWorkConnContent{
		User: plugin.UserInfo{
			User:  ctl.loginMsg.User,
			Metas: ctl.loginMsg.Metas,
			RunID: ctl.loginMsg.RunID,
		},
		NewWorkConn: *newMsg,
	}
	retContent, err := svr.pluginManager.NewWorkConn(content)
	if err == nil {
		newMsg = &retContent.NewWorkConn
		// Check auth.
		err = svr.authVerifier.VerifyNewWorkConn(newMsg)
	}
	if err != nil {
		xl.Warn("invalid NewWorkConn with run id [%s]", newMsg.RunID)
		_ = msg.WriteMsg(workConn, &msg.StartWorkConn{
			Error: util.GenerateResponseErrorString("invalid NewWorkConn", err, ctl.serverCfg.DetailedErrorsToClient),
		})
		return fmt.Errorf("invalid NewWorkConn with run id [%s]", newMsg.RunID)
	}
	return ctl.RegisterWorkConn(workConn)
}

func (svr *Service) RegisterVisitorConn(visitorConn net.Conn, newMsg *msg.NewVisitorConn) error {
	return svr.rc.VisitorManager.NewConn(newMsg.ProxyName, visitorConn, newMsg.Timestamp, newMsg.SignKey,
		newMsg.UseEncryption, newMsg.UseCompression)
}
