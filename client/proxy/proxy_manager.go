package proxy

import (
	"context"
	"fmt"
	"net"
	"sync"

	"github.com/fatedier/golib/errors"

	"github.com/fatedier/frp/client/event"
	"github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/util/xlog"
)

type Manager struct {
	sendCh chan msg.Message
	// ProxyManager管理的代理
	proxies map[string]*Wrapper

	closed bool
	mu     sync.RWMutex

	clientCfg config.ClientCommonConf

	// The UDP port that the server is listening on
	serverUDPPort int

	ctx context.Context
}

func NewManager(ctx context.Context, msgSendCh chan msg.Message, clientCfg config.ClientCommonConf, serverUDPPort int) *Manager {
	return &Manager{
		sendCh:        msgSendCh,
		proxies:       make(map[string]*Wrapper),
		closed:        false,
		clientCfg:     clientCfg,
		serverUDPPort: serverUDPPort,
		ctx:           ctx,
	}
}

// StartProxy 说起启动代理,实际上就是设置了以下状态
func (pm *Manager) StartProxy(name string, remoteAddr string, serverRespErr string) error {
	pm.mu.RLock()
	pxy, ok := pm.proxies[name]
	pm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("proxy [%s] not found", name)
	}

	// 这里主要是生成一个对应类型的代理插件,譬如TCP, HTTP, HTTPS, WEBSOCKET等等,然后设置当前代理的状态为Running
	err := pxy.SetRunningStatus(remoteAddr, serverRespErr)
	if err != nil {
		return err
	}
	return nil
}

func (pm *Manager) Close() {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	for _, pxy := range pm.proxies {
		pxy.Stop()
	}
	pm.proxies = make(map[string]*Wrapper)
}

func (pm *Manager) HandleWorkConn(name string, workConn net.Conn, m *msg.StartWorkConn) {
	pm.mu.RLock()
	pw, ok := pm.proxies[name]
	pm.mu.RUnlock()
	if ok {
		pw.InWorkConn(workConn, m)
	} else {
		workConn.Close()
	}
}

func (pm *Manager) HandleEvent(payload interface{}) error {
	var m msg.Message
	switch e := payload.(type) {
	case *event.StartProxyPayload:
		m = e.NewProxyMsg
	case *event.CloseProxyPayload:
		m = e.CloseProxyMsg
	default:
		return event.ErrPayloadType
	}

	err := errors.PanicToError(func() {
		pm.sendCh <- m
	})
	return err
}

func (pm *Manager) GetAllProxyStatus() []*WorkingStatus {
	ps := make([]*WorkingStatus, 0)
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for _, pxy := range pm.proxies {
		ps = append(ps, pxy.GetStatus())
	}
	return ps
}

// Reload 重新设置代理服务, 这里应该是frp代理热加载功能的实现
func (pm *Manager) Reload(pxyCfgs map[string]config.ProxyConf) {
	// 从context中获取logger
	xl := xlog.FromContextSafe(pm.ctx)
	pm.mu.Lock()
	defer pm.mu.Unlock()

	delPxyNames := make([]string, 0)
	for name, pxy := range pm.proxies {
		del := false
		cfg, ok := pxyCfgs[name]
		if !ok {
			del = true
		} else if !pxy.Cfg.Compare(cfg) {
			del = true
		}

		if del {
			delPxyNames = append(delPxyNames, name)
			delete(pm.proxies, name)

			pxy.Stop()
		}
	}
	if len(delPxyNames) > 0 {
		xl.Info("proxy removed: %v", delPxyNames)
	}

	addPxyNames := make([]string, 0)
	for name, cfg := range pxyCfgs {
		if _, ok := pm.proxies[name]; !ok {
			pxy := NewWrapper(pm.ctx, cfg, pm.clientCfg, pm.HandleEvent, pm.serverUDPPort)
			pm.proxies[name] = pxy
			addPxyNames = append(addPxyNames, name)

			pxy.Start()
		}
	}
	if len(addPxyNames) > 0 {
		xl.Info("proxy added: %v", addPxyNames)
	}
}
