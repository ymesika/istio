// Copyright 2018 Istio Authors. All Rights Reserved.
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

package istioAuthnPeerJwtBoundPeer

import (
	"encoding/base64"
	"fmt"
	"testing"

	"istio.io/istio/mixer/test/client/env"
)

// Check attributes from a good GET request
const checkAttributesOkGet = `
{
  "context.protocol": "http",
  "mesh1.ip": "[1 1 1 1]",
  "mesh2.ip": "[0 0 0 0 0 0 0 0 0 0 255 255 204 152 189 116]",
  "mesh3.ip": "[0 1 0 0 0 0 0 0 0 0 0 0 0 0 0 8]",
  "request.host": "*",
  "request.path": "/echo",
  "request.time": "*",
  "request.useragent": "Go-http-client/1.1",
  "request.method": "GET",
  "request.scheme": "http",
  "source.uid": "POD11",
  "source.namespace": "XYZ11",
  "source.ip": "[127 0 0 1]",
  "source.port": "*",
  "source.principal": "issuer@foo.com/sub@foo.com",
  "source.user": "issuer@foo.com/sub@foo.com",
  "target.name": "target-name",
  "target.user": "target-user",
  "target.uid": "POD222",
  "target.namespace": "XYZ222",
  "connection.mtls": false,
  "request.headers": {
     ":method": "GET",
     ":path": "/echo",
     ":authority": "*",
     "x-forwarded-proto": "http",
     "x-istio-attributes": "-",
     "x-request-id": "*"
  },
  "request.auth.audiences": "aud1",
  "request.auth.principal": "issuer@foo.com/sub@foo.com",
  "request.auth.claims": {
     "iss": "issuer@foo.com", 
     "sub": "sub@foo.com",
     "aud": "aud1",
     "some-other-string-claims": "some-claims-kept"
  }
}
`

// Report attributes from a good GET request with Istio authn policy
const reportAttributesOkGet = `
{
  "context.protocol": "http",
  "mesh1.ip": "[1 1 1 1]",
  "mesh2.ip": "[0 0 0 0 0 0 0 0 0 0 255 255 204 152 189 116]",
  "mesh3.ip": "[0 1 0 0 0 0 0 0 0 0 0 0 0 0 0 8]",
  "request.host": "*",
  "request.path": "/echo",
  "request.time": "*",
  "request.useragent": "Go-http-client/1.1",
  "request.method": "GET",
  "request.scheme": "http",
  "source.uid": "POD11",
  "source.namespace": "XYZ11",
  "source.ip": "[127 0 0 1]",
  "source.port": "*",
  "source.principal": "issuer@foo.com/sub@foo.com",
  "source.user": "issuer@foo.com/sub@foo.com",
  "destination.ip": "[127 0 0 1]",
  "destination.port": "*",
  "target.name": "target-name",
  "target.user": "target-user",
  "target.uid": "POD222",
  "target.namespace": "XYZ222",
  "connection.mtls": false,
  "check.cache_hit": false,
  "quota.cache_hit": false,
  "request.headers": {
     ":method": "GET",
     ":path": "/echo",
     ":authority": "*",
     "x-forwarded-proto": "http",
     "x-istio-attributes": "-",
     "x-request-id": "*"
  },
  "request.size": 0,
  "request.total_size": 589,
  "response.total_size": 99,
  "response.time": "*",
  "response.size": 0,
  "response.duration": "*",
  "response.code": 200,
  "response.headers": {
     "date": "*",
     "content-length": "0",
     ":status": "200",
     "server": "envoy"
  },
  "request.auth.audiences": "aud1",
  "request.auth.principal": "issuer@foo.com/sub@foo.com",
  "request.auth.claims": {
     "iss": "issuer@foo.com", 
     "sub": "sub@foo.com",
     "aud": "aud1",
     "some-other-string-claims": "some-claims-kept"
  }
}
`

// The Istio authn envoy config
const authnConfig = `
{
  "type": "decoder",
  "name": "istio_authn",
  "config": {
    "policy": {
      "peers": [
        {
          "jwt": {
            "issuer": "issuer@foo.com",
            "jwks_uri": "http://localhost:8081/"
          }
        }
      ],
      "principal_binding": 0
    },
    "jwt_output_payload_locations": {
      "issuer@foo.com": "sec-istio-auth-jwt-output"
    }
  }
},
`

const secIstioAuthUserInfoHeaderKey = "sec-istio-auth-jwt-output"

const secIstioAuthUserinfoHeaderValue = `
{
  "iss": "issuer@foo.com",
  "sub": "sub@foo.com",
  "aud": "aud1",
  "non-string-will-be-ignored": 1512754205,
  "some-other-string-claims": "some-claims-kept"
}
`

func TestAuthnCheckReportAttributesPeerJwtBoundToPeer(t *testing.T) {
	s := env.NewTestSetup(env.CheckReportIstioAuthnAttributesTestPeerJwtBoundToPeer, t)
	// In the Envoy config, principal_binding binds to peer
	s.SetFiltersBeforeMixer(authnConfig)

	env.SetStatsUpdateInterval(s.MfConfig(), 1)
	if err := s.SetUp(); err != nil {
		t.Fatalf("Failed to setup test: %v", err)
	}
	defer s.TearDown()

	url := fmt.Sprintf("http://localhost:%d/echo", s.Ports().ClientProxyPort)

	// Issues a GET echo request with 0 size body
	tag := "OKGet"

	// Add jwt_auth header to be consumed by Istio authn filter
	headers := map[string]string{}
	headers[secIstioAuthUserInfoHeaderKey] =
		base64.StdEncoding.EncodeToString([]byte(secIstioAuthUserinfoHeaderValue))

	if _, _, err := env.HTTPGetWithHeaders(url, headers); err != nil {
		t.Errorf("Failed in request %s: %v", tag, err)
	}
	// Verify that the authn attributes in the actual check call match those in the expected check call
	s.VerifyCheck(tag, checkAttributesOkGet)
	// Verify that the authn attributes in the actual report call match those in the expected report call
	s.VerifyReport(tag, reportAttributesOkGet)
}
