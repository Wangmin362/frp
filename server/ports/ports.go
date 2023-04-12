package ports

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"time"
)

const (
	MinPort = 1
	MaxPort = 65535
	// MaxPortReservedDuration 最多保留一天
	MaxPortReservedDuration    = time.Duration(24) * time.Hour
	CleanReservedPortsInterval = time.Hour
)

var (
	ErrPortAlreadyUsed = errors.New("port already used")
	ErrPortNotAllowed  = errors.New("port not allowed")
	ErrPortUnAvailable = errors.New("port unavailable")
	ErrNoAvailablePort = errors.New("no available port")
)

type PortCtx struct {
	ProxyName  string
	Port       int
	Closed     bool
	UpdateTime time.Time
}

type Manager struct {
	// TODO 什么叫做保留的端口？
	reservedPorts map[string]*PortCtx
	// 已经使用的端口
	usedPorts map[int]*PortCtx
	// 还未使用的端口
	freePorts map[int]struct{}

	bindAddr string // 绑定地址
	netType  string // TCP还是UDP
	mu       sync.Mutex
}

func NewManager(netType string, bindAddr string, allowPorts map[int]struct{}) *Manager {
	pm := &Manager{
		reservedPorts: make(map[string]*PortCtx),
		usedPorts:     make(map[int]*PortCtx),
		freePorts:     make(map[int]struct{}),
		bindAddr:      bindAddr,
		netType:       netType,
	}

	// 如果没有指定允许的端口，那么1-65535端口都是可以使用的
	if len(allowPorts) > 0 {
		for port := range allowPorts {
			pm.freePorts[port] = struct{}{}
		}
	} else {
		for i := MinPort; i <= MaxPort; i++ {
			pm.freePorts[i] = struct{}{}
		}
	}
	go pm.cleanReservedPortsWorker()
	return pm
}

// Acquire 当新增一个代理服务的时候，应该向PortManager申请一个可用端口
// TODO port难道可以不等于realPort么？  必须是相等的吧
func (pm *Manager) Acquire(name string, port int) (realPort int, err error) {
	portCtx := &PortCtx{
		ProxyName:  name,
		Closed:     false,
		UpdateTime: time.Now(),
	}

	var ok bool

	pm.mu.Lock()
	defer func() {
		if err == nil {
			portCtx.Port = realPort
		}
		pm.mu.Unlock()
	}()

	// check reserved ports first
	if port == 0 {
		if ctx, ok := pm.reservedPorts[name]; ok {
			if pm.isPortAvailable(ctx.Port) {
				realPort = ctx.Port
				pm.usedPorts[realPort] = portCtx
				pm.reservedPorts[name] = portCtx
				delete(pm.freePorts, realPort)
				return
			}
		}
	}

	if port == 0 {
		// get random port
		count := 0
		maxTryTimes := 5
		for k := range pm.freePorts {
			count++
			if count > maxTryTimes {
				break
			}
			if pm.isPortAvailable(k) {
				realPort = k
				pm.usedPorts[realPort] = portCtx
				pm.reservedPorts[name] = portCtx
				delete(pm.freePorts, realPort)
				break
			}
		}
		if realPort == 0 {
			err = ErrNoAvailablePort
		}
	} else {
		// specified port
		if _, ok = pm.freePorts[port]; ok {
			if pm.isPortAvailable(port) {
				realPort = port
				pm.usedPorts[realPort] = portCtx
				pm.reservedPorts[name] = portCtx
				delete(pm.freePorts, realPort)
			} else {
				err = ErrPortUnAvailable
			}
		} else {
			if _, ok = pm.usedPorts[port]; ok {
				err = ErrPortAlreadyUsed
			} else {
				err = ErrPortNotAllowed
			}
		}
	}
	return
}

// isPortAvailable 判断某个端口是否可以使用的标准就是看看能不能监听这个端口
func (pm *Manager) isPortAvailable(port int) bool {
	if pm.netType == "udp" {
		addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(pm.bindAddr, strconv.Itoa(port)))
		if err != nil {
			return false
		}
		l, err := net.ListenUDP("udp", addr)
		if err != nil {
			return false
		}
		l.Close()
		return true
	}

	l, err := net.Listen(pm.netType, net.JoinHostPort(pm.bindAddr, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// Release 释放一个端口，在frpc取消某个代理服务的时候，应该关闭这个端口
func (pm *Manager) Release(port int) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if ctx, ok := pm.usedPorts[port]; ok {
		pm.freePorts[port] = struct{}{}
		delete(pm.usedPorts, port)
		ctx.Closed = true
		ctx.UpdateTime = time.Now()
	}
}

// Release reserved port if it isn't used in last 24 hours.
func (pm *Manager) cleanReservedPortsWorker() {
	for {
		// 每一小时执行一次
		time.Sleep(CleanReservedPortsInterval)
		pm.mu.Lock()
		for name, ctx := range pm.reservedPorts {
			// 一旦超过24个小时，就从保留端口当中删除这个端口
			if ctx.Closed && time.Since(ctx.UpdateTime) > MaxPortReservedDuration {
				delete(pm.reservedPorts, name)
			}
		}
		pm.mu.Unlock()
	}
}
