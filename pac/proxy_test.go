// Copyright 2023 Sauce Labs Inc. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package pac

import (
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestProxies(t *testing.T) {
	tests := []struct {
		input string
		want  []Proxy
	}{
		{"", nil},
		{"DIRECT", []Proxy{{Mode: DIRECT}}},
		{"PROXY w3proxy.netscape.com:8080; PROXY mozilla.netscape.com:8081", []Proxy{
			{Mode: PROXY, Host: "w3proxy.netscape.com", Port: "8080"},
			{Mode: PROXY, Host: "mozilla.netscape.com", Port: "8081"},
		}},
		{"PROXY w3proxy.netscape.com:8080; PROXY mozilla.netscape.com:8081", []Proxy{
			{Mode: PROXY, Host: "w3proxy.netscape.com", Port: "8080"},
			{Mode: PROXY, Host: "mozilla.netscape.com", Port: "8081"},
		}},
		{"PROXY w3proxy.netscape.com:8080; PROXY mozilla.netscape.com:8081; DIRECT", []Proxy{
			{Mode: PROXY, Host: "w3proxy.netscape.com", Port: "8080"},
			{Mode: PROXY, Host: "mozilla.netscape.com", Port: "8081"},
			{Mode: DIRECT},
		}},
		{"PROXY w3proxy.netscape.com:8080; SOCKS socks:1080", []Proxy{
			{Mode: PROXY, Host: "w3proxy.netscape.com", Port: "8080"},
			{Mode: SOCKS, Host: "socks", Port: "1080"},
		}},
		{"SOCKS socks:1080; SOCKS4 socks4:1080; SOCKS5 socks5:1080", []Proxy{
			{Mode: SOCKS, Host: "socks", Port: "1080"},
			{Mode: SOCKS4, Host: "socks4", Port: "1080"},
			{Mode: SOCKS5, Host: "socks5", Port: "1080"},
		}},
	}

	for i := range tests {
		tc := tests[i]
		t.Run(tc.input, func(t *testing.T) {
			all, err := Proxies(tc.input).All()
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, all); diff != "" {
				t.Errorf("(-want +all)\n%s", diff)
			}
			if len(all) > 0 {
				first, err := Proxies(tc.input).First()
				if err != nil {
					t.Fatal(err)
				}
				if diff := cmp.Diff(tc.want[0], first); diff != "" {
					t.Errorf("(-want +all)\n%s", diff)
				}
			}
		})
	}
}
