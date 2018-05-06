// Copyright 2017 Istio Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package env

import (
	"encoding/base64"
	"fmt"
	"os"
	"text/template"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
)

type confParam struct {
	ClientPort         uint16
	ServerPort         uint16
	TCPProxyPort       uint16
	AdminPort          uint16
	MixerServer        string
	Backend            string
	ClientConfig       string
	ServerConfig       string
	TCPServerConfig    string
	AccessLog          string
	MixerRouteFlags    string
	FiltersBeforeMixer string

	// Ports contains the allocated ports.
	Ports    *Ports
	IstioSrc string
	IstioOut string

	// Options are additional config options for the template
	Options map[string]interface{}
}

// TODO: convert to v2, real clients use bootstrap v2 and all configs are switching !!!
// The envoy config template
const envoyConfTempl = `
{
  "listeners": [
    {
      "address": "tcp://0.0.0.0:{{.ServerPort}}",
      "bind_to_port": true,
      "filters": [
        {
          "type": "read",
          "name": "http_connection_manager",
          "config": {
            "codec_type": "auto",
            "stat_prefix": "ingress_http",
            "route_config": {
              "virtual_hosts": [
                {
                  "name": "backend",
                  "domains": ["*"],
                  "routes": [
                    {
                      "timeout_ms": 0,
                      "prefix": "/",
                      "cluster": "service1",
                      "opaque_config": {
{{.MixerRouteFlags}}
                      }
                    }
                  ]
                }
              ]
            },
            "access_log": [
              {
                "path": "{{.AccessLog}}"
              }
            ],
            "filters": [
{{.FiltersBeforeMixer}}
              {
                "type": "decoder",
                "name": "mixer",
                "config": {
{{.ServerConfig}}
                }
              },
              {
                "type": "decoder",
                "name": "router",
                "config": {}
              }
            ]
          }
        }
      ]
    },
    {
      "address": "tcp://0.0.0.0:{{.ClientPort}}",
      "bind_to_port": true,
      "filters": [
        {
          "type": "read",
          "name": "http_connection_manager",
          "config": {
            "codec_type": "auto",
            "stat_prefix": "ingress_http",
            "route_config": {
              "virtual_hosts": [
                {
                  "name": "backend",
                  "domains": ["*"],
                  "routes": [
                    {
                      "timeout_ms": 0,
                      "prefix": "/",
                      "cluster": "service2",
                      "opaque_config": {
                      }
                    }
                  ]
                }
              ]
            },
            "access_log": [
              {
                "path": "{{.AccessLog}}"
              }
            ],
            "filters": [
              {
                "type": "decoder",
                "name": "mixer",
                "config": {
{{.ClientConfig}}
                }
              },
              {
                "type": "decoder",
                "name": "router",
                "config": {}
              }
            ]
          }
        }
      ]
    },
    {
      "address": "tcp://0.0.0.0:{{.TCPProxyPort}}",
      "bind_to_port": true,
      "filters": [
        {
          "type": "both",
          "name": "mixer",
          "config": {
{{.TCPServerConfig}}
          }
        },
        {
          "type": "read",
          "name": "tcp_proxy",
          "config": {
            "stat_prefix": "tcp",
            "route_config": {
              "routes": [
                {
                  "cluster": "service1"
                }
              ]
            }
          }
        }
      ]
    }
  ],
  "admin": {
    "access_log_path": "/dev/stdout",
    "address": "tcp://0.0.0.0:{{.AdminPort}}"
  },
  "cluster_manager": {
    "clusters": [
      {
        "name": "service1",
        "connect_timeout_ms": 5000,
        "type": "strict_dns",
        "lb_type": "round_robin",
        "hosts": [
          {
            "url": "tcp://{{.Backend}}"
          }
        ]
      },
      {
        "name": "service2",
        "connect_timeout_ms": 5000,
        "type": "strict_dns",
        "lb_type": "round_robin",
        "hosts": [
          {
            "url": "tcp://localhost:{{.ServerPort}}"
          }
        ]
      },
      {
        "name": "mixer_server",
        "connect_timeout_ms": 5000,
        "type": "strict_dns",
	"circuit_breakers": {
           "default": {
	      "max_pending_requests": 10000,
	      "max_requests": 10000
            }
	},
        "lb_type": "round_robin",
        "features": "http2",
        "hosts": [
          {
            "url": "tcp://{{.MixerServer}}"
          }
        ]
      }
    ]
  }
}
`

func (c *confParam) write(outPath, confTmpl string) error {
	tmpl, err := template.New("test").Parse(confTmpl)
	if err != nil {
		return fmt.Errorf("failed to parse config template: %v", err)
	}

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("failed to create file %v: %v", outPath, err)
	}
	defer func() {
		_ = f.Close()
	}()
	return tmpl.Execute(f, *c)
}

// CreateEnvoyConf create envoy config.
func (s *TestSetup) CreateEnvoyConf(path string, stress bool, filtersBeforeMixer string, mfConfig *MixerFilterConf, ports *Ports,
	confVersion string) error {
	c := &confParam{
		ClientPort:      ports.ClientProxyPort,
		ServerPort:      ports.ServerProxyPort,
		TCPProxyPort:    ports.TCPProxyPort,
		AdminPort:       ports.AdminPort,
		MixerServer:     fmt.Sprintf("localhost:%d", ports.MixerPort),
		Backend:         fmt.Sprintf("localhost:%d", ports.BackendPort),
		AccessLog:       "/dev/stdout",
		ServerConfig:    getConfig(mfConfig.HTTPServerConf, confVersion),
		ClientConfig:    getConfig(mfConfig.HTTPClientConf, confVersion),
		TCPServerConfig: getConfig(mfConfig.TCPServerConf, confVersion),
		MixerRouteFlags: getPerRouteConfig(mfConfig.PerRouteConf),
		Ports:           ports,
		IstioSrc:        s.IstioSrc,
		IstioOut:        s.IstioOut,
		Options:         s.EnvoyConfigOpt,
	}
	// TODO: use fields from s directly instead of copying

	if stress {
		c.AccessLog = "/dev/null"
	}
	if len(filtersBeforeMixer) > 0 {
		c.FiltersBeforeMixer = filtersBeforeMixer
	}

	confTmpl := envoyConfTempl
	if s.EnvoyTemplate != "" {
		confTmpl = s.EnvoyTemplate
	}
	return c.write(path, confTmpl)
}

func getConfig(mixerFilterConfig proto.Message, configVersion string) string {
	m := jsonpb.Marshaler{
		Indent: "  ",
	}
	str, err := m.MarshalToString(mixerFilterConfig)
	if err != nil {
		return ""
	}
	return configVersion + str
}

func getPerRouteConfig(cfg proto.Message) string {
	m := jsonpb.Marshaler{}
	str, err := m.MarshalToString(cfg)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("\"mixer_sha\": \"id111\", \"mixer\": \"%v\"",
		base64.StdEncoding.EncodeToString([]byte(str)))
}
