// models_config.go makes the model list dynamic: models can be supplied via
// the host's config_yaml (parsed at register/reconfigure time) or an external
// models file re-read at runtime via the management /models/reload endpoint.
// When no configured models exist the built-in default list is used as a
// fallback (so the plugin still advertises models out of the box).
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// configuredModels holds the user-supplied model list, parsed from
// config_yaml at configure() time and optionally overridden by the
// /models/reload endpoint from an external models file. Empty slice means
// "not configured" — callers fall back to the built-in default list.
var (
	configuredModels   []pluginapi.ModelInfo
	configuredModelsMu sync.RWMutex

	// modelsFileSource records where the configured list came from for the
	// dashboard: "config_yaml", "<file path>", or "" (fallback defaults).
	modelsFileSource   = ""
	modelsFileSourceMu sync.RWMutex
)

// modelConfigItem is one entry in the config_yaml `models:` block. Field names
// match the ConfigField description (id, name, alias, context, max_tokens,
// enabled, reasoning). Unrecognized keys are ignored.
type modelConfigItem struct {
	ID          string `json:"id" yaml:"id"`
	Name        string `json:"name" yaml:"name"`
	Alias       string `json:"alias" yaml:"alias"`
	Context     int64  `json:"context" yaml:"context"`
	MaxTokens   int64  `json:"max_tokens" yaml:"max_tokens"`
	Enabled     *bool  `json:"enabled" yaml:"enabled"`
	Reasoning   bool   `json:"reasoning" yaml:"reasoning"`
	Description string `json:"description" yaml:"description"`
}

// setConfiguredModels stores a parsed list and records its source label.
// An empty/nil list clears the override so wbModels() falls back to the
// built-in defaults. The caller-supplied slice is copied to avoid aliasing.
func setConfiguredModels(models []pluginapi.ModelInfo, source string) {
	cp := make([]pluginapi.ModelInfo, len(models))
	copy(cp, models)
	configuredModelsMu.Lock()
	if len(cp) == 0 {
		configuredModels = nil
	} else {
		configuredModels = cp
	}
	configuredModelsMu.Unlock()
	modelsFileSourceMu.Lock()
	if len(cp) == 0 {
		modelsFileSource = ""
	} else {
		modelsFileSource = source
	}
	modelsFileSourceMu.Unlock()
}

// getConfiguredModels returns the current configured list (may be nil/empty).
// It is a snapshot copy so callers may mutate freely.
func getConfiguredModels() []pluginapi.ModelInfo {
	configuredModelsMu.RLock()
	defer configuredModelsMu.RUnlock()
	if len(configuredModels) == 0 {
		return nil
	}
	cp := make([]pluginapi.ModelInfo, len(configuredModels))
	copy(cp, configuredModels)
	return cp
}

func getModelsSource() string {
	modelsFileSourceMu.RLock()
	defer modelsFileSourceMu.RUnlock()
	return modelsFileSource
}

// modelConfigToModelInfo maps one config item to a ModelInfo. Alias is NOT
// applied here — aliasing is the host's job via oauth-model-alias; we only
// surface the id/name/context/max_tokens. An item with no id is skipped by the
// caller. When Name is empty it defaults to ID. Enabled=nil means enabled.
func modelConfigToModelInfo(m modelConfigItem) (pluginapi.ModelInfo, bool) {
	id := strings.TrimSpace(m.ID)
	if id == "" {
		return pluginapi.ModelInfo{}, false
	}
	if m.Enabled != nil && !*m.Enabled {
		return pluginapi.ModelInfo{}, false
	}
	name := strings.TrimSpace(m.Name)
	if name == "" {
		name = id
	}
	methods := []string{"chat"}
	return pluginapi.ModelInfo{
		ID:                         id,
		Name:                       name,
		ContextLength:              m.Context,
		MaxCompletionTokens:        m.MaxTokens,
		OwnedBy:                    providerName,
		SupportedGenerationMethods: methods,
	}, true
}

// parseModelsConfigYAML parses the `models:` block of config_yaml into a
// ModelInfo slice. It uses a small line-based scanner that handles the common
// YAML block-sequence form:
//
//	models:
//	  - id: glm-5.2
//	    name: GLM-5.2
//	    context: 1000000
//	    max_tokens: 8192
//	  - id: kimi-k2.7
//	    name: Kimi K2.7
//
// It also accepts an inline JSON array value for the `models:` key, e.g.
// `models: [{"id":"glm-5.2","name":"GLM-5.2","context":1000000}]`, which is
// convenient when the host stores arrays as a JSON string. Unparseable input
// yields nil (caller falls back to defaults) rather than an error, so a bad
// models block never breaks registration.
func parseModelsConfigYAML(rawYAML []byte) []pluginapi.ModelInfo {
	text := string(rawYAML)
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")

	// Locate the top-level `models:` key (column-0, not indented).
	idx := -1
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if ln[0] == ' ' || ln[0] == '\t' {
			continue // indented — belongs to some other block
		}
		if strings.HasPrefix(trimmed, "models:") {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil
	}

	keyLine := strings.TrimSpace(lines[idx])
	rest := strings.TrimSpace(strings.TrimPrefix(keyLine, "models:"))
	rest = strings.Trim(rest, "\"'")
	// Inline JSON array value on the same line.
	if strings.HasPrefix(rest, "[") {
		var items []modelConfigItem
		if err := json.Unmarshal([]byte(rest), &items); err == nil {
			return modelConfigItemsToInfos(items)
		}
		return nil
	}
	// Inline bare scalar or flow — not a list we can use.
	if rest != "" {
		return nil
	}

	// Block sequence: collect subsequent indented lines until a dedent to
	// column 0 (another top-level key) or EOF.
	var block []string
	for j := idx + 1; j < len(lines); j++ {
		ln := lines[j]
		if strings.TrimSpace(ln) == "" {
			block = append(block, ln)
			continue
		}
		if ln[0] == ' ' || ln[0] == '\t' || ln[0] == '-' {
			block = append(block, ln)
			continue
		}
		break
	}
	if len(block) == 0 {
		return nil
	}
	return parseModelsBlock(block)
}

// parseModelsBlock parses the collected indented YAML block of the `models:`
// section into config items. It groups lines into items by the `- ` list
// marker and reads `key: value` pairs within each item.
func parseModelsBlock(block []string) []pluginapi.ModelInfo {
	var items []modelConfigItem
	var cur modelConfigItem
	hasCur := false
	flush := func() {
		if hasCur {
			items = append(items, cur)
			cur = modelConfigItem{}
			hasCur = false
		}
	}

	for _, raw := range block {
		ln := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// A new list item starts with "- " (possibly after indentation).
		if strings.HasPrefix(trimmed, "- ") || trimmed == "-" {
			flush()
			hasCur = true
			// The remainder after "- " may carry the first key: value.
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
			rest = strings.TrimSpace(rest)
			if rest != "" {
				applyModelKV(&cur, rest)
			}
			continue
		}
		// Continuation key: value for the current item.
		if hasCur {
			applyModelKV(&cur, trimmed)
		}
	}
	flush()
	return modelConfigItemsToInfos(items)
}

// applyModelKV sets one "key: value" pair onto the config item. Values may be
// quoted; quotes are stripped. Booleans accept true/false/1/0/yes/no/on/off.
func applyModelKV(m *modelConfigItem, kv string) {
	parts := strings.SplitN(kv, ":", 2)
	if len(parts) != 2 {
		return
	}
	key := strings.TrimSpace(parts[0])
	val := strings.TrimSpace(parts[1])
	val = strings.Trim(val, "\"'")
	switch key {
	case "id":
		m.ID = val
	case "name":
		m.Name = val
	case "alias":
		m.Alias = val
	case "context":
		m.Context = parseInt64(val)
	case "max_tokens", "maxtokens", "max-tokens":
		m.MaxTokens = parseInt64(val)
	case "enabled":
		b := parseBool(val)
		m.Enabled = &b
	case "reasoning":
		m.Reasoning = parseBool(val)
	case "description":
		m.Description = val
	}
}

func modelConfigItemsToInfos(items []modelConfigItem) []pluginapi.ModelInfo {
	var out []pluginapi.ModelInfo
	for _, it := range items {
		if mi, ok := modelConfigToModelInfo(it); ok {
			out = append(out, mi)
		}
	}
	return out
}

func parseInt64(s string) int64 {
	s = strings.TrimSpace(strings.Trim(s, "\"'"))
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func parseBool(s string) bool {
	s = strings.TrimSpace(strings.Trim(strings.ToLower(s), "\"'"))
	switch s {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// modelsFilePathCandidates resolves the external models file location by
// probing several plausible paths. os.Executable() returns the host process
// (CPA main binary) path for a c-shared plugin, NOT the .so path, so the
// "plugins" directory is derived as <exe-dir>/plugins (the conventional CPA
// layout: <CPA-root>/cliproxyapi + <CPA-root>/plugins/workbuddy.so). To stay
// robust across different binary placements and Docker layouts, several
// candidates are tried in order; the first that exists wins.
//
// Priority:
//  1. env WB_MODELS_FILE (absolute path, any extension/format)
//  2. <exe-dir>/plugins/workbuddy.yaml   — standard CPA layout
//  3. <exe-dir>/workbuddy.yaml           — exe-dir is already the plugins dir
//  4. <exe-dir>/../plugins/workbuddy.yaml — binary in a bin/ subdir
func modelsFilePathCandidates() []string {
	var out []string
	seen := make(map[string]bool)
	add := func(p string) {
		p = filepath.Clean(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	if p := strings.TrimSpace(os.Getenv("WB_MODELS_FILE")); p != "" {
		add(p)
	}
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		add(filepath.Join(base, "plugins", "workbuddy.yaml"))
		add(filepath.Join(base, "workbuddy.yaml"))
		add(filepath.Join(base, "..", "plugins", "workbuddy.yaml"))
	}
	return out
}

// parseModelsFromAnyFormat parses a models file that may be either JSON (a
// top-level array of objects) or YAML (a `models:` block, or a bare YAML
// block sequence of objects). JSON is detected by a leading `[`/`{`; YAML
// is delegated to parseModelsConfigYAML (which finds `models:`) with a
// fallback to parseModelsBlock for bare-array files. Returns nil when the
// content contains no usable models (caller falls back to defaults).
func parseModelsFromAnyFormat(raw []byte) []pluginapi.ModelInfo {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return nil
	}
	if text[0] == '[' || text[0] == '{' {
		var items []modelConfigItem
		if err := json.Unmarshal(raw, &items); err == nil {
			return modelConfigItemsToInfos(items)
		}
		return nil
	}
	// YAML with a top-level models: key.
	if infos := parseModelsConfigYAML(raw); len(infos) > 0 {
		return infos
	}
	// Bare YAML block sequence (no models: wrapper).
	return parseModelsBlock(strings.Split(string(raw), "\n"))
}

// loadModelsFromFile reads the external models file (workbuddy.yaml under the
// CPA plugins directory, or a path from env WB_MODELS_FILE) in YAML or JSON
// form and, on success, installs it as the configured list. It probes every
// candidate from modelsFilePathCandidates and uses the first that exists and
// parses. Returns the parsed count and the actual source path. On any error
// the existing configured list is left untouched and the error is returned
// (listing every path that was tried when none was found).
func loadModelsFromFile() (int, string, error) {
	candidates := modelsFilePathCandidates()
	if len(candidates) == 0 {
		return 0, "", errNoModelsPath
	}
	var tried []string
	for _, path := range candidates {
		tried = append(tried, path)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		infos := parseModelsFromAnyFormat(raw)
		if len(infos) == 0 {
			return 0, "", &modelsErr{"models file " + path + " had no usable models"}
		}
		setConfiguredModels(infos, path)
		return len(infos), path, nil
	}
	return 0, "", &modelsErr{"no models file found; tried: " + strings.Join(tried, ", ")}
}

// errNoModelsPath is a sentinel for the reload handler when no candidate path
// could be derived at all.
var errNoModelsPath = &modelsErr{"WB_MODELS_FILE unset and plugin dir unknown"}

type modelsErr struct{ msg string }

func (e *modelsErr) Error() string { return e.msg }

// modelsSourceLabel returns a short label for the dashboard: "config_yaml",
// "<basename>", or "default" (built-in hardcoded list in use).
func modelsSourceLabel() string {
	src := getModelsSource()
	if src == "" {
		return "default"
	}
	if strings.HasPrefix(src, "config_yaml") {
		return "config_yaml"
	}
	return filepath.Base(src)
}

// modelsEditable reports whether the current model list can be edited and
// saved to a file via the panel. It is read-only ONLY when the source is
// config_yaml (config_yaml wins on restart; editing a file would be silently
// overridden). An empty source means the built-in defaults are in use — that
// IS editable, because the panel can create workbuddy.yaml for the first time.
func modelsEditable() bool {
	src := getModelsSource()
	return !strings.HasPrefix(src, "config_yaml")
}

// writableModelsFilePath picks a path to WRITE workbuddy.yaml to. It prefers
// an existing file (from the candidates list), otherwise defaults to
// <exe-dir>/plugins/workbuddy.yaml so a brand-new deployment still has a
// writable target. Returns "" when the path cannot be determined.
func writableModelsFilePath() string {
	// An explicit env override always wins — even when the file doesn't exist
	// yet, because the operator named it on purpose and we should create it.
	if p := strings.TrimSpace(os.Getenv("WB_MODELS_FILE")); p != "" {
		return filepath.Clean(p)
	}
	// Otherwise prefer an existing file from the candidate list, so the save
	// updates whichever workbuddy.yaml the loader has been reading.
	for _, p := range modelsFilePathCandidates() {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// No existing file; derive the default plugins-dir path for creation.
	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		// Prefer the standard <exe-dir>/plugins/ layout, ensuring the dir.
		pluginsDir := filepath.Join(base, "plugins")
		if _, err := os.Stat(pluginsDir); err == nil {
			return filepath.Join(pluginsDir, "workbuddy.yaml")
		}
		// Fall back to exe-dir itself (works when exe IS the plugins dir).
		return filepath.Join(base, "workbuddy.yaml")
	}
	return ""
}

// saveModelsToFile serializes the given model list to workbuddy.yaml in YAML
// form, writes it to the writable plugins-directory path, then installs it as
// the configured list and drops the dynamic cache. Used by the panel editor
// save action. Returns the path written and model count.
func saveModelsToFile(models []pluginapi.ModelInfo) (string, int, error) {
	if len(models) == 0 {
		return "", 0, &modelsErr{"refusing to save an empty model list"}
	}
	path := writableModelsFilePath()
	if path == "" {
		return "", 0, errNoModelsPath
	}
	yaml := modelsToYAML(models)
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		return "", 0, err
	}
	setConfiguredModels(models, path)
	clearDynamicModelsCache()
	return path, len(models), nil
}

// modelsToYAML renders a model list as a `models:` YAML block sequence, the
// same format workbuddy.yaml uses and parseModelsConfigYAML reads. Output is
// deterministic so round-trips (load → edit → save) produce minimal diffs.
func modelsToYAML(models []pluginapi.ModelInfo) string {
	var b strings.Builder
	b.WriteString("# workbuddy 模型配置（由面板编辑器生成）\n")
	b.WriteString("models:\n")
	for _, m := range models {
		b.WriteString("  - id: ")
		b.WriteString(yamlScalar(m.ID))
		b.WriteByte('\n')
		if m.Name != "" && m.Name != m.ID {
			b.WriteString("    name: ")
			b.WriteString(yamlScalar(m.Name))
			b.WriteByte('\n')
		}
		if m.ContextLength > 0 {
			b.WriteString("    context: ")
			b.WriteString(strconv.FormatInt(m.ContextLength, 10))
			b.WriteByte('\n')
		}
		if m.MaxCompletionTokens > 0 {
			b.WriteString("    max_tokens: ")
			b.WriteString(strconv.FormatInt(m.MaxCompletionTokens, 10))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// yamlScalar quotes a YAML scalar only when needed (contains special chars,
// or looks like a boolean/number). Leading/trailing whitespace is trimmed
// first — YAML would drop it anyway, and the editor inputs are trimmed.
// Otherwise emits it bare for cleaner diffs and a hand-written look.
func yamlScalar(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return `""`
	}
	needsQuote := false
	// Quote if it contains any YAML-indicator chars or would be misread.
	for _, c := range s {
		if c == ':' || c == '#' || c == '\'' || c == '"' || c == '\n' || c == '\t' {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		lower := strings.ToLower(s)
		switch lower {
		case "true", "false", "null", "yes", "no", "on", "off", "~":
			needsQuote = true
		}
		// Looks like a number? quote to preserve as string.
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			needsQuote = true
		}
		if _, err := strconv.ParseFloat(s, 64); err == nil {
			needsQuote = true
		}
	}
	if !needsQuote {
		return s
	}
	// Double-quoted form with backslash-escapes for quote and backslash.
	q := strings.ReplaceAll(s, "\\", "\\\\")
	q = strings.ReplaceAll(q, "\"", "\\\"")
	return "\"" + q + "\""
}
