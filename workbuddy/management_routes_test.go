package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestManagementRoutes_AllRegisteredHaveHandlers verifies every route the
// plugin advertises to the CPA host in managementRegistration() is actually
// dispatchable by handleManagement (i.e. does NOT fall through to 404).
//
// Background: a route added only as a switch case in handleManagement —
// without being listed in managementRegistration().Routes — is never
// forwarded by the host, so the panel gets a 404 before the plugin sees the
// request. That was the bug behind the 系统提示词脱敏 toggle: its
// /system-redact/config case existed but the route was never registered.
func TestManagementRoutes_AllRegisteredHaveHandlers(t *testing.T) {
	reg := managementRegistration()
	base := loadedManagementBasePath() + "/plugins/" + providerName

	for _, r := range reg.Routes {
		t.Run(r.Method+" "+r.Path, func(t *testing.T) {
			// Build a minimal request with the registered sub-path resolved to
			// the full host path the plugin expects.
			full := base + r.Path[len("/plugins/"+providerName):]
			raw, err := json.Marshal(pluginapi.ManagementRequest{
				Method:  r.Method,
				Path:    full,
				Body:    []byte("{}"),
				Headers: http.Header{},
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			out, err := handleManagement(raw)
			if err != nil {
				t.Fatalf("handleManagement error: %v", err)
			}
			var env envelope
			if err := json.Unmarshal(out, &env); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			var resp pluginapi.ManagementResponse
			if err := json.Unmarshal(env.Result, &resp); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if resp.StatusCode == http.StatusNotFound {
				t.Errorf("registered route %s %s was NOT dispatched by handleManagement (404)",
					r.Method, r.Path)
			}
		})
	}
}

// TestManagementRoutes_AllSwitchPathsRegistered is the reverse check — and
// the one that actually catches the original bug shape.
//
// The previous test iterates the REGISTERED routes, so a path that was never
// registered (like /system-redact/config) is simply never visited and the bug
// slips through. This test asserts the opposite direction: every path that
// handleManagement can dispatch MUST also appear in the registration list,
// otherwise the host 404s it before the plugin ever sees the request.
//
// Keep this list in sync with the switch cases in handleManagement.
func TestManagementRoutes_AllSwitchPathsRegistered(t *testing.T) {
	expect := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/accounts"},
		{http.MethodPost, "/refresh"},
		{http.MethodPost, "/checkin"},
		{http.MethodPost, "/checkin/config"},
		// /system-redact/config intentionally absent: system_redact is driven
		// only by config_yaml and has no runtime endpoint (see
		// TestSystemRedact_NoRuntimeSetter). Don't re-add without the handler.
		{http.MethodPost, "/models/reload"},
		{http.MethodGet, "/models"},
		{http.MethodPost, "/models/save"},
		{http.MethodGet, "/credits"},
		{http.MethodPost, "/import"},
		{http.MethodPost, "/trial"},
		{http.MethodPost, "/select"},
		{http.MethodPost, "/keepalive"},
		{http.MethodGet, "/keepalive/status"},
	}

	reg := managementRegistration()
	prefix := "/plugins/" + providerName
	registered := make(map[string]bool, len(reg.Routes))
	for _, r := range reg.Routes {
		registered[r.Method+" "+r.Path] = true
	}

	for _, e := range expect {
		key := e.method + " " + prefix + e.path
		if !registered[key] {
			t.Errorf("handleManagement dispatches %s but it is NOT registered in "+
				"managementRegistration().Routes — the host will return 404", key)
		}
	}
}

// The /system-redact/config route was removed on purpose: system_redact is
// driven only by config_yaml (the CPA plugin-config editor), so it has no
// runtime toggle endpoint. Its "must not exist" guard lives in
// system_redact_test.go:TestSystemRedact_NoRuntimeSetter.
