// credits_handler.go implements the management API endpoints that mutate or
// read account state: import credential, toggle check-in, claim trial, select
// active auth, and query credits for one account or all.
package main

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// handleImportAuth accepts nested or flat credential JSON and persists via host.auth.save.
func handleImportAuth(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		JSON json.RawMessage `json:"json"`
		Raw  string          `json:"raw"`
	}
	_ = json.Unmarshal(req.Body, &body)
	raw := []byte(strings.TrimSpace(body.Raw))
	if len(body.JSON) > 0 {
		raw = body.JSON
	}
	if len(raw) == 0 {
		return map[string]any{"success": false, "error": "missing json/raw credential payload"}
	}
	sa, err := parseStored(raw)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	// Persist nested storage + top-level type/note/logo/disabled for Auth page.
	fileJSON, err := buildAuthFileJSON(sa, false, displayNote(sa, nil, false), nil)
	if err != nil {
		return map[string]any{"success": false, "error": err.Error()}
	}
	auth := toAuthData(sa)
	saveReq := pluginapi.HostAuthSaveRequest{
		Name: auth.FileName,
		JSON: fileJSON,
	}
	saveBody, _ := json.Marshal(saveReq)
	rawResp, err := hostCall(pluginabi.MethodHostAuthSave, saveBody)
	if err != nil {
		return map[string]any{"success": false, "error": "host.auth.save: " + err.Error()}
	}
	var env envelope
	if err := json.Unmarshal(rawResp, &env); err != nil || !env.OK {
		msg := "host.auth.save failed"
		if env.Error != nil && env.Error.Message != "" {
			msg = env.Error.Message
		}
		return map[string]any{"success": false, "error": msg}
	}
	var saveResp pluginapi.HostAuthSaveResponse
	_ = json.Unmarshal(env.Result, &saveResp)
	// Remove legacy workbuddy.json if it exists and differs from the saved name.
	if saveResp.Name != "" && !strings.EqualFold(saveResp.Name, authFileName) {
		legacyPath := strings.TrimSpace(saveResp.Path)
		// Best-effort: if auth dir is known via saveResp.Path parent, try removing sibling workbuddy.json.
		if legacyPath != "" {
			dir := filepath.Dir(legacyPath)
			legacyFile := filepath.Join(dir, authFileName)
			// A-35: use deleteAuthFileInDir for absolute path + directory confinement.
			_ = deleteAuthFileInDir(legacyFile, dir)
		}
	}
	return map[string]any{
		"success":  true,
		"name":     saveResp.Name,
		"path":     saveResp.Path,
		"uid":      sa.Account.UID,
		"nickname": sa.Account.Nickname,
		"file":     auth.FileName,
	}
}

func handleCheckinConfig(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	_ = json.Unmarshal(req.Body, &body)
	checkinAutoMu.Lock()
	if body.Enabled != nil {
		// Runtime-only toggle: the CPA host exposes no plugin-config write
		// callback, so persisting would mean editing the host's config.yaml
		// from inside the plugin (fragile under docker volume mounts). The
		// value from config_yaml wins again on CPA restart.
		checkinAuto = *body.Enabled
	}
	cur := checkinAuto
	checkinAutoMu.Unlock()
	return map[string]any{"checkin_auto": cur, "persistent": false}
}

// handleModelsReload re-reads the external models file (WB_MODELS_FILE, or
// <plugin dir>/workbuddy.yaml) in YAML or JSON form and installs it as the
// configured model list at runtime, then drops the dynamic upstream-models
// cache so the next model query sees the fresh list. The models file is a
// YAML `models:` block / bare block sequence, or a JSON array, of objects
// with {id,name?,context?,max_tokens?,enabled?,reasoning?}. On any read
// error the existing list is left unchanged. Persistence: none — the loaded
// list lives only in memory and is cleared (back to config_yaml/default) on
// CPA restart, consistent with the other runtime toggles.
func handleModelsReload(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		// Clear bool `json:"clear"` — when true, drop the configured list and
		// fall back to defaults without reading a file.
		Clear *bool `json:"clear"`
	}
	_ = json.Unmarshal(req.Body, &body)
	if body.Clear != nil && *body.Clear {
		setConfiguredModels(nil, "")
		clearDynamicModelsCache()
		return map[string]any{
			"ok":            true,
			"action":        "cleared",
			"models_source": "default",
			"persistent":    false,
		}
	}
	n, path, err := loadModelsFromFile()
	if err != nil {
		return map[string]any{
			"ok":            false,
			"error":         err.Error(),
			"models_source": modelsSourceLabel(),
			"persistent":    false,
		}
	}
	// Drop the dynamic upstream cache so the next model.for_auth / static call
	// re-derives from the freshly configured list (or falls back to it).
	clearDynamicModelsCache()
	return map[string]any{
		"ok":            true,
		"action":        "loaded",
		"models_loaded": n,
		"models_source": filepath.Base(path),
		"models_path":   path,
		"persistent":    false,
	}
}

// handleModelsGet returns the current model list for the panel editor. The
// list is whatever wbModels() would serve right now (configured > default).
// editable=false means the source is config_yaml — the panel shows a read-only
// hint instead of the table, because config_yaml wins on restart.
func handleModelsGet(req pluginapi.ManagementRequest) map[string]any {
	models := wbModels()
	editable := modelsEditable()
	source := modelsSourceLabel()
	return map[string]any{
		"models":   models,
		"source":   source,
		"editable": editable,
		"count":    len(models),
	}
}

// handleModelsSave accepts an edited model list, writes it to workbuddy.yaml in
// the CPA plugins directory, then installs + reloads it. Refuses an empty
// list. When the source is config_yaml the endpoint returns an error so the
// panel can tell the user to edit config_yaml instead.
func handleModelsSave(req pluginapi.ManagementRequest) map[string]any {
	if !modelsEditable() {
		return map[string]any{
			"ok":     false,
			"error":  "模型列表来自 config_yaml（优先级最高），请在 CPA 配置里修改 models: 块，不要用面板编辑",
			"source": modelsSourceLabel(),
		}
	}
	var body struct {
		Models []struct {
			ID                  string `json:"id"`
			Name                string `json:"name"`
			ContextLength       int64  `json:"context"`
			MaxCompletionTokens int64  `json:"max_tokens"`
		} `json:"models"`
	}
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return map[string]any{"ok": false, "error": "解析请求失败: " + err.Error()}
	}
	if len(body.Models) == 0 {
		return map[string]any{"ok": false, "error": "模型列表不能为空"}
	}
	// Validate ids (required, non-empty, no duplicates).
	seen := make(map[string]bool)
	for i, m := range body.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			return map[string]any{"ok": false, "error": "第 " + intToStr(i+1) + " 个模型缺少 id"}
		}
		if seen[strings.ToLower(id)] {
			return map[string]any{"ok": false, "error": "模型 id 重复: " + id}
		}
		seen[strings.ToLower(id)] = true
	}
	infos := make([]pluginapi.ModelInfo, 0, len(body.Models))
	for _, m := range body.Models {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			name = strings.TrimSpace(m.ID)
		}
		infos = append(infos, pluginapi.ModelInfo{
			ID:                         strings.TrimSpace(m.ID),
			Name:                       name,
			ContextLength:              m.ContextLength,
			MaxCompletionTokens:        m.MaxCompletionTokens,
			OwnedBy:                    providerName,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	path, n, err := saveModelsToFile(infos)
	if err != nil {
		return map[string]any{"ok": false, "error": "保存失败: " + err.Error()}
	}
	return map[string]any{
		"ok":            true,
		"models_saved":  n,
		"models_source": filepath.Base(path),
		"models_path":   path,
		"persistent":    true,
	}
}

// intToStr is a small helper to avoid importing strconv just for one use.
func intToStr(n int) string { return strconv.Itoa(n) }

// handleClaimTrial claims the expert trial pack for one Global account.
// CN accounts are rejected — the trial endpoint is Global-only.
func handleClaimTrial(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required"}
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, f := range files {
		if f.AuthIndex != authIndex {
			continue
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return map[string]any{"auth_index": authIndex, "error": err.Error()}
		}
		if !isGlobalDomain(sa.Auth.Domain) {
			return map[string]any{"auth_index": authIndex, "error": "专家加油包仅适用于国际版账号"}
		}
		res, err := performTrialCall(sa)
		out := map[string]any{"auth_index": authIndex, "nickname": sa.Account.Nickname}
		if err != nil {
			out["error"] = err.Error()
		} else {
			for k, v := range res {
				out[k] = v
			}
		}
		// Invalidate credits cache (copy entry, set credits=nil, keep plan/checkin).
		if v, ok := accountCache.Load(f.ID); ok {
			if e, ok2 := v.(*accountCacheEntry); ok2 {
				fresh := *e
				fresh.credits = nil
				fresh.fetched = time.Now()
				accountCache.Store(f.ID, &fresh)
			}
		}
		if lifecycleEnabled() {
			_, _ = reconcileOneAccount(authIndex, f.ID, true)
		}
		return out
	}
	return map[string]any{"error": "account not found"}
}

// handleSelectAuth sets the panel-selected account used for chat routing.
// Region (CN/Global) is read from that account's stored domain on each request.
func handleSelectAuth(req pluginapi.ManagementRequest) map[string]any {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	_ = json.Unmarshal(req.Body, &body)
	authIndex := strings.TrimSpace(body.AuthIndex)
	if authIndex == "" {
		return map[string]any{"error": "auth_index is required", "active_auth": getActiveAuthID()}
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	for _, f := range files {
		if f.AuthIndex != authIndex {
			continue
		}
		if f.Disabled {
			return map[string]any{"error": "账号已禁用，无法选中", "auth_index": authIndex}
		}
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			return map[string]any{"error": err.Error(), "auth_index": authIndex}
		}
		setActiveAuthID(f.ID)
		return map[string]any{
			"ok":          true,
			"active_auth": f.ID,
			"region":      accountRegion(sa),
			"nickname":    sa.Account.Nickname,
			"uid":         sa.Account.UID,
		}
	}
	return map[string]any{"error": "account not found", "auth_index": authIndex}
}

// handleCreditsQuery returns real-time credits for one or all accounts.
// Pass ?auth_index=<idx> to query a single account; omit for all.
// Single-account mode returns full account info (nickname, region, credits,
// exhausted, trial_claimed) so the panel can update one card without
// reloading the entire dashboard.
func handleCreditsQuery(req pluginapi.ManagementRequest) map[string]any {
	authIndex := ""
	if vals := req.Query["auth_index"]; len(vals) > 0 {
		authIndex = strings.TrimSpace(vals[0])
	}
	files, err := hostAuthList()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	// Single-account: return one full account row (like dashboard entry).
	if authIndex != "" {
		for _, f := range files {
			if f.AuthIndex != authIndex {
				continue
			}
			sa, err := hostAuthGet(f.AuthIndex)
			if err != nil {
				return map[string]any{"accounts": []map[string]any{{
					"auth_index": authIndex, "error": "load auth: " + err.Error(),
				}}}
			}
			cr, err := fetchUserResource(sa)
			acct := map[string]any{
				"auth_index": authIndex,
				"nickname":   sa.Account.Nickname,
				"uid":        sa.Account.UID,
				"region":     accountRegion(sa),
				"name":       f.Name,
				"label":      f.Label,
				"disabled":   f.Disabled,
				"selected":   getActiveAuthID() == f.ID,
			}
			if err != nil {
				acct["error"] = err.Error()
			} else {
				acct["credits"] = cr
				acct["exhausted"] = isCreditsExhausted(cr)
				if isGlobalDomain(sa.Auth.Domain) {
					acct["trial_claimed"] = hasTrialPack(cr)
				}
				// Also fetch plan so the badge updates on lazy load.
				acct["plan"] = fetchPaymentType(sa)
				// Update cache so subsequent dashboard loads see fresh data.
				now := time.Now()
				if cr != nil {
					cr.FetchedAt = now.UTC().Format(time.RFC3339)
				}
				// Merge into existing cache entry (keep checkin if present).
				var prev *accountCacheEntry
				if v, ok := accountCache.Load(f.ID); ok {
					prev, _ = v.(*accountCacheEntry)
				}
				var ci *checkinSummary
				if prev != nil {
					ci = prev.checkin
				}
				plan, _ := acct["plan"].(string)
				accountCache.Store(f.ID, &accountCacheEntry{
					checkin: ci, credits: cr, plan: plan, fetched: now,
				})
			}
			return map[string]any{"accounts": []map[string]any{acct}}
		}
		return map[string]any{"error": "account not found"}
	}
	// All accounts: return simplified list.
	type acctCredits struct {
		AuthIndex string          `json:"auth_index"`
		Nickname  string          `json:"nickname"`
		UID       string          `json:"uid"`
		Credits   *creditsSummary `json:"credits,omitempty"`
		Error     string          `json:"error,omitempty"`
	}
	var out []acctCredits
	for _, f := range files {
		sa, err := hostAuthGet(f.AuthIndex)
		if err != nil {
			out = append(out, acctCredits{AuthIndex: f.AuthIndex, Error: "load auth: " + err.Error()})
			continue
		}
		cr, err := fetchUserResource(sa)
		ac := acctCredits{AuthIndex: f.AuthIndex, Nickname: sa.Account.Nickname, UID: sa.Account.UID}
		if err != nil {
			ac.Error = err.Error()
		} else {
			ac.Credits = cr
		}
		out = append(out, ac)
	}
	return map[string]any{"accounts": out}
}
