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

// Functions related to translation from the control policies to Envoy config
// Policies apply to Envoy upstream clusters but may appear in the route section.

package v1

import (
	meshconfig "istio.io/api/mesh/v1alpha1"
	routing "istio.io/api/routing/v1alpha1"
	"istio.io/istio/pilot/pkg/model"
	authn_plugin "istio.io/istio/pilot/pkg/networking/plugin/authn"
)

func isDestinationExcludedForMTLS(serviceName string, mtlsExcludedServices []string) bool {
	hostname, _, _ := model.ParseServiceKey(serviceName)
	for _, serviceName := range mtlsExcludedServices {
		if hostname.String() == serviceName {
			return true
		}
	}
	return false
}

// ApplyClusterPolicy assumes an outbound cluster and inserts custom configuration for the cluster
func ApplyClusterPolicy(cluster *Cluster,
	proxyInstances []*model.ServiceInstance,
	config model.IstioConfigStore,
	mesh *meshconfig.MeshConfig,
	accounts model.ServiceAccounts) {
	duration := protoDurationToMS(mesh.ConnectTimeout)
	cluster.ConnectTimeoutMs = duration

	// skip remaining policies for non mesh-local outbound clusters
	if !cluster.outbound {
		return
	}

	// Original DST cluster are used to route to services outside the mesh
	// where Istio auth does not apply.
	if cluster.Type != ClusterTypeOriginalDST {
		requireTLS, _ := authn_plugin.RequireTLS(model.GetConsolidateAuthenticationPolicy(mesh, config, model.Hostname(cluster.Hostname), cluster.Port))
		if !isDestinationExcludedForMTLS(cluster.ServiceName, mesh.MtlsExcludedServices) && requireTLS {
			// apply auth policies
			ports := model.PortList{cluster.Port}.GetNames()
			serviceAccounts := accounts.GetIstioServiceAccounts(model.Hostname(cluster.Hostname), ports)
			cluster.SSLContext = buildClusterSSLContext(model.AuthCertsPath, serviceAccounts)
		}
	}

	// apply destination policies
	policyConfig := config.Policy(proxyInstances, cluster.Hostname, cluster.labels)
	if policyConfig == nil {
		return
	}

	policy := policyConfig.Spec.(*routing.DestinationPolicy)

	// Load balancing policies do not apply for Original DST clusters
	// as the intent is to go directly to the instance.
	if policy.LoadBalancing != nil && cluster.Type != ClusterTypeOriginalDST {
		switch policy.LoadBalancing.GetName() {
		case routing.LoadBalancing_ROUND_ROBIN:
			cluster.LbType = LbTypeRoundRobin
		case routing.LoadBalancing_LEAST_CONN:
			cluster.LbType = LbTypeLeastRequest
		case routing.LoadBalancing_RANDOM:
			cluster.LbType = LbTypeRandom
		}
	}

	// Set up circuit breakers and outlier detection
	if policy.CircuitBreaker != nil && policy.CircuitBreaker.GetSimpleCb() != nil {
		cbconfig := policy.CircuitBreaker.GetSimpleCb()
		cluster.MaxRequestsPerConnection = int(cbconfig.HttpMaxRequestsPerConnection)

		// Envoy's circuit breaker is a combination of its circuit breaker (which is actually a bulk head)
		// outlier detection (which is per pod circuit breaker)
		cluster.CircuitBreaker = &CircuitBreaker{}
		if cbconfig.MaxConnections > 0 {
			cluster.CircuitBreaker.Default.MaxConnections = int(cbconfig.MaxConnections)
		}
		if cbconfig.HttpMaxRequests > 0 {
			cluster.CircuitBreaker.Default.MaxRequests = int(cbconfig.HttpMaxRequests)
		}
		if cbconfig.HttpMaxPendingRequests > 0 {
			cluster.CircuitBreaker.Default.MaxPendingRequests = int(cbconfig.HttpMaxPendingRequests)
		}
		if cbconfig.HttpMaxRetries > 0 {
			cluster.CircuitBreaker.Default.MaxRetries = int(cbconfig.HttpMaxRetries)
		}

		cluster.OutlierDetection = &OutlierDetection{}

		cluster.OutlierDetection.MaxEjectionPercent = 10
		if cbconfig.SleepWindow.Seconds > 0 {
			cluster.OutlierDetection.BaseEjectionTimeMS = protoDurationToMS(cbconfig.SleepWindow)
		}
		if cbconfig.HttpConsecutiveErrors > 0 {
			cluster.OutlierDetection.ConsecutiveErrors = int(cbconfig.HttpConsecutiveErrors)
		}
		if cbconfig.HttpDetectionInterval.Seconds > 0 {
			cluster.OutlierDetection.IntervalMS = protoDurationToMS(cbconfig.HttpDetectionInterval)
		}
		if cbconfig.HttpMaxEjectionPercent > 0 {
			cluster.OutlierDetection.MaxEjectionPercent = int(cbconfig.HttpMaxEjectionPercent)
		}
	}
}
