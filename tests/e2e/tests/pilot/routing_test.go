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

package pilot

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"istio.io/istio/pkg/log"
	"istio.io/istio/tests/util"
)

func TestRoutes(t *testing.T) {
	samples := 100

	var cfgs *deployableConfig
	applyRuleFunc := func(t *testing.T, ruleYaml string) {
		// Delete the previous rule if there was one. No delay on the teardown, since we're going to apply
		// a delay when we push the new config.
		if cfgs != nil {
			if err := cfgs.TeardownNoDelay(); err != nil {
				t.Fatal(err)
			}
			cfgs = nil
		}

		// Apply the new rule
		cfgs = &deployableConfig{
			Namespace:  tc.Kube.Namespace,
			YamlFiles:  []string{ruleYaml},
			kubeconfig: tc.Kube.KubeConfig,
		}
		if err := cfgs.Setup(); err != nil {
			t.Fatal(err)
		}
	}
	// Upon function exit, delete the active rule.
	defer func() {
		if cfgs != nil {
			_ = cfgs.Teardown()
		}
	}()

	cases := []struct {
		testName      string
		description   string
		config        string
		scheme        string
		src           string
		dst           string
		headerKey     string
		headerVal     string
		expectedCount map[string]int
		operation     string
		onFailure     func()
	}{
		{
			// First test default routing
			testName:      "a->c[v1=100]",
			description:   "routing all traffic to c-v1",
			config:        "rule-default-route.yaml",
			scheme:        "http",
			src:           "a",
			dst:           "c",
			headerKey:     "",
			headerVal:     "",
			expectedCount: map[string]int{"v1": 100, "v2": 0},
			operation:     "default-route",
		},
		{
			testName:      "a->c[v1=75,v2=25]",
			description:   "routing 75 percent to c-v1, 25 percent to c-v2",
			config:        "rule-weighted-route.yaml",
			scheme:        "http",
			src:           "a",
			dst:           "c",
			headerKey:     "",
			headerVal:     "",
			expectedCount: map[string]int{"v1": 75, "v2": 25},
		},
		{
			testName:      "a->c[v2=100]_header",
			description:   "routing 100 percent to c-v2 using header",
			config:        "rule-content-route.yaml",
			scheme:        "http",
			src:           "a",
			dst:           "c",
			headerKey:     "version",
			headerVal:     "v2",
			expectedCount: map[string]int{"v1": 0, "v2": 100},
			operation:     "",
		},
		{
			testName:      "a->c[v2=100]_regex_header",
			description:   "routing 100 percent to c-v2 using regex header",
			config:        "rule-regex-route.yaml",
			scheme:        "http",
			src:           "a",
			dst:           "c",
			headerKey:     "foo",
			headerVal:     "bar",
			expectedCount: map[string]int{"v1": 0, "v2": 100},
			operation:     "",
			onFailure: func() {
				op, err := tc.Kube.GetRoutes("a")
				log.Infof("error: %v\n%s", err, op)
				cfg, err := util.GetConfigs("destinationrules.networking.istio.io",
					"virtualservices.networking.istio.io", "serviceentries.networking.istio.io",
					"policies.authentication.istio.io")

				log.Infof("config: %v\n%s", err, cfg)
			},
		},
		// In case of websockets, the server does not return headers as part of response.
		// After upgrading to websocket connection, it waits for a dummy message from the
		// client over the websocket connection. It then returns all the headers as
		// part of the response message which is then printed out by the client.
		// So the verify checks here are really parsing the output of a websocket message
		// i.e., we are effectively checking websockets beyond just the upgrade.
		{
			testName:      "a->c[v1=100]_websocket",
			description:   "routing 100 percent to c-v1 with websocket upgrades",
			config:        "rule-websocket-route.yaml",
			scheme:        "ws",
			src:           "a",
			dst:           "c",
			headerKey:     "testwebsocket",
			headerVal:     "enabled",
			expectedCount: map[string]int{"v1": 100, "v2": 0},
			operation:     "",
		},
		{
			testName:      "a->c[v1=100]_append_headers",
			description:   "routing all traffic to c-v1 with appended headers",
			config:        "rule-default-route-append-headers.yaml",
			scheme:        "http",
			src:           "a",
			dst:           "c",
			headerKey:     "",
			headerVal:     "",
			expectedCount: map[string]int{"v1": 100, "v2": 0},
			operation:     "default-route",
		},
		{
			testName:      "a->c[v1=100]_CORS_policy",
			description:   "routing all traffic to c-v1 with CORS policy",
			config:        "rule-default-route-cors-policy.yaml",
			scheme:        "http",
			src:           "a",
			dst:           "c",
			headerKey:     "",
			headerVal:     "",
			expectedCount: map[string]int{"v1": 100, "v2": 0},
			operation:     "default-route",
		},
	}

	for _, version := range configVersions() {
		t.Run(version, func(t *testing.T) {
			if version == "v1alpha3" {
				destRule := "testdata/v1alpha3/destination-rule-c.yaml"
				cfgs := &deployableConfig{
					Namespace:  tc.Kube.Namespace,
					YamlFiles:  []string{destRule},
					kubeconfig: tc.Kube.KubeConfig,
				}
				if err := cfgs.Setup(); err != nil {
					t.Fatal(err)
				}
				// Teardown after, but no need to wait, since a delay will be applied by either the next rule's
				// Setup() or the Teardown() for the final rule.
				defer cfgs.TeardownNoDelay()
			}

			for _, c := range cases {
				if strings.Contains(c.testName, "websocket") && version == "v1alpha3" {
					log.Infof("Skipping Websocket tests in v1alpha3 as they are not implemented yet")
					continue
				}

				// Run each case in a function to scope the configuration's lifecycle.
				func() {
					ruleYaml := fmt.Sprintf("testdata/%s/%s", version, c.config)
					applyRuleFunc(t, ruleYaml)

					runRetriableTest(t, c.testName, 5, func() error {
						reqURL := fmt.Sprintf("%s://%s/%s", c.scheme, c.dst, c.src)
						resp := ClientRequest(c.src, reqURL, samples, fmt.Sprintf("-key %s -val %s", c.headerKey, c.headerVal))
						count := make(map[string]int)
						for _, elt := range resp.Version {
							count[elt] = count[elt] + 1
						}
						log.Infof("request counts %v", count)
						epsilon := 10

						for version, expected := range c.expectedCount {
							if count[version] > expected+epsilon || count[version] < expected-epsilon {
								return fmt.Errorf("expected %v requests (+/-%v) to reach %s => Got %v",
									expected, epsilon, version, count[version])
							}
						}

						if c.operation != "" {
							response := ClientRequest(
								"t",
								fmt.Sprintf("http://zipkin.%s:9411/api/v1/traces", tc.Kube.Namespace),
								1, "",
							)

							if !response.IsHTTPOk() {
								return fmt.Errorf("could not retrieve traces from zipkin")
							}

							text := fmt.Sprintf("\"name\":\"%s\"", c.operation)
							if strings.Count(response.Body, text) != 10 {
								return fmt.Errorf("could not find operation %q in zipkin traces", c.operation)
							}
						}

						return nil
					}, c.onFailure)
				}()
			}
		})
	}
}

func TestRouteFaultInjection(t *testing.T) {
	for _, version := range configVersions() {
		// Invoke a function to scope the lifecycle of the deployed configs.
		func() {
			if version == "v1alpha3" {
				destRule := "testdata/v1alpha3/destination-rule-c.yaml"
				dRule := &deployableConfig{
					Namespace:  tc.Kube.Namespace,
					YamlFiles:  []string{destRule},
					kubeconfig: tc.Kube.KubeConfig,
				}
				if err := dRule.Setup(); err != nil {
					t.Fatal(err)
				}
				// Teardown after, but no need to wait, since a delay will be applied by either the next rule's
				// Setup() or the Teardown() for the final rule.
				defer dRule.TeardownNoDelay()
			}

			ruleYaml := fmt.Sprintf("testdata/%s/rule-fault-injection.yaml", version)
			cfgs := &deployableConfig{
				Namespace:  tc.Kube.Namespace,
				YamlFiles:  []string{ruleYaml},
				kubeconfig: tc.Kube.KubeConfig,
			}
			if err := cfgs.Setup(); err != nil {
				t.Fatal(err)
			}
			defer cfgs.Teardown()

			runRetriableTest(t, version, 5, func() error {
				reqURL := "http://c/a"

				start := time.Now()
				resp := ClientRequest("a", reqURL, 1, "-key version -val v2")
				elapsed := time.Since(start)

				statusCode := ""
				if len(resp.Code) > 0 {
					statusCode = resp.Code[0]
				}

				respCode := 503
				respTime := time.Second * 5
				epsilon := time.Second * 2 // +/- 2s variance
				if elapsed > respTime+epsilon || elapsed < respTime-epsilon || strconv.Itoa(respCode) != statusCode {
					return fmt.Errorf("fault injection verification failed: "+
						"response time is %s with status code %s, "+
						"expected response time is %s +/- %s with status code %d", elapsed, statusCode, respTime, epsilon, respCode)
				}
				return nil
			})
		}()
	}
}

func TestRouteRedirectInjection(t *testing.T) {
	for _, version := range configVersions() {
		// Invoke a function to scope the lifecycle of the deployed configs.
		func() {
			// Push the rule config.
			ruleYaml := fmt.Sprintf("testdata/%s/rule-redirect-injection.yaml", version)
			cfgs := &deployableConfig{
				Namespace:  tc.Kube.Namespace,
				YamlFiles:  []string{ruleYaml},
				kubeconfig: tc.Kube.KubeConfig,
			}
			if err := cfgs.Setup(); err != nil {
				t.Fatal(err)
			}
			defer cfgs.Teardown()

			runRetriableTest(t, version, 5, func() error {
				targetHost := "b"
				targetPath := "/new/path"

				reqURL := "http://c/a"
				resp := ClientRequest("a", reqURL, 1, "-key testredirect -val enabled")
				if !resp.IsHTTPOk() {
					return fmt.Errorf("redirect failed: response status code: %v, expected 200", resp.Code)
				}

				var host string
				if matches := regexp.MustCompile("(?i)Host=(.*)").FindStringSubmatch(resp.Body); len(matches) >= 2 {
					host = matches[1]
				}
				if host != targetHost {
					return fmt.Errorf("redirect failed: response body contains Host=%v, expected Host=%v", host, targetHost)
				}

				exp := regexp.MustCompile("(?i)URL=(.*)")
				paths := exp.FindAllStringSubmatch(resp.Body, -1)
				var path string
				if len(paths) > 1 {
					path = paths[1][1]
				}
				if path != targetPath {
					return fmt.Errorf("redirect failed: response body contains URL=%v, expected URL=%v", path, targetPath)
				}

				return nil
			})
		}()
	}
}

// TODO this is not implemented properly at the moment.
func TestRouteMirroring(t *testing.T) {
	t.Skipf("Skipping %s due to incomplete implementation", t.Name())
	for _, version := range configVersions() {
		logs := newAccessLogs()
		// Invoke a function to scope the lifecycle of the deployed configs.
		func() {
			// Push the rule config.
			ruleYaml := fmt.Sprintf("testdata/%s/rule-default-route-mirrored.yaml", version)
			cfgs := &deployableConfig{
				Namespace:  tc.Kube.Namespace,
				YamlFiles:  []string{ruleYaml},
				kubeconfig: tc.Kube.KubeConfig,
			}
			if err := cfgs.Setup(); err != nil {
				t.Fatal(err)
			}
			defer cfgs.Teardown()

			reqURL := "http://c/a"
			for i := 1; i <= 100; i++ {
				resp := ClientRequest("a", reqURL, 1, fmt.Sprintf("-key X-Request-Id -val %d", i))
				logEntry := fmt.Sprintf("HTTP request from a to c.istio-system.svc.cluster.local:80")
				if len(resp.ID) > 0 {
					id := resp.ID[0]
					logs.add("b", id, logEntry)
				}
			}

			t.Run("check", func(t *testing.T) {
				logs.checkLogs(t)
			})
		}()
	}
}
