package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestLoadModelsConfig_StartupAutoloadsFile(t *testing.T) {
	// Simulate CPA startup with no models: block in config_yaml — the plugin
	// must auto-load workbuddy.yaml from the plugins directory.
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy.yaml")
	content := "models:\n  - id: from-file-1\n    name: From File 1\n    context: 5000\n    max_tokens: 500\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("WB_MODELS_FILE", path)
	defer setConfiguredModels(nil, "")

	// config_yaml has other settings but NO models: block.
	loadModelsConfig([]byte("checkin_auto: true\nsystem_redact: false\n"))

	got := wbModels()
	if len(got) != 1 || got[0].ID != "from-file-1" {
		t.Fatalf("startup did not autoload file models; got %+v", got)
	}
	if got[0].ContextLength != 5000 {
		t.Errorf("context = %d, want 5000", got[0].ContextLength)
	}
	if modelsSourceLabel() != "workbuddy.yaml" {
		t.Errorf("source = %q, want workbuddy.yaml", modelsSourceLabel())
	}
}

func TestLoadModelsConfig_ConfigYAMLWinsOverFile(t *testing.T) {
	// config_yaml models: block must beat the file (config is authoritative).
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy.yaml")
	_ = os.WriteFile(path, []byte("models:\n  - id: from-file\n"), 0o644)
	t.Setenv("WB_MODELS_FILE", path)
	defer setConfiguredModels(nil, "")

	loadModelsConfig([]byte("models:\n  - id: from-config\n    name: From Config\n"))

	got := wbModels()
	if len(got) != 1 || got[0].ID != "from-config" {
		t.Fatalf("config_yaml should win; got %+v", got)
	}
	if modelsSourceLabel() != "config_yaml" {
		t.Errorf("source = %q, want config_yaml", modelsSourceLabel())
	}
}

func TestLoadModelsConfig_FallsBackToDefaultWhenNoFile(t *testing.T) {
	// No config_yaml block and no file → built-in defaults.
	dir := t.TempDir()
	t.Setenv("WB_MODELS_FILE", filepath.Join(dir, "nonexistent.yaml"))
	defer setConfiguredModels(nil, "")

	loadModelsConfig([]byte("checkin_auto: true\n"))

	got := wbModels()
	def := defaultModels()
	if len(got) != len(def) {
		t.Fatalf("expected %d default models, got %d", len(def), len(got))
	}
	if got[0].ID != def[0].ID {
		t.Errorf("expected default model %q, got %q", def[0].ID, got[0].ID)
	}
	if modelsSourceLabel() != "default" {
		t.Errorf("source = %q, want default", modelsSourceLabel())
	}
}

func TestLoadModelsConfig_BadFileFallsBackToDefault(t *testing.T) {
	// A present but unparsable file must NOT break startup — fall to defaults.
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy.yaml")
	_ = os.WriteFile(path, []byte("this is not a valid models file\n"), 0o644)
	t.Setenv("WB_MODELS_FILE", path)
	defer setConfiguredModels(nil, "")

	loadModelsConfig([]byte("checkin_auto: true\n"))

	got := wbModels()
	if len(got) != len(defaultModels()) {
		t.Errorf("expected built-in defaults for unparsable file, got %d models", len(got))
	}
}

func TestModelsToYAML_RoundTrip(t *testing.T) {
	// modelsToYAML output must be re-readable by the parser (round-trip).
	models := []pluginapi.ModelInfo{
		{ID: "glm-5.2", Name: "GLM-5.2", ContextLength: 1000000, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "kimi-k2.7", Name: "Kimi K2.7", ContextLength: 262144, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
	yaml := modelsToYAML(models)
	got := parseModelsFromAnyFormat([]byte(yaml))
	if len(got) != 2 {
		t.Fatalf("round-trip produced %d models, want 2\nYAML:\n%s", len(got), yaml)
	}
	if got[0].ID != "glm-5.2" || got[0].Name != "GLM-5.2" || got[0].ContextLength != 1000000 || got[0].MaxCompletionTokens != 8192 {
		t.Errorf("model[0] mismatch: %+v", got[0])
	}
	if got[1].ID != "kimi-k2.7" || got[1].Name != "Kimi K2.7" {
		t.Errorf("model[1] mismatch: %+v", got[1])
	}
	if !strings.Contains(yaml, "models:") {
		t.Errorf("YAML missing models: key\n%s", yaml)
	}
}

func TestModelsToYAML_SkipsRedundantName(t *testing.T) {
	// When Name == ID the serializer should omit the name line.
	yaml := modelsToYAML([]pluginapi.ModelInfo{
		{ID: "hy3", Name: "hy3", ContextLength: 262144, MaxCompletionTokens: 8192, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	})
	if strings.Contains(yaml, "name:") {
		t.Errorf("should omit name when equal to id:\n%s", yaml)
	}
}

func TestYamlScalar(t *testing.T) {
	cases := []struct{ in, want string }{
		{"glm-5.2", "glm-5.2"},         // plain
		{"GLM 五点二", "GLM 五点二"},         // unicode ok
		{"true", `"true"`},             // bool-like
		{"123", `"123"`},               // number-like
		{"has: colon", `"has: colon"`}, // colon
		{"#hash", `"#hash"`},           // comment char
		{"  padded  ", "padded"},       // trimmed, plain (YAML drops edge spaces)
		{"GLM 五点二", "GLM 五点二"},         // unicode + inner spaces kept bare
		{"", `""`},                     // empty
	}
	for _, c := range cases {
		if got := yamlScalar(c.in); got != c.want {
			t.Errorf("yamlScalar(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSaveModelsToFile(t *testing.T) {
	// Point WB_MODELS_FILE at a temp file (creates it on save).
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy.yaml")
	t.Setenv("WB_MODELS_FILE", path)
	defer setConfiguredModels(nil, "")

	models := []pluginapi.ModelInfo{
		{ID: "m1", Name: "Model 1", ContextLength: 1000, MaxCompletionTokens: 100, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
		{ID: "m2", Name: "Model 2", ContextLength: 2000, MaxCompletionTokens: 200, OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}},
	}
	savedPath, n, err := saveModelsToFile(models)
	if err != nil {
		t.Fatalf("saveModelsToFile: %v", err)
	}
	if savedPath != path {
		t.Errorf("path = %q, want %q", savedPath, path)
	}
	if n != 2 {
		t.Errorf("saved %d, want 2", n)
	}
	// File must exist and re-parse to the same models.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := parseModelsFromAnyFormat(raw)
	if len(got) != 2 || got[0].ID != "m1" || got[1].ID != "m2" {
		t.Fatalf("re-parsed = %+v", got)
	}
	if got[0].ContextLength != 1000 || got[0].MaxCompletionTokens != 100 {
		t.Errorf("m1 fields = %+v", got[0])
	}
	// Memory list must now be the saved one.
	if cfg := getConfiguredModels(); len(cfg) != 2 {
		t.Errorf("configured after save = %d, want 2", len(cfg))
	}
}

func TestSaveModelsToFile_RejectsEmpty(t *testing.T) {
	t.Setenv("WB_MODELS_FILE", filepath.Join(t.TempDir(), "workbuddy.yaml"))
	defer setConfiguredModels(nil, "")
	if _, _, err := saveModelsToFile(nil); err == nil {
		t.Fatal("expected error saving an empty list")
	}
}

func TestModelsEditable(t *testing.T) {
	defer setConfiguredModels(nil, "")
	// No source (defaults) → editable (panel can create the file).
	setConfiguredModels(nil, "")
	if !modelsEditable() {
		t.Error("default source should be editable (panel creates workbuddy.yaml)")
	}
	// File source → editable.
	one := []pluginapi.ModelInfo{{ID: "x", Name: "x", OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}}}
	setConfiguredModels(one, "/some/path/workbuddy.yaml")
	if !modelsEditable() {
		t.Error("file source should be editable")
	}
	// config_yaml source → read-only.
	setConfiguredModels(one, "config_yaml")
	if modelsEditable() {
		t.Error("config_yaml source should be read-only")
	}
	if modelsSourceLabel() != "config_yaml" {
		t.Errorf("source label = %q, want config_yaml", modelsSourceLabel())
	}
}

func TestParseModelsConfigYAML_BlockSequence(t *testing.T) {
	yaml := []byte(`checkin_auto: true
models:
  - id: glm-5.2
    name: GLM-5.2
    context: 1000000
    max_tokens: 8192
  - id: kimi-k2.7
    name: Kimi K2.7
    context: 262144
system_redact: false
`)
	got := parseModelsConfigYAML(yaml)
	if len(got) != 2 {
		t.Fatalf("expected 2 models, got %d: %+v", len(got), got)
	}
	if got[0].ID != "glm-5.2" || got[0].Name != "GLM-5.2" || got[0].ContextLength != 1000000 || got[0].MaxCompletionTokens != 8192 {
		t.Errorf("model[0] mismatch: %+v", got[0])
	}
	if got[0].OwnedBy != providerName {
		t.Errorf("OwnedBy = %q, want %q", got[0].OwnedBy, providerName)
	}
	if got[1].ID != "kimi-k2.7" || got[1].Name != "Kimi K2.7" {
		t.Errorf("model[1] mismatch: %+v", got[1])
	}
}

func TestParseModelsConfigYAML_DisabledItemSkipped(t *testing.T) {
	yaml := []byte(`models:
  - id: glm-5.2
    enabled: false
  - id: kimi-k2.7
    enabled: true
`)
	got := parseModelsConfigYAML(yaml)
	if len(got) != 1 {
		t.Fatalf("expected 1 enabled model, got %d", len(got))
	}
	if got[0].ID != "kimi-k2.7" {
		t.Errorf("expected kimi-k2.7, got %s", got[0].ID)
	}
}

func TestParseModelsConfigYAML_NameDefaultsToID(t *testing.T) {
	yaml := []byte(`models:
  - id: deepseek-v4-pro
    context: 1000000
`)
	got := parseModelsConfigYAML(yaml)
	if len(got) != 1 {
		t.Fatalf("expected 1 model, got %d", len(got))
	}
	if got[0].Name != "deepseek-v4-pro" {
		t.Errorf("Name should default to ID, got %q", got[0].Name)
	}
}

func TestParseModelsConfigYAML_InlineJSON(t *testing.T) {
	yaml := []byte(`checkin_auto: true
models: [{"id":"glm-5.2","name":"GLM-5.2","context":1000000,"max_tokens":8192}]
`)
	got := parseModelsConfigYAML(yaml)
	if len(got) != 1 {
		t.Fatalf("expected 1 model from inline JSON, got %d", len(got))
	}
	if got[0].ID != "glm-5.2" || got[0].ContextLength != 1000000 {
		t.Errorf("inline JSON model mismatch: %+v", got[0])
	}
}

func TestParseModelsConfigYAML_NoBlock(t *testing.T) {
	got := parseModelsConfigYAML([]byte("checkin_auto: true\nsystem_redact: false\n"))
	if got != nil {
		t.Errorf("expected nil when no models: block, got %+v", got)
	}
}

func TestParseModelsConfigYAML_Empty(t *testing.T) {
	if parseModelsConfigYAML(nil) != nil {
		t.Error("nil input should return nil")
	}
	if parseModelsConfigYAML([]byte("")) != nil {
		t.Error("empty input should return nil")
	}
}

func TestParseModelsConfigYAML_MalformedNoPanic(t *testing.T) {
	// Garbage input must not panic; it returns nil (fallback to defaults).
	got := parseModelsConfigYAML([]byte("models:\n  - !!broken yaml: ["))
	if got != nil {
		// It's OK if it parses 0 items; either nil or empty-but-nil is fine.
		// We only require no panic (the test reaching here means no panic).
	}
}

func TestWbModels_FallsBackToDefault(t *testing.T) {
	// Ensure no configured models are set, then verify fallback.
	setConfiguredModels(nil, "")
	got := wbModels()
	def := defaultModels()
	if len(got) != len(def) {
		t.Fatalf("wbModels() len = %d, want default %d", len(got), len(def))
	}
	for i := range got {
		if got[i].ID != def[i].ID {
			t.Errorf("wbModels()[%d].ID = %q, want %q", i, got[i].ID, def[i].ID)
		}
	}
}

func TestWbModels_UsesConfigured(t *testing.T) {
	custom := []pluginapi.ModelInfo{{ID: "custom-1", Name: "Custom 1", OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}}}
	setConfiguredModels(custom, "config_yaml")
	defer setConfiguredModels(nil, "") // reset for other tests

	got := wbModels()
	if len(got) != 1 || got[0].ID != "custom-1" {
		t.Fatalf("wbModels() = %+v, want custom-1", got)
	}
	// Mutating the returned slice must not corrupt the stored configured list.
	got[0].ID = "mutated"
	got2 := wbModels()
	if got2[0].ID != "custom-1" {
		t.Errorf("configured list was corrupted by caller mutation: got %q", got2[0].ID)
	}
}

func TestLoadModelsFromFile(t *testing.T) {
	// Write a temp models JSON file and point WB_MODELS_FILE at it.
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy_models.json")
	content := `[{"id":"file-model-1","name":"File Model 1","context":200000,"max_tokens":4096},{"id":"file-model-2","name":"File Model 2"}]`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	t.Setenv("WB_MODELS_FILE", path)
	defer setConfiguredModels(nil, "") // reset

	n, src, err := loadModelsFromFile()
	if err != nil {
		t.Fatalf("loadModelsFromFile error: %v", err)
	}
	if n != 2 {
		t.Errorf("loaded %d models, want 2", n)
	}
	if src != path {
		t.Errorf("source = %q, want %q", src, path)
	}
	got := getConfiguredModels()
	if len(got) != 2 || got[0].ID != "file-model-1" || got[1].ID != "file-model-2" {
		t.Errorf("configured models = %+v", got)
	}
	if got[0].ContextLength != 200000 || got[0].MaxCompletionTokens != 4096 {
		t.Errorf("model[0] fields = %+v", got[0])
	}
}

func TestLoadModelsFromFile_YAML(t *testing.T) {
	// workbuddy.yaml format: a `models:` block (same shape as config_yaml).
	dir := t.TempDir()
	path := filepath.Join(dir, "workbuddy.yaml")
	content := `# comment line
models:
  - id: glm-5.2
    name: GLM-5.2
    context: 1000000
    max_tokens: 8192
  - id: kimi-k2.7
    name: Kimi K2.7
    context: 262144
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	t.Setenv("WB_MODELS_FILE", path)
	defer setConfiguredModels(nil, "")

	n, src, err := loadModelsFromFile()
	if err != nil {
		t.Fatalf("loadModelsFromFile error: %v", err)
	}
	if n != 2 {
		t.Errorf("loaded %d models, want 2", n)
	}
	if src != path {
		t.Errorf("source = %q, want %q", src, path)
	}
	got := getConfiguredModels()
	if got[0].ID != "glm-5.2" || got[0].ContextLength != 1000000 {
		t.Errorf("model[0] = %+v", got[0])
	}
	if got[1].ID != "kimi-k2.7" {
		t.Errorf("model[1] = %+v", got[1])
	}
}

func TestLoadModelsFromFile_YAMLBareArray(t *testing.T) {
	// Bare YAML block sequence (no models: wrapper).
	dir := t.TempDir()
	path := filepath.Join(dir, "models.yaml")
	content := `- id: glm-5.2
  name: GLM-5.2
  context: 1000000
- id: kimi-k2.7
  name: Kimi K2.7
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("WB_MODELS_FILE", path)
	defer setConfiguredModels(nil, "")

	n, _, err := loadModelsFromFile()
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if n != 2 {
		t.Fatalf("loaded %d, want 2", n)
	}
	got := getConfiguredModels()
	if got[0].ID != "glm-5.2" || got[1].ID != "kimi-k2.7" {
		t.Errorf("IDs = %q / %q", got[0].ID, got[1].ID)
	}
}

func TestParseModelsFromAnyFormat(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"json array", `[{"id":"a"},{"id":"b"}]`, 2},
		{"yaml models block", "models:\n  - id: a\n  - id: b\n", 2},
		{"yaml bare array", "- id: a\n- id: b\n", 2},
		{"empty", "", 0},
		{"garbage", "not a models file at all\nnothing here", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseModelsFromAnyFormat([]byte(c.raw))
			if len(got) != c.want {
				t.Errorf("got %d models, want %d", len(got), c.want)
			}
		})
	}
}

func TestLoadModelsFromFile_MissingFile(t *testing.T) {
	t.Setenv("WB_MODELS_FILE", filepath.Join(t.TempDir(), "nonexistent.json"))
	setConfiguredModels(nil, "")
	n, _, err := loadModelsFromFile()
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if n != 0 {
		t.Errorf("expected 0 models, got %d", n)
	}
	// Configured list should remain unchanged (empty here).
	if got := getConfiguredModels(); got != nil {
		t.Errorf("configured list should be nil after failed load, got %+v", got)
	}
}

func TestLoadModelsFromFile_BadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(path, []byte("{not valid json"), 0o644)
	t.Setenv("WB_MODELS_FILE", path)
	setConfiguredModels(nil, "")

	_, _, err := loadModelsFromFile()
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestLoadModelsFromFile_EmptyList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	_ = os.WriteFile(path, []byte("[]"), 0o644)
	t.Setenv("WB_MODELS_FILE", path)
	setConfiguredModels(nil, "")

	n, _, err := loadModelsFromFile()
	if err == nil {
		t.Fatal("expected error for empty models list")
	}
	if n != 0 {
		t.Errorf("expected 0 models, got %d", n)
	}
}

func TestSetConfiguredModels_SourceLabel(t *testing.T) {
	defer setConfiguredModels(nil, "")
	setConfiguredModels([]pluginapi.ModelInfo{{ID: "x", Name: "x", OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}}}, "config_yaml")
	if got := modelsSourceLabel(); got != "config_yaml" {
		t.Errorf("source = %q, want config_yaml", got)
	}
	setConfiguredModels(nil, "")
	if got := modelsSourceLabel(); got != "default" {
		t.Errorf("source = %q, want default", got)
	}
}

func TestClearDynamicModelsCache(t *testing.T) {
	// Stash something, clear it, verify it's gone.
	storeDynamicModels([]pluginapi.ModelInfo{{ID: "cached", Name: "cached", OwnedBy: providerName, SupportedGenerationMethods: []string{"chat"}}})
	clearDynamicModelsCache()
	got, ok := cachedDynamicModels()
	if ok {
		t.Errorf("expected cache cleared (ok=false), got ok=true, models=%+v", got)
	}
}

func TestParseModelsConfigYAML_QuotedValues(t *testing.T) {
	yaml := []byte("models:\n  - id: \"glm-5.2\"\n    name: 'GLM 五点二'\n    context: \"1000000\"\n")
	got := parseModelsConfigYAML(yaml)
	if len(got) != 1 {
		t.Fatalf("expected 1 model, got %d", len(got))
	}
	if got[0].ID != "glm-5.2" {
		t.Errorf("ID = %q, want glm-5.2", got[0].ID)
	}
	if got[0].Name != "GLM 五点二" {
		t.Errorf("Name = %q, want 'GLM 五点二'", got[0].Name)
	}
	if got[0].ContextLength != 1000000 {
		t.Errorf("ContextLength = %d, want 1000000", got[0].ContextLength)
	}
}

func TestParseModelsConfigYAML_MultipleBlocks(t *testing.T) {
	// models: block with trailing keys — verify the scanner stops at dedent.
	yaml := []byte(`models:
  - id: a
  - id: b
checkin_auto: true
`)
	got := parseModelsConfigYAML(yaml)
	if len(got) != 2 {
		t.Fatalf("expected 2 models, got %d: %+v", len(got), got)
	}
	want := []string{"a", "b"}
	gotIDs := []string{got[0].ID, got[1].ID}
	if !reflect.DeepEqual(gotIDs, want) {
		t.Errorf("IDs = %v, want %v", gotIDs, want)
	}
}
