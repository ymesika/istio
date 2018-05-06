// Copyright 2018 Istio Authors
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

package mixer

import (
	"fmt"
	"net"

	xdsapi "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/route"
	http_conn "github.com/envoyproxy/go-control-plane/envoy/config/filter/network/http_connection_manager/v2"
	"github.com/gogo/protobuf/types"
	"github.com/prometheus/common/log"

	meshconfig "istio.io/api/mesh/v1alpha1"
	mpb "istio.io/api/mixer/v1"
	mccpb "istio.io/api/mixer/v1/config/client"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/networking/plugin"
	"istio.io/istio/pilot/pkg/networking/util"
	"istio.io/istio/pilot/pkg/proxy/envoy/v1"
)

// Plugin is a mixer plugin.
type Plugin struct{}

// NewPlugin returns an ptr to an initialized mixer.Plugin.
func NewPlugin() plugin.Plugin {
	return Plugin{}
}

// OnOutboundListener implements the Callbacks interface method.
func (Plugin) OnOutboundListener(in *plugin.InputParams, mutable *plugin.MutableObjects) error {
	if in.Service == nil || !in.Service.MeshExternal {
		return nil
	}

	env := in.Env
	node := in.Node
	proxyInstances := in.ProxyInstances

	switch in.ListenerType {
	case plugin.ListenerTypeHTTP:
		for cnum := range mutable.FilterChains {
			m := buildMixerHTTPFilter(env, node, proxyInstances, true)
			if m != nil {
				mutable.FilterChains[cnum].HTTP = append(mutable.FilterChains[cnum].HTTP, m)
			}
		}
		return nil
	case plugin.ListenerTypeTCP:
		// Adding an empty filter prevents listeners from loading
		//		for cnum := range mutable.FilterChains {
		//			mutable.FilterChains[cnum].TCP = append(mutable.FilterChains[cnum].TCP, buildMixerOutboundTCPFilter(env, node))
		//		}
		return nil
	}

	return fmt.Errorf("unknown listener type %v in mixer.OnOutboundListener", in.ListenerType)
}

// OnInboundListener implements the Callbacks interface method.
func (Plugin) OnInboundListener(in *plugin.InputParams, mutable *plugin.MutableObjects) error {
	env := in.Env
	node := in.Node
	proxyInstances := in.ProxyInstances
	instance := in.ServiceInstance

	switch in.ListenerType {
	case plugin.ListenerTypeHTTP:
		for cnum := range mutable.FilterChains {
			mutable.FilterChains[cnum].HTTP = append(mutable.FilterChains[cnum].HTTP, buildMixerHTTPFilter(env, node, proxyInstances, false))
		}
		return nil
	case plugin.ListenerTypeTCP:
		for cnum := range mutable.FilterChains {
			m := buildMixerInboundTCPFilter(env, node, instance)
			if m != nil {
				mutable.FilterChains[cnum].TCP = append(mutable.FilterChains[cnum].TCP, *m)
			}
		}
		return nil
	}

	return fmt.Errorf("unknown listener type %v in mixer.OnOutboundListener", in.ListenerType)
}

// OnOutboundCluster implements the Plugin interface method.
func (Plugin) OnOutboundCluster(env model.Environment, node model.Proxy, service *model.Service, servicePort *model.Port, cluster *xdsapi.Cluster) {
}

// OnInboundCluster implements the Plugin interface method.
func (Plugin) OnInboundCluster(env model.Environment, node model.Proxy, service *model.Service, servicePort *model.Port, cluster *xdsapi.Cluster) {
}

// OnOutboundRouteConfiguration implements the Plugin interface method.
func (Plugin) OnOutboundRouteConfiguration(in *plugin.InputParams, routeConfiguration *xdsapi.RouteConfiguration) {
}

// oc := BuildMixerConfig(node, serviceName, dest, proxyInstances, config, mesh.DisablePolicyChecks, false)
// func BuildMixerConfig(source model.Proxy, destName string, dest *model.Service, instances []*model.ServiceInstance, config model.IstioConfigStore,

// OnInboundRouteConfiguration implements the Plugin interface method.
func (Plugin) OnInboundRouteConfiguration(in *plugin.InputParams, routeConfiguration *xdsapi.RouteConfiguration) {
	forward := false
	if in.Node.Type == model.Ingress {
		forward = true
	}

	switch in.ListenerType {
	case plugin.ListenerTypeHTTP:
		var nvhs []route.VirtualHost
		for _, vh := range routeConfiguration.VirtualHosts {
			nvh := vh
			var nrs []route.Route
			for _, r := range vh.Routes {
				nr := r
				if nr.PerFilterConfig == nil {
					nr.PerFilterConfig = make(map[string]*types.Struct)
				}
				nr.PerFilterConfig[v1.MixerFilter] = util.MessageToStruct(
					buildMixerPerRouteConfig(in.Env.Mesh.DisablePolicyChecks, forward, in.ServiceInstance.Service.Hostname))
				nrs = append(nrs, nr)
			}
			nvh.Routes = nrs
			nvhs = append(nvhs, nvh)
		}
		routeConfiguration.VirtualHosts = nvhs

	case plugin.ListenerTypeTCP:
		// TODO: implement
	default:
		log.Warn("Unknown listener type in mixer#OnOutboundRouteConfiguration")
	}
}

func buildMixerPerRouteConfig(disableCheck, _ /*disableForward*/ bool, destinationService string) *mccpb.ServiceConfig {
	out := &mccpb.ServiceConfig{
		// Report calls are never disabled. Disable forward is currently not in the proto.
		DisableCheckCalls: disableCheck,
	}
	if destinationService != "" {
		out.MixerAttributes = &mpb.Attributes{}
		out.MixerAttributes.Attributes = map[string]*mpb.Attributes_AttributeValue{
			v1.AttrDestinationService: {Value: &mpb.Attributes_AttributeValue_StringValue{StringValue: destinationService}},
		}
	}

	return out
}

// buildMixerHTTPFilter builds a filter with a v1 mixer config encapsulated as JSON in a proto.Struct for v2 consumption.
func buildMixerHTTPFilter(env *model.Environment, node *model.Proxy,
	proxyInstances []*model.ServiceInstance, outbound bool) *http_conn.HttpFilter {
	mesh := env.Mesh
	config := env.IstioConfigStore
	if mesh.MixerCheckServer == "" && mesh.MixerReportServer == "" {
		return nil
	}

	c := buildHTTPMixerFilterConfig(mesh, *node, proxyInstances, outbound, config)
	return &http_conn.HttpFilter{
		Name:   v1.MixerFilter,
		Config: util.MessageToStruct(c),
	}
}

// buildMixerInboundTCPFilter builds a filter with a v1 mixer config encapsulated as JSON in a proto.Struct for v2 consumption.
func buildMixerInboundTCPFilter(env *model.Environment, node *model.Proxy, instance *model.ServiceInstance) *listener.Filter {
	mesh := env.Mesh
	if mesh.MixerCheckServer == "" && mesh.MixerReportServer == "" {
		return nil
	}

	c := buildTCPMixerFilterConfig(mesh, *node, instance)
	return &listener.Filter{
		Name:   v1.MixerFilter,
		Config: util.MessageToStruct(c),
	}
}

// // buildMixerOutboundTCPFilter builds a filter with a v1 mixer config encapsulated as JSON in a proto.Struct for v2 consumption.
// func buildMixerOutboundTCPFilter(env *model.Environment, node *model.Proxy) listener.Filter {
// 	// TODO(mostrowski): implementation
// 	return listener.Filter{}
// }

// buildHTTPMixerFilterConfig builds a mixer HTTP filter config. Mixer filter uses outbound configuration by default
// (forward attributes, but not invoke check calls)  ServiceInstances belong to the Node.
func buildHTTPMixerFilterConfig(mesh *meshconfig.MeshConfig, role model.Proxy, nodeInstances []*model.ServiceInstance, outboundRoute bool, config model.IstioConfigStore) *mccpb.HttpClientConfig { // nolint: lll
	mcs, _, _ := net.SplitHostPort(mesh.MixerCheckServer)
	mrs, _, _ := net.SplitHostPort(mesh.MixerReportServer)

	pname := &model.Port{Name: "http2-mixer"}
	if mesh.AuthPolicy == meshconfig.MeshConfig_MUTUAL_TLS {
		pname = &model.Port{Name: "tcp-mtls"}
	}

	// TODO: derive these port types.
	transport := &mccpb.TransportConfig{
		CheckCluster:  model.BuildSubsetKey(model.TrafficDirectionOutbound, "", mcs, pname),
		ReportCluster: model.BuildSubsetKey(model.TrafficDirectionOutbound, "", mrs, pname),
	}

	mxConfig := &mccpb.HttpClientConfig{
		MixerAttributes: &mpb.Attributes{
			Attributes: map[string]*mpb.Attributes_AttributeValue{},
		},
		ServiceConfigs: map[string]*mccpb.ServiceConfig{},
		Transport:      transport,
	}

	var labels map[string]string
	// Note: instances are all running on mode.Node named 'role'
	// So instance labels are the workload / Node labels.
	if len(nodeInstances) > 0 {
		labels = nodeInstances[0].Labels
		mxConfig.DefaultDestinationService = nodeInstances[0].Service.Hostname
	}

	if !outboundRoute {
		// for outboundRoutes there are no default MixerAttributes
		// specific MixerAttributes are in per route configuration.
		v1.AddStandardNodeAttributes(mxConfig.MixerAttributes.Attributes, v1.AttrDestinationPrefix, role.IPAddress, role.ID, labels)
	}

	if role.Type == model.Sidecar && !outboundRoute {
		// Don't forward mixer attributes to the app from inbound sidecar routes
	} else {
		mxConfig.ForwardAttributes = &mpb.Attributes{
			Attributes: map[string]*mpb.Attributes_AttributeValue{},
		}
		addStandardNodeAttributes(mxConfig.ForwardAttributes.Attributes, v1.AttrSourcePrefix, role.IPAddress, role.ID, labels)
	}

	for _, instance := range nodeInstances {
		mxConfig.ServiceConfigs[instance.Service.Hostname] = v1.ServiceConfig(instance.Service.Hostname, instance, config,
			outboundRoute || mesh.DisablePolicyChecks, outboundRoute)
	}

	return mxConfig
}

// buildTCPMixerFilterConfig builds a TCP filter config for inbound requests.
func buildTCPMixerFilterConfig(mesh *meshconfig.MeshConfig, role model.Proxy, instance *model.ServiceInstance) *mccpb.TcpClientConfig {
	attrs := v1.StandardNodeAttributes(v1.AttrDestinationPrefix, role.IPAddress, role.ID, nil)
	attrs[v1.AttrDestinationService] = &mpb.Attributes_AttributeValue{Value: &mpb.Attributes_AttributeValue_StringValue{instance.Service.Hostname}}

	mxConfig := &mccpb.TcpClientConfig{
		MixerAttributes: &mpb.Attributes{
			Attributes: attrs,
		},
		Transport: &mccpb.TransportConfig{
			CheckCluster:  v1.MixerCheckClusterName,
			ReportCluster: v1.MixerReportClusterName,
		},
		DisableCheckCalls: mesh.DisablePolicyChecks,
	}

	return mxConfig
}

// addStandardNodeAttributes add standard node attributes with the given prefix
func addStandardNodeAttributes(attr map[string]*mpb.Attributes_AttributeValue, prefix string, IPAddress string, ID string, labels map[string]string) {
	if len(IPAddress) > 0 {
		attr[prefix+"."+v1.AttrIPSuffix] = &mpb.Attributes_AttributeValue{
			Value: &mpb.Attributes_AttributeValue_BytesValue{net.ParseIP(IPAddress)},
		}
	}

	attr[prefix+"."+v1.AttrUIDSuffix] = &mpb.Attributes_AttributeValue{
		Value: &mpb.Attributes_AttributeValue_StringValue{"kubernetes://" + ID},
	}

	if len(labels) > 0 {
		attr[prefix+"."+v1.AttrLabelsSuffix] = &mpb.Attributes_AttributeValue{
			Value: &mpb.Attributes_AttributeValue_StringMapValue{
				StringMapValue: &mpb.Attributes_StringMap{Entries: labels},
			},
		}
	}
}
