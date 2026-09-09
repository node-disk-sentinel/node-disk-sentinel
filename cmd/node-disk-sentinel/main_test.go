// Copyright 2026 Volker Theile
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewServerMux(t *testing.T) {
	tests := []struct {
		name           string
		metricsEnabled bool
		readyFunc      func() bool
		requestPath    string
		expectedStatus int
	}{
		{
			name:           "healthz with metrics enabled",
			metricsEnabled: true,
			requestPath:    "/healthz",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "healthz with metrics disabled",
			metricsEnabled: false,
			requestPath:    "/healthz",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "readyz when ready",
			metricsEnabled: true,
			readyFunc:      func() bool { return true },
			requestPath:    "/readyz",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "readyz when not ready",
			metricsEnabled: true,
			readyFunc:      func() bool { return false },
			requestPath:    "/readyz",
			expectedStatus: http.StatusServiceUnavailable,
		},
		{
			name:           "readyz with nil readyFunc",
			metricsEnabled: true,
			readyFunc:      nil,
			requestPath:    "/readyz",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "metrics enabled",
			metricsEnabled: true,
			requestPath:    "/metrics",
			expectedStatus: http.StatusOK,
		},
		{
			name:           "metrics disabled",
			metricsEnabled: false,
			requestPath:    "/metrics",
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mux := newServerMux(tt.metricsEnabled, tt.readyFunc)
			req := httptest.NewRequest(http.MethodGet, tt.requestPath, nil)
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if rec.Code != tt.expectedStatus {
				t.Fatalf("expected status %d for path %s (metricsEnabled=%v), got %d",
					tt.expectedStatus, tt.requestPath, tt.metricsEnabled, rec.Code)
			}
		})
	}
}

func TestOptions_MetricsEnabledFlag(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		expected bool
	}{
		{
			name:     "default value is true",
			args:     []string{},
			expected: true,
		},
		{
			name:     "explicitly set to false",
			args:     []string{"--metrics-enabled=false"},
			expected: false,
		},
		{
			name:     "explicitly set to true",
			args:     []string{"--metrics-enabled=true"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			opts := &options{}

			fs.BoolVar(&opts.metricsEnabled, "metrics-enabled", true, "Expose Prometheus metrics endpoint")
			if err := fs.Parse(tt.args); err != nil {
				t.Fatalf("failed to parse flags: %v", err)
			}

			if opts.metricsEnabled != tt.expected {
				t.Fatalf("expected metricsEnabled=%v, got %v", tt.expected, opts.metricsEnabled)
			}
		})
	}
}

func TestOptionsExcludeRules(t *testing.T) {
	opts := options{excludeDisks: []string{"node=worker-01,vendor=HP,model=LOGICAL_VOLUME"}}
	rules, err := opts.excludeRules()
	if err != nil {
		t.Fatalf("excludeRules() error = %v", err)
	}
	if len(rules) != 1 || rules[0].Node != "worker-01" || rules[0].Vendor != "HP" || rules[0].Model != "LOGICAL_VOLUME" {
		t.Fatalf("excludeRules() = %#v; want HP LOGICAL_VOLUME rule", rules)
	}
}
