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

package v2

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"sync"

	"github.com/gogo/protobuf/jsonpb"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry"
	"istio.io/istio/pilot/pkg/serviceregistry/aggregate"
	"istio.io/istio/pkg/log"
)

// memregistry is based on mock/discovery - it is used for testing and debugging v2.
// In future (post 1.0) it may be used for representing remote pilots.

// InitDebug initializes the debug handlers and adds a debug in-memory registry.
func (s *DiscoveryServer) InitDebug(mux *http.ServeMux, sctl *aggregate.Controller) {
	// For debugging and load testing v2 we add an memory registry.
	s.MemRegistry = NewMemServiceDiscovery(
		map[string]*model.Service{ // mock.HelloService.Hostname: mock.HelloService,
		}, 2)

	sctl.AddRegistry(aggregate.Registry{
		ClusterID:        "v2-debug",
		Name:             serviceregistry.ServiceRegistry("memAdapter"),
		ServiceDiscovery: s.MemRegistry,
		ServiceAccounts:  s.MemRegistry,
		Controller:       s.MemRegistry.controller,
	})

	mux.HandleFunc("/debug/edsz", edsz)
	mux.HandleFunc("/debug/adsz", adsz)
	mux.HandleFunc("/debug/cdsz", cdsz)

	mux.HandleFunc("/debug/registryz", s.registryz)
	mux.HandleFunc("/debug/endpointz", s.endpointz)
	mux.HandleFunc("/debug/configz", s.configz)
}

// NewMemServiceDiscovery builds an in-memory MemServiceDiscovery
func NewMemServiceDiscovery(services map[string]*model.Service, versions int) *MemServiceDiscovery {
	return &MemServiceDiscovery{
		services:    services,
		versions:    versions,
		controller:  &memServiceController{},
		instances:   map[string][]*model.ServiceInstance{},
		ip2instance: map[string][]*model.ServiceInstance{},
	}
}

// TODO: the mock was used for test setup, has no mutex. This will also be used for
// integration and load tests, will need to add mutex as we cleanup the code.

type memServiceController struct {
	svcHandlers  []func(*model.Service, model.Event)
	instHandlers []func(*model.ServiceInstance, model.Event)
}

func (c *memServiceController) AppendServiceHandler(f func(*model.Service, model.Event)) error {
	c.svcHandlers = append(c.svcHandlers, f)
	return nil
}

func (c *memServiceController) AppendInstanceHandler(f func(*model.ServiceInstance, model.Event)) error {
	c.instHandlers = append(c.instHandlers, f)
	return nil
}

func (c *memServiceController) Run(<-chan struct{}) {}

// MemServiceDiscovery is a mock discovery interface
type MemServiceDiscovery struct {
	services map[string]*model.Service
	// Endpoints table. Key is the fqdn of the service, ':', port
	instances                     map[string][]*model.ServiceInstance
	ip2instance                   map[string][]*model.ServiceInstance
	versions                      int
	WantGetProxyServiceInstances  []*model.ServiceInstance
	ServicesError                 error
	GetServiceError               error
	InstancesError                error
	GetProxyServiceInstancesError error
	controller                    model.Controller

	// Single mutex for now - it's for debug only.
	mutex sync.Mutex
}

// ClearErrors clear errors used for mocking failures during model.MemServiceDiscovery interface methods
func (sd *MemServiceDiscovery) ClearErrors() {
	sd.ServicesError = nil
	sd.GetServiceError = nil
	sd.InstancesError = nil
	sd.GetProxyServiceInstancesError = nil
}

// AddService adds an in-memory service.
func (sd *MemServiceDiscovery) AddService(name string, svc *model.Service) {
	sd.mutex.Lock()
	sd.services[name] = svc
	sd.mutex.Unlock()
	// TODO: notify listeners
}

// AddInstance adds an in-memory instance.
func (sd *MemServiceDiscovery) AddInstance(service string, instance *model.ServiceInstance) {
	// WIP: add enough code to allow tests and load tests to work
	sd.mutex.Lock()
	defer sd.mutex.Unlock()
	svc := sd.services[service]
	if svc == nil {
		return
	}
	instance.Service = svc
	sd.ip2instance[instance.Endpoint.Address] = []*model.ServiceInstance{instance}

	key := fmt.Sprintf("%s:%s", service, instance.Endpoint.ServicePort.Name)
	instanceList := sd.instances[key]
	if instanceList == nil {
		instanceList = []*model.ServiceInstance{instance}
		sd.instances[key] = instanceList
		return
	}
	sd.instances[key] = append(instanceList, instance)
}

// AddEndpoint adds an endpoint to a service.
func (sd *MemServiceDiscovery) AddEndpoint(service, servicePortName string, servicePort int, address string, port int) *model.ServiceInstance {
	instance := &model.ServiceInstance{
		Endpoint: model.NetworkEndpoint{
			Address: address,
			Port:    port,
			ServicePort: &model.Port{
				Name:                 servicePortName,
				Port:                 servicePort,
				Protocol:             model.ProtocolHTTP,
				AuthenticationPolicy: meshconfig.AuthenticationPolicy_INHERIT,
			},
		},
	}
	sd.AddInstance(service, instance)
	return instance
}

// Services implements discovery interface
func (sd *MemServiceDiscovery) Services() ([]*model.Service, error) {
	sd.mutex.Lock()
	defer sd.mutex.Unlock()
	if sd.ServicesError != nil {
		return nil, sd.ServicesError
	}
	out := make([]*model.Service, 0, len(sd.services))
	for _, service := range sd.services {
		out = append(out, service)
	}
	return out, sd.ServicesError
}

// GetService implements discovery interface
func (sd *MemServiceDiscovery) GetService(hostname string) (*model.Service, error) {
	sd.mutex.Lock()
	defer sd.mutex.Unlock()
	if sd.GetServiceError != nil {
		return nil, sd.GetServiceError
	}
	val := sd.services[hostname]
	return val, sd.GetServiceError
}

// Instances filters the service instances by labels. This assumes single port, as is
// used by EDS/ADS.
func (sd *MemServiceDiscovery) Instances(hostname string, ports []string,
	labels model.LabelsCollection) ([]*model.ServiceInstance, error) {
	sd.mutex.Lock()
	defer sd.mutex.Unlock()
	if sd.InstancesError != nil {
		return nil, sd.InstancesError
	}
	if len(ports) != 1 {
		log.Warna("Unexpected ports ", ports)
		return nil, nil
	}
	key := hostname + ":" + ports[0]
	instances, ok := sd.instances[key]
	if !ok {
		return nil, nil
	}
	return instances, nil
}

// GetProxyServiceInstances returns service instances associated with a node, resulting in
// 'in' services.
func (sd *MemServiceDiscovery) GetProxyServiceInstances(node *model.Proxy) ([]*model.ServiceInstance, error) {
	sd.mutex.Lock()
	defer sd.mutex.Unlock()
	if sd.GetProxyServiceInstancesError != nil {
		return nil, sd.GetProxyServiceInstancesError
	}
	if sd.WantGetProxyServiceInstances != nil {
		return sd.WantGetProxyServiceInstances, nil
	}
	out := make([]*model.ServiceInstance, 0)
	si, found := sd.ip2instance[node.IPAddress]
	if found {
		out = append(out, si...)
	}
	return out, sd.GetProxyServiceInstancesError
}

// ManagementPorts implements discovery interface
func (sd *MemServiceDiscovery) ManagementPorts(addr string) model.PortList {
	sd.mutex.Lock()
	defer sd.mutex.Unlock()
	return model.PortList{{
		Name:     "http",
		Port:     3333,
		Protocol: model.ProtocolHTTP,
	}, {
		Name:     "custom",
		Port:     9999,
		Protocol: model.ProtocolTCP,
	}}
}

// GetIstioServiceAccounts gets the Istio service accounts for a service hostname.
func (sd *MemServiceDiscovery) GetIstioServiceAccounts(hostname string, ports []string) []string {
	sd.mutex.Lock()
	defer sd.mutex.Unlock()
	if hostname == "world.default.svc.cluster.local" {
		return []string{
			"spiffe://cluster.local/ns/default/sa/serviceaccount1",
			"spiffe://cluster.local/ns/default/sa/serviceaccount2",
		}
	}
	return make([]string, 0)
}

// registryz providees debug support for registry - adding and listing model items.
// Can be combined with the push debug interface to reproduce changes.
func (s *DiscoveryServer) registryz(w http.ResponseWriter, req *http.Request) {
	_ = req.ParseForm()
	w.Header().Add("Content-Type", "application/json")
	svcName := req.Form.Get("svc")
	if svcName != "" {
		data, err := ioutil.ReadAll(req.Body)
		if err != nil {
			return
		}
		svc := &model.Service{}
		err = json.Unmarshal(data, svc)
		if err != nil {
			return
		}
		s.MemRegistry.AddService(svcName, svc)
	}

	all, err := s.env.ServiceDiscovery.Services()
	if err != nil {
		return
	}
	fmt.Fprintln(w, "[")
	for _, svc := range all {
		b, err := json.MarshalIndent(svc, "", "  ")
		if err != nil {
			return
		}
		_, _ = w.Write(b)
		fmt.Fprintln(w, ",")
	}
	fmt.Fprintln(w, "{}]")
}

// Endpoint debugging
func (s *DiscoveryServer) endpointz(w http.ResponseWriter, req *http.Request) {
	_ = req.ParseForm()
	w.Header().Add("Content-Type", "application/json")
	svcName := req.Form.Get("svc")
	if svcName != "" {
		data, err := ioutil.ReadAll(req.Body)
		if err != nil {
			return
		}
		svc := &model.ServiceInstance{}
		err = json.Unmarshal(data, svc)
		if err != nil {
			return
		}
		s.MemRegistry.AddInstance(svcName, svc)
	}
	brief := req.Form.Get("brief")
	if brief != "" {
		svc, _ := s.env.ServiceDiscovery.Services()
		for _, ss := range svc {
			for _, p := range ss.Ports {
				all, err := s.env.ServiceDiscovery.Instances(ss.Hostname, []string{p.Name}, nil)
				if err != nil {
					return
				}
				for _, svc := range all {
					fmt.Fprintf(w, "%s:%s %s:%d %v %v %s\n", ss.Hostname,
						p.Name, svc.Endpoint.Address, svc.Endpoint.Port, svc.Labels,
						svc.AvailabilityZone, svc.ServiceAccount)
				}
			}
		}
		return
	}

	svc, _ := s.env.ServiceDiscovery.Services()
	fmt.Fprint(w, "[\n")
	for _, ss := range svc {
		for _, p := range ss.Ports {
			all, err := s.env.ServiceDiscovery.Instances(ss.Hostname, []string{p.Name}, nil)
			if err != nil {
				return
			}
			fmt.Fprintf(w, "\n{\"svc\": \"%s:%s\", \"ep\": [\n", ss.Hostname, p.Name)
			for _, svc := range all {
				b, err := json.MarshalIndent(svc, "  ", "  ")
				if err != nil {
					return
				}
				_, _ = w.Write(b)
				fmt.Fprint(w, ",\n")
			}
			fmt.Fprint(w, "\n{}]},")
		}
	}
	fmt.Fprint(w, "\n{}]\n")
}

// Config debugging.
func (s *DiscoveryServer) configz(w http.ResponseWriter, req *http.Request) {
	w.Header().Add("Content-Type", "application/json")
	fmt.Fprintf(w, "\n[\n")
	for _, typ := range s.env.IstioConfigStore.ConfigDescriptor() {
		cfg, _ := s.env.IstioConfigStore.List(typ.Type, "")
		for _, c := range cfg {
			b, err := json.MarshalIndent(c, "  ", "  ")
			if err != nil {
				return
			}
			_, _ = w.Write(b)
			fmt.Fprint(w, ",\n")
		}
	}
	fmt.Fprint(w, "\n{}]")
}

// adsz implements a status and debug interface for ADS.
// It is mapped to /debug/adsz
func adsz(w http.ResponseWriter, req *http.Request) {
	_ = req.ParseForm()
	w.Header().Add("Content-Type", "application/json")
	if req.Form.Get("debug") != "" {
		adsDebug = req.Form.Get("debug") == "1"
		return
	}
	if req.Form.Get("push") != "" {
		adsPushAll()
		fmt.Fprintf(w, "Pushed to %d servers", len(adsClients))
		return
	}

	if proxyID := req.URL.Query().Get("proxyID"); proxyID != "" {
		writeADSForSidecar(w, proxyID)
		return
	}
	writeAllADS(w)
}

func writeADSForSidecar(w io.Writer, proxyID string) {
	adsClientsMutex.RLock()
	defer adsClientsMutex.RUnlock()
	connections := adsSidecarIDConnectionsMap[proxyID]
	for _, conn := range connections {
		for _, ls := range conn.HTTPListeners {
			jsonm := &jsonpb.Marshaler{Indent: "  "}
			dbgString, _ := jsonm.MarshalToString(ls)
			if _, err := w.Write([]byte(dbgString)); err != nil {
				return
			}
			fmt.Fprintln(w)
		}
		for _, cs := range conn.HTTPClusters {
			jsonm := &jsonpb.Marshaler{Indent: "  "}
			dbgString, _ := jsonm.MarshalToString(cs)
			if _, err := w.Write([]byte(dbgString)); err != nil {
				return
			}
			fmt.Fprintln(w)
		}
	}
}

func writeAllADS(w io.Writer) {
	adsClientsMutex.RLock()
	defer adsClientsMutex.RUnlock()

	// Dirty json generation - because standard json is dirty (struct madness)
	// Unfortunately we must use the jsonbp to encode part of the json - I'm sure there are
	// better ways, but this is mainly for debugging.
	fmt.Fprint(w, "[\n")
	comma := false
	for _, c := range adsClients {
		if comma {
			fmt.Fprint(w, ",\n")
		} else {
			comma = true
		}
		fmt.Fprintf(w, "\n\n  {\"node\": \"%s\",\n \"addr\": \"%s\",\n \"connect\": \"%v\",\n \"listeners\":[\n", c.ConID, c.PeerAddr, c.Connect)
		printListeners(w, c)
		fmt.Fprint(w, "],\n")
		fmt.Fprintf(w, ",\"clusters\":[\n")
		printClusters(w, c)
		fmt.Fprint(w, "]}\n")
	}
	fmt.Fprint(w, "]\n")
}

// edsz implements a status and debug interface for EDS.
// It is mapped to /debug/edsz on the monitor port (9093).
func edsz(w http.ResponseWriter, req *http.Request) {
	_ = req.ParseForm()
	if req.Form.Get("debug") != "" {
		edsDebug = req.Form.Get("debug") == "1"
		return
	}
	w.Header().Add("Content-Type", "application/json")

	if req.Form.Get("push") != "" {
		edsPushAll()
	}

	edsClusterMutex.Lock()
	comma := false
	fmt.Fprintln(w, "[")
	for _, eds := range edsClusters {
		if comma {
			fmt.Fprint(w, ",\n")
		} else {
			comma = true
		}
		jsonm := &jsonpb.Marshaler{Indent: "  "}
		dbgString, _ := jsonm.MarshalToString(eds.LoadAssignment)
		if _, err := w.Write([]byte(dbgString)); err != nil {
			return
		}
	}
	fmt.Fprintln(w, "]")
	edsClusterMutex.Unlock()
}

// cdsz implements a status and debug interface for CDS.
// It is mapped to /debug/cdsz
func cdsz(w http.ResponseWriter, req *http.Request) {
	_ = req.ParseForm()
	w.Header().Add("Content-Type", "application/json")

	adsClientsMutex.RLock()

	fmt.Fprint(w, "[\n")
	comma := false
	for _, c := range adsClients {
		if comma {
			fmt.Fprint(w, ",\n")
		} else {
			comma = true
		}
		fmt.Fprintf(w, "\n\n  {\"node\": \"%s\", \"addr\": \"%s\", \"connect\": \"%v\",\"Clusters\":[\n", c.ConID, c.PeerAddr, c.Connect)
		printClusters(w, c)
		fmt.Fprint(w, "]}\n")
	}
	fmt.Fprint(w, "]\n")

	adsClientsMutex.RUnlock()
}

func printListeners(w io.Writer, c *XdsConnection) {
	comma := false
	for _, ls := range c.HTTPListeners {
		if ls == nil {
			log.Errorf("INVALID LISTENER NIL")
			continue
		}
		if comma {
			fmt.Fprint(w, ",\n")
		} else {
			comma = true
		}
		jsonm := &jsonpb.Marshaler{Indent: "  "}
		dbgString, _ := jsonm.MarshalToString(ls)
		if _, err := w.Write([]byte(dbgString)); err != nil {
			return
		}
	}
}

func printClusters(w io.Writer, c *XdsConnection) {
	comma := false
	for _, cl := range c.HTTPClusters {
		if cl == nil {
			log.Errorf("INVALID Cluster NIL")
			continue
		}
		if comma {
			fmt.Fprint(w, ",\n")
		} else {
			comma = true
		}
		jsonm := &jsonpb.Marshaler{Indent: "  "}
		dbgString, _ := jsonm.MarshalToString(cl)
		if _, err := w.Write([]byte(dbgString)); err != nil {
			return
		}
	}
}
