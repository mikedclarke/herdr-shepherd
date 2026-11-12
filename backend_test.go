package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateBackend clears every input resolveBackend reads, so a case sees only
// what it sets: an empty PATH and home, no plugin environment, and a herdr
// that is not there. It returns the PATH directory and the home.
func isolateBackend(t *testing.T) (bin, home string) {
	t.Helper()
	home, bin = t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	for _, k := range []string{backendEnv, "HERDR_SOCKET_PATH", "HERDR_PLUGIN_CONFIG_DIR", "HERDR_PLUGIN_STATE_DIR"} {
		t.Setenv(k, "")
	}
	t.Setenv("HERDR_BIN_PATH", filepath.Join(bin, "no-such-herdr"))
	return bin, home
}

func writeExe(t *testing.T, path, script string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeConfig(t *testing.T, body string) {
	t.Helper()
	dir := standaloneConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantBackend(t *testing.T, want string) {
	t.Helper()
	got, err := resolveBackend()
	if err != nil || got != want {
		t.Fatalf("got %q %v, want %q", got, err, want)
	}
}

func TestBackendEnvWinsOverEverything(t *testing.T) {
	bin, _ := isolateBackend(t)
	writeExe(t, filepath.Join(bin, "hc"), "exit 0")
	writeConfig(t, `backend = "herdr"`)
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
	t.Setenv(backendEnv, "core")
	wantBackend(t, backendCore)

	writeConfig(t, `backend = "core"`)
	t.Setenv(backendEnv, "herdr")
	wantBackend(t, backendHerdr)
}

func TestBackendEnvRejectsAnUnknownValue(t *testing.T) {
	isolateBackend(t)
	t.Setenv(backendEnv, "Core")
	if _, err := resolveBackend(); err == nil || !strings.Contains(err.Error(), backendEnv) {
		t.Fatalf("got %v", err)
	}
}

func TestBackendConfigWinsOverDetection(t *testing.T) {
	bin, _ := isolateBackend(t)
	writeConfig(t, `backend = "core"`)
	t.Setenv("HERDR_SOCKET_PATH", "/tmp/herdr.sock")
	wantBackend(t, backendCore)

	t.Setenv("HERDR_SOCKET_PATH", "")
	writeExe(t, filepath.Join(bin, "hc"), "exit 0")
	writeConfig(t, `backend = "herdr"`)
	wantBackend(t, backendHerdr)
}

func TestBackendConfigErrors(t *testing.T) {
	isolateBackend(t)
	writeConfig(t, `backend = "tmux"`)
	if _, err := resolveBackend(); err == nil || !strings.Contains(err.Error(), "config.toml") {
		t.Errorf("unknown value: got %v", err)
	}
	writeConfig(t, `backend = `)
	if _, err := resolveBackend(); err == nil {
		t.Error("malformed TOML must be an error, not a silent fallback")
	}
}

func TestBackendConfigWithoutTheKeyOrFileFallsThrough(t *testing.T) {
	bin, _ := isolateBackend(t)
	writeExe(t, filepath.Join(bin, "hc"), "exit 0")
	wantBackend(t, backendCore)
	writeConfig(t, "# nothing set\n")
	wantBackend(t, backendCore)
}

// config.toml is read from the standalone config dir only, never from the
// plugin config dir the terminal Herdr injects.
func TestBackendConfigIsReadFromTheStandaloneDirOnly(t *testing.T) {
	bin, _ := isolateBackend(t)
	writeExe(t, filepath.Join(bin, "hc"), "exit 0")
	plugin := t.TempDir()
	if err := os.WriteFile(filepath.Join(plugin, "config.toml"), []byte(`backend = "core"`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", plugin)
	wantBackend(t, backendHerdr)
}

func TestBackendDetectsThePluginEnvironment(t *testing.T) {
	for _, k := range []string{"HERDR_SOCKET_PATH", "HERDR_PLUGIN_CONFIG_DIR"} {
		t.Run(k, func(t *testing.T) {
			bin, _ := isolateBackend(t)
			writeExe(t, filepath.Join(bin, "hc"), "exit 0")
			t.Setenv(k, t.TempDir())
			wantBackend(t, backendHerdr)
		})
	}
}

func TestBackendDetectsAnAnsweringHerdr(t *testing.T) {
	bin, home := isolateBackend(t)
	writeExe(t, filepath.Join(bin, "hc"), "exit 0")
	herdr := filepath.Join(bin, "herdr")
	t.Setenv("HERDR_BIN_PATH", herdr)
	writeExe(t, herdr, `[ "$1 $2" = "plugin config-dir" ] && echo `+home+`/plugin-config`)
	wantBackend(t, backendHerdr)

	writeExe(t, herdr, "exit 1")
	wantBackend(t, backendCore)
}

func TestBackendDetectsHC(t *testing.T) {
	t.Run("on PATH", func(t *testing.T) {
		bin, _ := isolateBackend(t)
		writeExe(t, filepath.Join(bin, "hc"), "exit 0")
		wantBackend(t, backendCore)
	})
	t.Run("in ~/.local/bin", func(t *testing.T) {
		_, home := isolateBackend(t)
		writeExe(t, filepath.Join(home, ".local", "bin", "hc"), "exit 0")
		wantBackend(t, backendCore)
	})
	t.Run("a directory named hc is not hc", func(t *testing.T) {
		_, home := isolateBackend(t)
		if err := os.MkdirAll(filepath.Join(home, ".local", "bin", "hc"), 0o755); err != nil {
			t.Fatal(err)
		}
		wantBackend(t, backendHerdr)
	})
}

func TestBackendDefaultsToHerdr(t *testing.T) {
	isolateBackend(t)
	wantBackend(t, backendHerdr)
	if _, err := newClient(); err == nil || !strings.Contains(err.Error(), "HERDR_SOCKET_PATH is not set") {
		t.Errorf("the herdr default must keep today's error, got %v", err)
	}
}

func TestResolvePathsOnCoreUsesStandaloneDirsWithoutAskingHerdr(t *testing.T) {
	bin, home := isolateBackend(t)
	asked := filepath.Join(home, "herdr-was-asked")
	herdr := filepath.Join(bin, "herdr")
	writeExe(t, herdr, "touch "+asked+"; echo "+home+"/plugin-config")
	t.Setenv("HERDR_BIN_PATH", herdr)
	t.Setenv(backendEnv, "core")
	p := resolvePaths()
	if p.ConfigDir != filepath.Join(home, "config", "herdr-shepherd") || p.StateDir != filepath.Join(home, "state", "herdr-shepherd") {
		t.Errorf("got %+v", p)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	p = resolvePaths()
	if p.ConfigDir != filepath.Join(home, ".config", "herdr-shepherd") || p.StateDir != filepath.Join(home, ".local", "state", "herdr-shepherd") {
		t.Errorf("without XDG: got %+v", p)
	}
	if _, err := os.Stat(asked); !os.IsNotExist(err) {
		t.Error("resolving paths on core ran herdr")
	}
}

func TestResolvePathsOnCoreStillHonoursInjectedDirs(t *testing.T) {
	isolateBackend(t)
	t.Setenv(backendEnv, "core")
	cfg, state := t.TempDir(), t.TempDir()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", cfg)
	t.Setenv("HERDR_PLUGIN_STATE_DIR", state)
	if p := resolvePaths(); p.ConfigDir != cfg || p.StateDir != state {
		t.Errorf("got %+v", p)
	}
}

func TestNewClientOnCore(t *testing.T) {
	bin, home := isolateBackend(t)
	t.Setenv(backendEnv, "core")
	if _, err := newClient(); err == nil || !strings.Contains(err.Error(), "hc") {
		t.Errorf("core without hc: got %v", err)
	}
	writeExe(t, filepath.Join(bin, "hc"), "exit 0")
	if _, err := newClient(); err == nil || !strings.Contains(err.Error(), "herdr-app") {
		t.Errorf("core without herdr-app: got %v", err)
	}
	writeExe(t, filepath.Join(home, ".local", "bin", "herdr-app"), "exit 0")
	client, err := newClient()
	if err != nil {
		t.Fatal(err)
	}
	c, ok := client.(*coreClient)
	if !ok {
		t.Fatalf("got %T", client)
	}
	if c.hc != filepath.Join(bin, "hc") || c.app != filepath.Join(home, ".local", "bin", "herdr-app") ||
		c.stateDir != filepath.Join(home, "state", "herdr-shepherd") {
		t.Errorf("got %+v", c)
	}
}

func TestStartRefusesTheHerdrBackend(t *testing.T) {
	isolateBackend(t)
	t.Setenv(backendEnv, "herdr")
	if err := cmdStart(); err == nil || !strings.Contains(err.Error(), "startup hook") {
		t.Errorf("got %v", err)
	}
}

// Detection that had to ask herdr hands its answer to resolvePaths: one
// lookup per resolve, as before there was a backend to detect.
func TestResolvePathsAsksHerdrOnce(t *testing.T) {
	bin, home := isolateBackend(t)
	calls := filepath.Join(home, "herdr-calls")
	herdr := filepath.Join(bin, "herdr")
	writeExe(t, herdr, "echo x >> "+calls+"; echo "+home+"/plugin-config")
	t.Setenv("HERDR_BIN_PATH", herdr)
	p := resolvePaths()
	if p.Backend != backendHerdr || p.ConfigDir != filepath.Join(home, "plugin-config") {
		t.Fatalf("got %+v", p)
	}
	data, _ := os.ReadFile(calls)
	if n := strings.Count(string(data), "x"); n != 1 {
		t.Errorf("herdr asked %d times", n)
	}
}
