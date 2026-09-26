package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// The operator's chosen VideoDir is held in memory (settings.VideoDir) but is
// also persisted here so it survives a restart. Losing it on restart is what
// let the wrong event folder go unnoticed. The file lives beside the per-event
// state under dataRoot() (%LOCALAPPDATA%\TBA-uploader on the AV PC).
func helperConfigPath() string {
	return filepath.Join(dataRoot(), "helper-config.json")
}

type helperConfig struct {
	VideoDir string `json:"video_dir"`
}

// loadHelperConfig reads the persisted config, returning a zero value when none
// exists or it can't be parsed.
func loadHelperConfig() helperConfig {
	var c helperConfig
	data, err := os.ReadFile(helperConfigPath())
	if err != nil {
		return c
	}
	_ = json.Unmarshal(data, &c)
	return c
}

// saveHelperConfig writes the config, creating dataRoot() if needed.
func saveHelperConfig(c helperConfig) error {
	if err := os.MkdirAll(filepath.Dir(helperConfigPath()), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(helperConfigPath(), data, 0o644)
}

// isEventFolder reports whether dir is a FIM-AV event folder — one holding the
// shared upload database, or (legacy, during rollout) a fimav-matches.json
// manifest. Either marks a folder FIM-AV Assistant is recording into.
func isEventFolder(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, fimavManifest))
	return err == nil
}

// eventFolders lists the immediate subdirectories of dir that are valid event
// folders, as full paths, sorted.
func eventFolders(dir string) []string {
	out := []string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if isEventFolder(full) {
			out = append(out, full)
		}
	}
	sort.Strings(out)
	return out
}
