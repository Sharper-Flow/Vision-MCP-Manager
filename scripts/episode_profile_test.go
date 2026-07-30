package scripts

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testEnv builds a minimal environment for the episode-profile script and makes
// it executable. Tests run in the scripts package directory, so the script is at
// ./episode-profile.
func testEnv(t *testing.T, extras map[string]string) []string {
	t.Helper()
	if err := os.Chmod("episode-profile", 0755); err != nil {
		t.Fatalf("chmod episode-profile: %v", err)
	}

	base := os.Environ()
	for k, v := range extras {
		base = append(base, fmt.Sprintf("%s=%s", k, v))
	}
	return base
}

func writeProcfs(t *testing.T, root string, pid int, vmrss, rssAnon, rssFile, rssShmem, pss int) {
	t.Helper()
	pidDir := filepath.Join(root, fmt.Sprintf("%d", pid))
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatalf("mkdir procfs pid dir: %v", err)
	}
	status := fmt.Sprintf("Name:\tepisode\nPid:\t%d\nVmRSS:\t%d kB\nRssAnon:\t%d kB\nRssFile:\t%d kB\nRssShmem:\t%d kB\n", pid, vmrss, rssAnon, rssFile, rssShmem)
	if err := os.WriteFile(filepath.Join(pidDir, "status"), []byte(status), 0644); err != nil {
		t.Fatalf("write status: %v", err)
	}
	smaps := fmt.Sprintf("Rss:\t%d kB\nPss:\t%d kB\n", vmrss, pss)
	if err := os.WriteFile(filepath.Join(pidDir, "smaps_rollup"), []byte(smaps), 0644); err != nil {
		t.Fatalf("write smaps_rollup: %v", err)
	}
}

func startVisionServer(t *testing.T, pid int) (*httptest.Server, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/servers/Episode", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":               "Episode",
			"state":              "running",
			"pid":                pid,
			"registered_seconds": 3600,
			"uptime_seconds":     1200,
			"session_metrics": map[string]any{
				"active_sessions": 3,
			},
		})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"uptime": "1h0m0s",
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, ts.URL
}

func runScript(t *testing.T, env []string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("./episode-profile", args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("episode-profile %s failed: %v\noutput:\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func TestEpisodeProfileSnapshot(t *testing.T) {
	procfs := t.TempDir()
	const pid = 12345
	writeProcfs(t, procfs, pid, 1500000, 1200000, 100000, 200000, 1400000)

	ts, url := startVisionServer(t, pid)
	_ = ts

	env := testEnv(t, map[string]string{
		"VISION_API_URL": url,
		"PROCFS_ROOT":    procfs,
	})

	out := runScript(t, env, "--name", "Episode", "--json")

	var snap map[string]any
	if err := json.Unmarshal(out, &snap); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v\noutput:\n%s", err, out)
	}

	if got := snap["server_name"]; got != "Episode" {
		t.Errorf("server_name = %v, want Episode", got)
	}
	if got, _ := snap["pid"].(float64); got != pid {
		t.Errorf("pid = %v, want %d", got, pid)
	}

	status, ok := snap["procfs"].(map[string]any)["status"].(map[string]any)
	if !ok {
		t.Fatalf("procfs.status missing or not an object: %v", snap["procfs"])
	}
	assertFloat(t, "VmRSS_kB", status["VmRSS_kB"], 1500000)
	assertFloat(t, "RssAnon_kB", status["RssAnon_kB"], 1200000)
	assertFloat(t, "RssFile_kB", status["RssFile_kB"], 100000)
	assertFloat(t, "RssShmem_kB", status["RssShmem_kB"], 200000)

	smaps, ok := snap["procfs"].(map[string]any)["smaps_rollup"].(map[string]any)
	if !ok {
		t.Fatalf("procfs.smaps_rollup missing or not an object: %v", snap["procfs"])
	}
	assertFloat(t, "Pss_kB", smaps["Pss_kB"], 1400000)

	ctx, ok := snap["context"].(map[string]any)
	if !ok {
		t.Fatalf("context missing: %v", snap)
	}
	if ctx["server"] == nil {
		t.Errorf("context.server is nil")
	}
	if ctx["daemon_health"] == nil {
		t.Errorf("context.daemon_health is nil")
	}

	readErrors, ok := snap["procfs"].(map[string]any)["read_errors"].([]any)
	if !ok || len(readErrors) != 0 {
		t.Errorf("expected no read_errors, got %v", readErrors)
	}
}

func TestEpisodeProfileComparison_Growing(t *testing.T) {
	procfs := t.TempDir()
	const pid = 12345
	writeProcfs(t, procfs, pid, 1500000, 1200000, 100000, 200000, 1400000)

	ts, url := startVisionServer(t, pid)
	_ = ts

	env := testEnv(t, map[string]string{
		"VISION_API_URL": url,
		"PROCFS_ROOT":    procfs,
	})

	baseline := runScript(t, env, "--name", "Episode", "--json")

	// Increase memory by more than 5% and more than 1024 kB.
	writeProcfs(t, procfs, pid, 1600000, 1300000, 100000, 200000, 1500000)

	baselineFile := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(baselineFile, baseline, 0644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}

	out := runScript(t, env, "--name", "Episode", "--json", "--compare", baselineFile)

	var report map[string]any
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("comparison report is not valid JSON: %v\noutput:\n%s", err, out)
	}

	cmp, ok := report["comparison"].(map[string]any)
	if !ok {
		t.Fatalf("comparison missing: %v", report)
	}
	if got := cmp["classification"]; got != "growing" {
		t.Errorf("classification = %v, want growing", got)
	}
	assertFloat(t, "delta_VmRSS_kB", cmp["delta_VmRSS_kB"], 100000)
	assertFloat(t, "delta_RssAnon_kB", cmp["delta_RssAnon_kB"], 100000)
}

func TestEpisodeProfileComparison_Stable(t *testing.T) {
	procfs := t.TempDir()
	const pid = 12345
	writeProcfs(t, procfs, pid, 1500000, 1200000, 100000, 200000, 1400000)

	ts, url := startVisionServer(t, pid)
	_ = ts

	env := testEnv(t, map[string]string{
		"VISION_API_URL": url,
		"PROCFS_ROOT":    procfs,
	})

	baseline := runScript(t, env, "--name", "Episode", "--json")

	// Minimal change well below the 5% / 1024 kB threshold.
	writeProcfs(t, procfs, pid, 1500100, 1200100, 100000, 200000, 1400000)

	baselineFile := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(baselineFile, baseline, 0644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}

	out := runScript(t, env, "--name", "Episode", "--json", "--compare", baselineFile)

	var report map[string]any
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("comparison report is not valid JSON: %v\noutput:\n%s", err, out)
	}

	cmp := report["comparison"].(map[string]any)
	if got := cmp["classification"]; got != "stable" {
		t.Errorf("classification = %v, want stable", got)
	}
}

func TestEpisodeProfileComparison_PidChanged(t *testing.T) {
	procfs := t.TempDir()
	const pid1 = 12345
	writeProcfs(t, procfs, pid1, 1500000, 1200000, 100000, 200000, 1400000)

	// First snapshot with pid1.
	ts, url := startVisionServer(t, pid1)
	_ = ts
	env := testEnv(t, map[string]string{
		"VISION_API_URL": url,
		"PROCFS_ROOT":    procfs,
	})
	baseline := runScript(t, env, "--name", "Episode", "--json")

	// Now the process has been replaced by pid2.
	const pid2 = 12346
	writeProcfs(t, procfs, pid2, 1600000, 1300000, 100000, 200000, 1500000)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/servers/Episode", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":  "Episode",
			"state": "running",
			"pid":   pid2,
		})
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
	})
	ts2 := httptest.NewServer(mux)
	defer ts2.Close()

	env2 := testEnv(t, map[string]string{
		"VISION_API_URL": ts2.URL,
		"PROCFS_ROOT":    procfs,
	})

	baselineFile := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(baselineFile, baseline, 0644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}

	out := runScript(t, env2, "--name", "Episode", "--json", "--compare", baselineFile)

	var report map[string]any
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("comparison report is not valid JSON: %v\noutput:\n%s", err, out)
	}
	cmp := report["comparison"].(map[string]any)
	if got := cmp["classification"]; got != "inconclusive" {
		t.Errorf("classification = %v, want inconclusive", got)
	}
	if got := cmp["pid_changed"]; got != true {
		t.Errorf("pid_changed = %v, want true", got)
	}
}

func TestEpisodeProfileMissingProcfs(t *testing.T) {
	procfs := t.TempDir()
	const pid = 12345

	// Do not create /procfs/pid directory.
	ts, url := startVisionServer(t, pid)
	_ = ts

	env := testEnv(t, map[string]string{
		"VISION_API_URL": url,
		"PROCFS_ROOT":    procfs,
	})

	out := runScript(t, env, "--name", "Episode", "--json")

	var snap map[string]any
	if err := json.Unmarshal(out, &snap); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v\noutput:\n%s", err, out)
	}

	status := snap["procfs"].(map[string]any)["status"].(map[string]any)
	if status["VmRSS_kB"] != nil {
		t.Errorf("expected null VmRSS, got %v", status["VmRSS_kB"])
	}
	readErrors := snap["procfs"].(map[string]any)["read_errors"].([]any)
	if len(readErrors) == 0 {
		t.Errorf("expected read_errors for missing procfs, got none")
	}
}

func assertFloat(t *testing.T, name string, got any, want int) {
	t.Helper()
	f, ok := got.(float64)
	if !ok {
		t.Errorf("%s = %v (%T), want float64", name, got, got)
		return
	}
	if int(f) != want {
		t.Errorf("%s = %v, want %d", name, f, want)
	}
}
