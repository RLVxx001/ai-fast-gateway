package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type bridgeSessionRotator struct {
	cfg     config
	mu      sync.Mutex
	entries map[string]*bridgeSessionRotation
}

type bridgeSessionRotation struct {
	Original    string `json:"original"`
	Current     string `json:"current"`
	Failures    int    `json:"failures"`
	Generation  int    `json:"generation"`
	UpdatedUnix int64  `json:"updated_unix"`
}

type bridgeSessionRotationView struct {
	Key         string `json:"key"`
	Original    string `json:"original"`
	Current     string `json:"current"`
	Failures    int    `json:"failures"`
	Generation  int    `json:"generation"`
	UpdatedUnix int64  `json:"updated_unix"`
	UpdatedAt   string `json:"updated_at"`
	Rotated     bool   `json:"rotated"`
}

func newBridgeSessionRotator(cfg config) *bridgeSessionRotator {
	r := &bridgeSessionRotator{
		cfg:     cfg,
		entries: map[string]*bridgeSessionRotation{},
	}
	r.load()
	return r
}

func (r *bridgeSessionRotator) enabled() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	enabled := r.cfg.ccWSSessionRotateEnabled
	r.mu.Unlock()
	return enabled
}

func (r *bridgeSessionRotator) updateConfig(cfg config) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
}

func (r *bridgeSessionRotator) effectiveSession(clientKey string, original string) string {
	if !r.enabled() {
		return strings.TrimSpace(original)
	}
	original = strings.TrimSpace(original)
	if original == "" || original == "default" || original == "responses" {
		return original
	}
	key := bridgeSessionRotationKey(clientKey, original)
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry := r.entries[key]; entry != nil && strings.TrimSpace(entry.Current) != "" {
		return entry.Current
	}
	return original
}

func (r *bridgeSessionRotator) markSuccess(clientKey string, original string) {
	if !r.enabled() {
		return
	}
	original = strings.TrimSpace(original)
	if original == "" {
		return
	}
	key := bridgeSessionRotationKey(clientKey, original)
	r.mu.Lock()
	entry := r.entries[key]
	if entry == nil || entry.Failures == 0 {
		r.mu.Unlock()
		return
	}
	entry.Failures = 0
	entry.UpdatedUnix = time.Now().Unix()
	r.mu.Unlock()
	r.save()
}

func (r *bridgeSessionRotator) markFirstEventFailure(clientKey string, original string, err error) string {
	if !r.enabled() {
		return strings.TrimSpace(original)
	}
	original = strings.TrimSpace(original)
	if original == "" || original == "default" || original == "responses" {
		return original
	}
	r.mu.Lock()
	threshold := positiveOrDefault(r.cfg.ccWSSessionRotateThreshold, 2)
	r.mu.Unlock()
	key := bridgeSessionRotationKey(clientKey, original)

	r.mu.Lock()
	entry := r.entries[key]
	if entry == nil {
		entry = &bridgeSessionRotation{
			Original: original,
			Current:  original,
		}
		r.entries[key] = entry
	}
	entry.Failures++
	rotated := false
	if entry.Failures >= threshold {
		entry.Generation++
		entry.Current = original + ":wsr" + randomHex(4) + "-" + shortHash(time.Now().Format(time.RFC3339Nano))
		entry.Failures = 0
		rotated = true
	}
	entry.UpdatedUnix = time.Now().Unix()
	current := entry.Current
	generation := entry.Generation
	failures := entry.Failures
	r.mu.Unlock()

	if rotated {
		log.Printf("cc ws session rotated: client_key=%s original_session=%s new_session=%s generation=%d err=%v",
			truncateLogValue(clientKey, 32), truncateLogValue(original, 16), truncateLogValue(current, 16), generation, err)
	} else {
		log.Printf("cc ws session first-event failure recorded: client_key=%s original_session=%s failures=%d/%d err=%v",
			truncateLogValue(clientKey, 32), truncateLogValue(original, 16), failures, threshold, err)
	}
	r.save()
	return current
}

func (r *bridgeSessionRotator) list() []bridgeSessionRotationView {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]bridgeSessionRotationView, 0, len(r.entries))
	for key, entry := range r.entries {
		out = append(out, bridgeSessionRotationView{
			Key:         key,
			Original:    entry.Original,
			Current:     entry.Current,
			Failures:    entry.Failures,
			Generation:  entry.Generation,
			UpdatedUnix: entry.UpdatedUnix,
			UpdatedAt:   formatUnixTime(entry.UpdatedUnix),
			Rotated:     entry.Current != "" && entry.Current != entry.Original,
		})
	}
	return out
}

func (r *bridgeSessionRotator) delete(key string) bool {
	if r == nil {
		return false
	}
	key = strings.TrimSpace(key)
	r.mu.Lock()
	_, ok := r.entries[key]
	if ok {
		delete(r.entries, key)
	}
	r.mu.Unlock()
	if ok {
		r.save()
	}
	return ok
}

func (r *bridgeSessionRotator) forceRotate(key string) (bridgeSessionRotationView, bool) {
	if r == nil {
		return bridgeSessionRotationView{}, false
	}
	key = strings.TrimSpace(key)
	r.mu.Lock()
	entry := r.entries[key]
	if entry == nil {
		r.mu.Unlock()
		return bridgeSessionRotationView{}, false
	}
	entry.Generation++
	entry.Current = entry.Original + ":wsr" + randomHex(4) + "-" + shortHash(time.Now().Format(time.RFC3339Nano))
	entry.Failures = 0
	entry.UpdatedUnix = time.Now().Unix()
	view := bridgeSessionRotationView{
		Key:         key,
		Original:    entry.Original,
		Current:     entry.Current,
		Failures:    entry.Failures,
		Generation:  entry.Generation,
		UpdatedUnix: entry.UpdatedUnix,
		UpdatedAt:   formatUnixTime(entry.UpdatedUnix),
		Rotated:     true,
	}
	r.mu.Unlock()
	log.Printf("cc ws session manually rotated: key=%s original_session=%s new_session=%s generation=%d",
		truncateLogValue(key, 16), truncateLogValue(view.Original, 16), truncateLogValue(view.Current, 16), view.Generation)
	r.save()
	return view, true
}

func (r *bridgeSessionRotator) load() {
	if r == nil || strings.TrimSpace(r.cfg.ccWSSessionRotateStateFile) == "" {
		return
	}
	data, err := os.ReadFile(r.cfg.ccWSSessionRotateStateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("cc ws session rotation state load failed: file=%s err=%v", r.cfg.ccWSSessionRotateStateFile, err)
		}
		return
	}
	var entries map[string]*bridgeSessionRotation
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("cc ws session rotation state decode failed: file=%s err=%v", r.cfg.ccWSSessionRotateStateFile, err)
		return
	}
	r.entries = entries
	log.Printf("cc ws session rotation state loaded: file=%s entries=%d", r.cfg.ccWSSessionRotateStateFile, len(entries))
}

func (r *bridgeSessionRotator) save() {
	if r == nil || strings.TrimSpace(r.cfg.ccWSSessionRotateStateFile) == "" {
		return
	}
	r.mu.Lock()
	snapshot := make(map[string]*bridgeSessionRotation, len(r.entries))
	for key, entry := range r.entries {
		copied := *entry
		snapshot[key] = &copied
	}
	r.mu.Unlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		log.Printf("cc ws session rotation state encode failed: file=%s err=%v", r.cfg.ccWSSessionRotateStateFile, err)
		return
	}
	file := r.cfg.ccWSSessionRotateStateFile
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		log.Printf("cc ws session rotation state mkdir failed: file=%s err=%v", file, err)
		return
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("cc ws session rotation state write failed: file=%s err=%v", file, err)
		return
	}
	if err := os.Rename(tmp, file); err != nil {
		log.Printf("cc ws session rotation state replace failed: file=%s err=%v", file, err)
	}
}

func bridgeSessionRotationKey(clientKey string, original string) string {
	return shortHash(clientKey) + "|" + shortHash(original)
}

func formatUnixTime(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).Format(time.RFC3339)
}
