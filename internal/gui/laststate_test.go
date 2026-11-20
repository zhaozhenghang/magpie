package gui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/middleware"
	"github.com/yetone/magpie/internal/plugin"
	"github.com/yetone/magpie/internal/settings"
)

// forgetLastState is a new run: nothing kept in memory, the file read
// again.
func forgetLastState() {
	lastState.Lock()
	lastState.kept, lastState.read = nil, false
	lastState.Unlock()
}

func TestStateListsActivePluginRoutings(t *testing.T) {
	sandboxHome(t)
	forgetLastState()
	t.Cleanup(forgetLastState)
	src := filepath.Join(t.TempDir(), "spread.gateway.js")
	if err := os.WriteFile(src, []byte(`export const strategies = { fewest: (pool, seats) => seats.map((_, i) => i) };`), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := json.Marshal(plugin.List{Plugins: []plugin.Entry{{Spec: src}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settings.Dir(), "plugins.json"), list, 0o600); err != nil {
		t.Fatal(err)
	}
	middleware.Reload()
	t.Cleanup(middleware.Reload)
	stop := middleware.Points().(interface{ Serve() func() }).Serve()
	t.Cleanup(stop)

	srv := Handler(nil, nil)
	routings := func(path string) []middleware.Routing {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		var out struct {
			Settings struct {
				JSRoutings []middleware.Routing `json:"jsRoutings"`
			} `json:"settings"`
		}
		if rec.Code != 200 {
			t.Fatalf("%s returned HTTP %d", path, rec.Code)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Settings.JSRoutings
	}
	for _, path := range []string{"/api/state", "/api/state?last=1"} {
		if got := routings(path); len(got) != 1 || got[0].ID != "spread/fewest" {
			t.Errorf("%s: active routings = %+v", path, got)
		}
	}
	stop()
	for _, path := range []string{"/api/state", "/api/state?last=1"} {
		if got := routings(path); len(got) != 0 {
			t.Errorf("%s: inactive routings = %+v", path, got)
		}
	}
}

// #1519: the Agents page draws the state kept from the last read at once
// (?last=1), this run's or the run before's, with the settings as they are
// now; none kept is null, and the settings' secrets never reach the file.
func TestLastKnownState(t *testing.T) {
	h := sandboxHome(t)
	t.Setenv("CODEX_HOME", filepath.Join(h, ".codex"))
	os.MkdirAll(filepath.Join(h, ".codex"), 0o755)
	os.WriteFile(filepath.Join(h, ".codex", "config.toml"), []byte("model = \"gpt-5.4\"\n"), 0o600)
	s := settings.Load()
	s.GitHubToken = "ghp_secret1519"
	s.Theme = "light"
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	forgetLastState()
	t.Cleanup(forgetLastState)
	srv := Handler(nil, nil)
	get := func(path string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		return strings.TrimSpace(rec.Body.String())
	}
	hasCodex := func(st stateJSON) bool {
		for _, a := range st.Agents {
			if a.ID == "codex" {
				return true
			}
		}
		return false
	}

	if b := get("/api/state?last=1"); b != "null" {
		t.Fatalf("nothing kept yet: want null, got %.200s", b)
	}
	var fresh stateJSON
	json.Unmarshal([]byte(get("/api/state")), &fresh)
	if fresh.Last || !hasCodex(fresh) {
		t.Fatalf("fresh state: last=%v codex=%v", fresh.Last, hasCodex(fresh))
	}
	file, err := os.ReadFile(lastStatePath())
	if err != nil {
		t.Fatalf("the read was not kept: %v", err)
	}
	if strings.Contains(string(file), "ghp_secret1519") {
		t.Error("the kept state holds the GitHub token")
	}

	// the user changes a setting, and magpie starts again
	s = settings.Load()
	s.Theme = "dark"
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	forgetLastState()
	var last stateJSON
	json.Unmarshal([]byte(get("/api/state?last=1")), &last)
	if !last.Last || !hasCodex(last) {
		t.Fatalf("last-known state: last=%v codex=%v", last.Last, hasCodex(last))
	}
	if last.Settings.Theme != "dark" {
		t.Errorf("last-known state's settings are not today's: theme %q", last.Settings.Theme)
	}
	if last.Settings.GitHubToken != "" {
		t.Error("the last-known state sends the GitHub token")
	}

	// another version's is not drawn
	was := Version
	Version = "v-other"
	t.Cleanup(func() { Version = was })
	if b := get("/api/state?last=1"); b != "null" {
		t.Errorf("another version's state was served: %.200s", b)
	}
}
