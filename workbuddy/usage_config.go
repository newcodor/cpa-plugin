// usage_config.go decodes plugin config from config_yaml on every
// register/reconfigure call and resolves the CPAMP usage report URL/key.
// All plugin-level config lives here so the rest of the plugin reads
// consistent, lock-protected snapshots.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// check-in schedule: 09:00 and 21:00 local time.
var checkinHours = []int{9, 21}

// plugin-level config decoded from plugin.register/reconfigure config_yaml.
var (
	checkinAuto   = true // enabled by default
	checkinAutoMu sync.RWMutex

	// systemRedact injects U+200B into role=system message content before the
	// request is forwarded upstream (default false — off, preserves exact prompt).
	systemRedact   = false
	systemRedactMu sync.RWMutex

	// usageReportURL / usageReportKey: POST NDJSON to CPA-Manager-Plus
	// /v0/management/usage/import (only path that reaches request monitoring;
	// c-shared plugins cannot use host usage.DefaultManager/redisqueue).
	//
	// Resolution order (community-style, like codex-auth-importer env injection):
	//  1) plugins.configs.workbuddy.usage_report_* in config.yaml
	//  2) env USAGE_REPORT_URL / USAGE_REPORT_KEY / CPAMP_ADMIN_KEY
	//  3) secret files (docker secrets / bind-mount), e.g. /run/secrets/cpamp_admin_key
	// Default URL targets the compose service name of CPA-Manager-Plus.
	usageReportURL = defaultUsageReportURL
	usageReportKey = ""
	usageReportMu  sync.RWMutex

	// managementAPIKey: plugin-layer auth for /v0/management/plugins/workbuddy/*
	// write endpoints. When empty, plugin relies on host-side auth (CPA's
	// management middleware) — that's the historical default and stays
	// backward-compatible. When set via config_yaml management_key: or env
	// WB_MANAGEMENT_KEY, handleManagement enforces constant-time Bearer match
	// plus per-IP token-bucket rate limiting on mutating endpoints.
	managementAPIKey   = ""
	managementAPIKeyMu sync.RWMutex
)

// Default URL tries localhost first (works for both bare-metal and Docker
// host-network), falls back to Docker compose service name. The probe runs
// once at configure() time; a reachable endpoint wins.
//
// For users who run CPA Manager Plus on a different host/port, set
// usage_report_url in plugin config or env USAGE_REPORT_URL.
const defaultUsageReportURL = "http://127.0.0.1:18317/v0/management/usage/import"

const fallbackUsageReportURL = "http://cpa-manager-plus:18317/v0/management/usage/import"

// configure decodes plugin config from the lifecycle request.
func configure(raw []byte) {
	// Parse config without holding any lock (fixes nested-lock hazard).
	nextCheckinAuto := true
	nextLifecycleAuto := true
	nextSchedulerMode := schedulerModeOff // reset to default on reconfigure
	nextKeepaliveAuto := true
	nextSystemRedact := false
	nextMgmtKey := ""

	// Captured for the models: block parser (parsed after the line scan).
	var configYAMLBytes []byte

	cfgURL, cfgKey := "", ""
	if len(raw) > 0 {
		var req struct {
			// []byte is correct and must stay []byte: encoding/json decodes a
			// []byte field from a base64-encoded JSON string, and the host sends
			// config_yaml base64-encoded. (Using string here would feed raw
			// base64 text to the YAML line scanner and silently break every
			// config_yaml setting.)
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &req); err == nil {
			configYAMLBytes = req.ConfigYAML
			for _, line := range strings.Split(string(req.ConfigYAML), "\n") {
				line = strings.TrimSpace(line)
				if strings.HasPrefix(line, "checkin_auto:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "checkin_auto:"))
					nextCheckinAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "lifecycle_auto:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "lifecycle_auto:"))
					v = strings.Trim(v, "\"'")
					nextLifecycleAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "scheduler_mode:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "scheduler_mode:"))
					v = strings.Trim(v, "\"'")
					if v == schedulerModeCredits {
						nextSchedulerMode = schedulerModeCredits
					}
				}
				if strings.HasPrefix(line, "usage_report_url:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "usage_report_url:"))
					cfgURL = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "usage_report_key:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "usage_report_key:"))
					cfgKey = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "management_key:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "management_key:"))
					nextMgmtKey = strings.Trim(v, "\"'")
				}
				if strings.HasPrefix(line, "token_keepalive:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "token_keepalive:"))
					v = strings.Trim(v, "\"'")
					nextKeepaliveAuto = v == "true" || v == "1" || v == "yes" || v == "on"
				}
				if strings.HasPrefix(line, "system_redact:") {
					v := strings.TrimSpace(strings.TrimPrefix(line, "system_redact:"))
					v = strings.Trim(v, "\"'")
					nextSystemRedact = v == "true" || v == "1" || v == "yes" || v == "on"
				}
			}
		}
	}

	// Apply each setting under its own lock — no nesting.
	checkinAutoMu.Lock()
	checkinAuto = nextCheckinAuto
	checkinAutoMu.Unlock()

	lifecycleAutoMu.Lock()
	lifecycleAuto = nextLifecycleAuto
	lifecycleAutoMu.Unlock()

	schedulerModeMu.Lock()
	schedulerMode = nextSchedulerMode
	schedulerModeMu.Unlock()

	keepaliveAutoMu.Lock()
	keepaliveAuto = nextKeepaliveAuto
	keepaliveAutoMu.Unlock()

	systemRedactMu.Lock()
	systemRedact = nextSystemRedact
	systemRedactMu.Unlock()

	// management key: config_yaml > env > keep existing. Empty stays empty
	// (plugin-layer auth disabled, host middleware still guards).
	if nextMgmtKey == "" {
		nextMgmtKey = strings.TrimSpace(os.Getenv("WB_MANAGEMENT_KEY"))
	}
	managementAPIKeyMu.Lock()
	managementAPIKey = nextMgmtKey
	managementAPIKeyMu.Unlock()

	// Models: resolve in priority order (see loadModelsConfig).
	loadModelsConfig(configYAMLBytes)

	resolveUsageReport(cfgURL, cfgKey)
	ensureScheduler()
}

// loadModelsConfig resolves the model list in priority order:
//  1. config_yaml `models:` block — authoritative config, wins on restart
//  2. workbuddy.yaml in the CPA plugins directory — auto-loaded at startup so
//     a CPA restart picks up the file without requiring a manual reload
//  3. built-in default list — wbModels() falls back when nothing is set
//
// Errors from the file are non-fatal: the list stays cleared and wbModels()
// serves built-in defaults. A config_yaml block that yields nothing usable
// also falls through to defaults rather than erroring registration.
func loadModelsConfig(configYAMLBytes []byte) {
	if configured := parseModelsConfigYAML(configYAMLBytes); len(configured) > 0 {
		setConfiguredModels(configured, "config_yaml")
		return
	}
	if len(configYAMLBytes) > 0 && strings.Contains(string(configYAMLBytes), "models:") {
		// User declared a models: block but it produced nothing usable — clear
		// any previously loaded override so defaults are used (don't silently
		// keep a stale list that contradicts the current config).
		setConfiguredModels(nil, "")
		return
	}
	// No config_yaml models: block — auto-load workbuddy.yaml from the CPA
	// plugins directory (or env WB_MODELS_FILE). Ignore errors: a missing or
	// unparsable file simply leaves the list cleared → built-in defaults.
	if _, _, err := loadModelsFromFile(); err != nil {
		setConfiguredModels(nil, "")
	}
}

// systemRedactEnabled reports whether system-prompt obfuscation is enabled.
//
// The value comes ONLY from config_yaml (`system_redact: true/false` in the CPA
// plugin-config editor) — that is the single source of truth and the only place
// it can be persisted. There is deliberately no runtime setter: a panel-side
// toggle could not write back to config_yaml, so it would either be silently
// discarded or clobbered by the next reconfigure, which is exactly the
// misleading behavior we are removing. The panel shows this value read-only.
func systemRedactEnabled() bool {
	systemRedactMu.RLock()
	defer systemRedactMu.RUnlock()
	return systemRedact
}

// resolveUsageReport fills usageReportURL/key from config → env → secret files.
// Mirrors community plugins that inject management keys via env/build (e.g.
// codex-auth-importer CODEX_AUTH_IMPORTER_MANAGEMENT_KEY), not plaintext CPA
// remote-management.secret-key (that field is bcrypt-hashed).
func resolveUsageReport(cfgURL, cfgKey string) {
	url := firstNonEmpty(
		strings.TrimSpace(cfgURL),
		strings.TrimSpace(os.Getenv("USAGE_REPORT_URL")),
		strings.TrimSpace(os.Getenv("CPAMP_USAGE_IMPORT_URL")),
	)
	if url == "" {
		url = probeUsageReportURL()
	}
	key := firstNonEmpty(
		strings.TrimSpace(cfgKey),
		strings.TrimSpace(os.Getenv("USAGE_REPORT_KEY")),
		strings.TrimSpace(os.Getenv("CPAMP_ADMIN_KEY")),
		strings.TrimSpace(os.Getenv("CPA_MANAGER_ADMIN_KEY")),
		readSecretFile(os.Getenv("USAGE_REPORT_KEY_FILE")),
		readSecretFile(os.Getenv("CPAMP_ADMIN_KEY_FILE")),
		readSecretFile(os.Getenv("CPA_MANAGER_ADMIN_KEY_FILE")),
		// docker compose secrets default path
		readSecretFile("/run/secrets/cpamp_admin_key"),
		readSecretFile("/run/secrets/cpamp-admin-key"),
		// optional bind-mounts used on this host
		readSecretFile("/CLIProxyAPI/secrets/cpamp-admin-key"),
		readSecretFile("/CLIProxyAPI/secrets/cpamp_admin_key"),
	)
	usageReportMu.Lock()
	usageReportURL = url
	usageReportKey = key
	usageReportMu.Unlock()
}

// probeUsageReportURL tries localhost first (bare-metal + Docker host-network),
// then Docker compose service name. Returns whichever responds; defaults to
// localhost if both fail (better to try localhost than an unreachable hostname).
func probeUsageReportURL() string {
	for _, candidate := range []string{defaultUsageReportURL, fallbackUsageReportURL} {
		if probeURL(candidate, 2*time.Second) {
			return candidate
		}
	}
	return defaultUsageReportURL
}

// probeURL does a quick HEAD/GET to check if the endpoint is reachable.
func probeURL(target string, timeout time.Duration) bool {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(target)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Any HTTP response (even 401) means the endpoint is reachable;
	// connection refused / DNS failure means not reachable.
	return resp.StatusCode > 0
}

func readSecretFile(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
