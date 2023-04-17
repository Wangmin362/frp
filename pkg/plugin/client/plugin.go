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

package plugin

import (
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/fatedier/golib/errors"
)

// Creators are used for create plugins to handle connections.
var creators = make(map[string]CreatorFn)

// CreatorFn params has prefix "plugin_"
// TODO 所有通过plugin_开头的参数都被认为是插件参数
type CreatorFn func(params map[string]string) (Plugin, error)

func Register(name string, fn CreatorFn) {
	creators[name] = fn
}

func Create(name string, params map[string]string) (p Plugin, err error) {
	if fn, ok := creators[name]; ok {
		p, err = fn(params)
	} else {
		err = fmt.Errorf("plugin [%s] is not registered", name)
	}
	return
}

// Plugin 就目前的设计来看，一个代理服务只能配置一个插件
// TODO 有没有一种场景是需要配置多个插件的？ FRP如何支持多个插件？ 为什么目前FRP的设计只能支持每个代理服务配置一个插件
// 答：实际上FRP当前的插件都是代理插件，既然是代理，那么必然只能代理到一个地方，所以每个代理服务只能配置一个代理插件
type Plugin interface {
	Name() string

	// Handle extraBufToLocal will send to local connection first, then join conn with local connection
	// conn为frpc和frps之间的TCP连接  realConn为被代理的服务和frpc之间的连接
	Handle(conn io.ReadWriteCloser, realConn net.Conn, extraBufToLocal []byte)
	Close() error
}

type Listener struct {
	conns  chan net.Conn
	closed bool
	mu     sync.Mutex
}

func NewProxyListener() *Listener {
	return &Listener{
		conns: make(chan net.Conn, 64),
	}
}

func (l *Listener) Accept() (net.Conn, error) {
	conn, ok := <-l.conns
	if !ok {
		return nil, fmt.Errorf("listener closed")
	}
	return conn, nil
}

func (l *Listener) PutConn(conn net.Conn) error {
	err := errors.PanicToError(func() {
		l.conns <- conn
	})
	return err
}

func (l *Listener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		close(l.conns)
		l.closed = true
	}
	return nil
}

func (l *Listener) Addr() net.Addr {
	return (*net.TCPAddr)(nil)
}
