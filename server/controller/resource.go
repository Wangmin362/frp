// Copyright 2019 fatedier, fatedier@gmail.com
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

package controller

import (
	"github.com/fatedier/frp/pkg/nathole"
	plugin "github.com/fatedier/frp/pkg/plugin/server"
	"github.com/fatedier/frp/pkg/util/tcpmux"
	"github.com/fatedier/frp/pkg/util/vhost"
	"github.com/fatedier/frp/server/group"
	"github.com/fatedier/frp/server/ports"
	"github.com/fatedier/frp/server/visitor"
)

// ResourceController All resource managers and controllers
type ResourceController struct {
	// Manage all visitor listeners
	// TODO 点对点的内网穿透的管理
	VisitorManager *visitor.Manager

	// TCP Group Controller
	// TCP服务的负载均衡
	TCPGroupCtl *group.TCPGroupCtl

	// HTTP Group Controller
	// HTTP服务的负载均衡
	HTTPGroupCtl *group.HTTPGroupController

	// TCP Mux Group Controller
	// TCPMux类型的服务的负载均衡
	TCPMuxGroupCtl *group.TCPMuxGroupCtl

	// Manage all TCP ports
	// TCP端口管理，主要是用于判断当前代理需要的remote_port是否被占用，以及分配一个可使用的新端口
	TCPPortManager *ports.Manager

	// Manage all UDP ports
	// UDP端口管理，主要是用于判断当前代理需要的remote_port是否被占用，以及分配一个可使用的新端口
	UDPPortManager *ports.Manager

	// For HTTP proxies, forwarding HTTP requests
	// TODO 根据HTTP的特性进行反向代理
	HTTPReverseProxy *vhost.HTTPReverseProxy

	// For HTTPS proxies, route requests to different clients by hostname and other information
	// TODO 根据HTTP的特性进行反向代理
	VhostHTTPSMuxer *vhost.HTTPSMuxer

	// Controller for nat hole connections
	// UDP服务
	NatHoleController *nathole.Controller

	// TCPMux HTTP CONNECT multiplexer
	// TODO
	TCPMuxHTTPConnectMuxer *tcpmux.HTTPConnectTCPMuxer

	// All server manager plugin
	PluginManager *plugin.Manager
}
