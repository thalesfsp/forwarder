// Copyright 2023 Sauce Labs Inc. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package httpexpect

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
)

type Client struct {
	t       *testing.T
	rt      http.RoundTripper
	baseURL string
}

func NewClient(t *testing.T, baseURL string, rt http.RoundTripper) *Client {
	t.Helper()

	return &Client{
		t:       t,
		rt:      rt,
		baseURL: baseURL,
	}
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(req)

	// There is a difference between sending HTTP and HTTPS requests.
	// For HTTPS client issues a CONNECT request to the proxy and then sends the original request.
	// In case the proxy responds with status code 4XX or 5XX to the CONNECT request, the client interprets it as URL error.
	//
	// This is to cover this case.
	if req.URL.Scheme == "https" && err != nil {
		for i := 400; i < 600; i++ {
			if err.Error() == http.StatusText(i) {
				return &http.Response{
					StatusCode: i,
					Status:     http.StatusText(i),
					ProtoMajor: 1,
					ProtoMinor: 1,
					Header:     http.Header{},
					Body:       http.NoBody,
					Request:    req,
				}, nil
			}
		}
	}

	return resp, err
}

func (c *Client) GET(path string, opts ...func(*http.Request)) *Response {
	return c.request("GET", path, opts...)
}

func (c *Client) HEAD(path string, opts ...func(*http.Request)) *Response {
	return c.request("HEAD", path, opts...)
}

func (c *Client) request(method, path string, opts ...func(*http.Request)) *Response {
	req, err := http.NewRequestWithContext(context.Background(), method, fmt.Sprintf("%s%s", c.baseURL, path), http.NoBody)
	if err != nil {
		c.t.Fatalf("Failed to create request %s, %s: %v", method, path, err)
	}
	for _, opt := range opts {
		opt(req)
	}
	resp, err := c.do(req)
	if err != nil {
		c.t.Fatalf("Failed to execute request %s, %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("Failed to read body from %s, %s: %v", method, path, err)
	}
	return &Response{Response: resp, body: b, t: c.t}
}

type Response struct {
	*http.Response
	body []byte
	t    *testing.T
}

func (r *Response) ExpectStatus(status int) *Response {
	if r.StatusCode != status {
		r.t.Fatalf("%s, %s: expected status %d, got %d", r.Request.Method, r.Request.URL, status, r.StatusCode)
	}
	return r
}

func (r *Response) ExpectHeader(key, value string) *Response {
	if v := r.Header.Get(key); v != value {
		r.t.Fatalf("%s, %s: expected header %s to equal '%s', got '%s'", r.Request.Method, r.Request.URL, key, value, v)
	}
	return r
}

func (r *Response) ExpectBodySize(expectedSize int) *Response {
	if bodySize := len(r.body); bodySize != expectedSize {
		r.t.Fatalf("%s, %s: expected body size %d, got %d", r.Request.Method, r.Request.URL, expectedSize, bodySize)
	}
	return r
}

func (r *Response) ExpectBodyContent(content string) *Response {
	if b := string(r.body); b != content {
		r.t.Fatalf("%s, %s: expected body to equal '%s', got '%s'", r.Request.Method, r.Request.URL, content, b)
	}
	return r
}