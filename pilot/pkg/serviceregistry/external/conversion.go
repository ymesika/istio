// Copyright 2017 Istio Authors
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

package external

import (
	"net"

	meshconfig "istio.io/api/mesh/v1alpha1"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
)

func convertPort(port *networking.Port) *model.Port {
	return &model.Port{
		Name:                 port.Name,
		Port:                 int(port.Number),
		Protocol:             model.ParseProtocol(port.Protocol),
		AuthenticationPolicy: meshconfig.AuthenticationPolicy_NONE,
	}
}

func convertServices(serviceEntry *networking.ServiceEntry) []*model.Service {
	out := make([]*model.Service, 0)

	var resolution model.Resolution
	switch serviceEntry.Resolution {
	case networking.ServiceEntry_NONE:
		resolution = model.Passthrough
	case networking.ServiceEntry_DNS:
		resolution = model.DNSLB
	case networking.ServiceEntry_STATIC:
		resolution = model.ClientSideLB
	}

	svcPorts := make(model.PortList, 0, len(serviceEntry.Ports))
	for _, port := range serviceEntry.Ports {
		svcPorts = append(svcPorts, convertPort(port))
	}

	for _, host := range serviceEntry.Hosts {
		if len(serviceEntry.Addresses) > 0 {
			for _, address := range serviceEntry.Addresses {
				if _, _, cidrErr := net.ParseCIDR(address); cidrErr == nil || net.ParseIP(address) != nil {
					out = append(out, &model.Service{
						MeshExternal: serviceEntry.Location == networking.ServiceEntry_MESH_EXTERNAL,
						Hostname:     model.Hostname(host),
						Address:      address,
						Ports:        svcPorts,
						Resolution:   resolution,
					})
				}
			}
		} else {
			out = append(out, &model.Service{
				MeshExternal: serviceEntry.Location == networking.ServiceEntry_MESH_EXTERNAL,
				Hostname:     model.Hostname(host),
				Address:      "",
				Ports:        svcPorts,
				Resolution:   resolution,
			})
		}
	}

	return out
}

func convertEndpoint(service *model.Service, servicePort *networking.Port,
	endpoint *networking.ServiceEntry_Endpoint) *model.ServiceInstance {

	instancePort := endpoint.Ports[servicePort.Name]
	if instancePort == 0 {
		instancePort = servicePort.Number
	}

	return &model.ServiceInstance{
		Endpoint: model.NetworkEndpoint{
			Address:     endpoint.Address,
			Port:        int(instancePort),
			ServicePort: convertPort(servicePort),
		},
		// TODO AvailabilityZone, ServiceAccount
		Service: service,
		Labels:  endpoint.Labels,
	}
}

func convertInstances(serviceEntry *networking.ServiceEntry) []*model.ServiceInstance {
	out := make([]*model.ServiceInstance, 0)
	for _, service := range convertServices(serviceEntry) {
		for _, servicePort := range serviceEntry.Ports {
			if len(serviceEntry.Endpoints) == 0 &&
				serviceEntry.Resolution == networking.ServiceEntry_DNS {
				// when service entry has discovery type DNS and no endpoints
				// we create endpoints from service entry hosts field
				for _, host := range serviceEntry.Hosts {
					out = append(out, &model.ServiceInstance{
						Endpoint: model.NetworkEndpoint{
							Address:     host,
							Port:        int(servicePort.Number),
							ServicePort: convertPort(servicePort),
						},
						// TODO AvailabilityZone, ServiceAccount
						Service: service,
						Labels:  nil,
					})
				}
			}
			for _, endpoint := range serviceEntry.Endpoints {
				out = append(out, convertEndpoint(service, servicePort, endpoint))
			}
		}
	}
	return out
}
