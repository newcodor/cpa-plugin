package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// configYAMLRequest builds the lifecycle request the way the host does.
// NOTE: the host sends config_yaml base64-encoded, which is why the field in
// configure() must stay []byte (encoding/json maps []byte <-> base64 string).
// A usage_report_url is always included so resolveUsageReport skips its 2s
// network probe.
func configYAMLRequest(t *testing.T, yaml string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"config_yaml": base64.StdEncoding.EncodeToString([]byte(
			yaml + "\nusage_report_url: http://127.0.0.1:1/x\n")),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// TestSystemRedact_ConfigYAMLIsSourceOfTruth pins the contract: system_redact
// is driven solely by config_yaml (the CPA plugin-config editor). There is no
// runtime setter on purpose — a panel toggle could not persist to config_yaml
// and would be clobbered by the next reconfigure.
func TestSystemRedact_ConfigYAMLIsSourceOfTruth(t *testing.T) {
	orig := systemRedactEnabled()
	defer func() {
		systemRedactMu.Lock()
		systemRedact = orig
		systemRedactMu.Unlock()
	}()

	cases := []struct {
		yaml string
		want bool
	}{
		{"system_redact: true", true},
		{"system_redact: false", false},
		{"system_redact: 1", true},
		{"system_redact: 0", false},
		{"system_redact: yes", true},
		{"system_redact: on", true},
		{"system_redact: off", false},
		{"# nothing set", false}, // absent key → default off
	}
	for _, c := range cases {
		configure(configYAMLRequest(t, c.yaml))
		if got := systemRedactEnabled(); got != c.want {
			t.Errorf("config_yaml %q → systemRedactEnabled()=%v, want %v", c.yaml, got, c.want)
		}
	}
}

// TestSystemRedact_NoRuntimeSetter guards the design decision: there must be
// no /system-redact/config route, so the panel cannot (and must not) flip it.
func TestSystemRedact_NoRuntimeSetter(t *testing.T) {
	reg := managementRegistration()
	for _, r := range reg.Routes {
		if r.Path == "/plugins/"+providerName+"/system-redact/config" {
			t.Errorf("route %s must NOT be registered: system_redact is read-only "+
				"in the panel and driven only by config_yaml", r.Path)
		}
	}
	if mutatingManagementPath("/v0/management/plugins/workbuddy/system-redact/config") {
		t.Error("/system-redact/config must not be a mutating path (endpoint removed)")
	}
}
