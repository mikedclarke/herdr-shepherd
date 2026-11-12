package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// The two Herdrs shepherd can drive: the terminal Herdr, whose plugin it is,
// and the desktop app with its herdr-core runtime.
const (
	backendHerdr = "herdr"
	backendCore  = "core"
	backendEnv   = "HERDR_SHEPHERD_BACKEND"
)

// resolveBackend picks the backend: the environment variable, then
// `backend` in config.toml in the standalone config dir, then what is
// installed. A plugin environment or an answering herdr means the terminal
// Herdr; otherwise hc means the core; with neither it is herdr, whose client
// then reports what is missing.
func resolveBackend() (string, error) {
	b, _, err := detectBackend()
	return b, err
}

// herdrAnswer is what detection learned by asking herdr for its managed
// config dir, so resolvePaths does not ask a second time.
type herdrAnswer struct {
	asked bool
	dir   string
}

func detectBackend() (string, herdrAnswer, error) {
	var ans herdrAnswer
	if v := os.Getenv(backendEnv); v != "" {
		if !validBackend(v) {
			return "", ans, fmt.Errorf("%s=%q: want %q or %q", backendEnv, v, backendHerdr, backendCore)
		}
		return v, ans, nil
	}
	v, err := configBackend()
	if err != nil {
		return "", ans, err
	}
	if v != "" {
		return v, ans, nil
	}
	if os.Getenv("HERDR_SOCKET_PATH") != "" || os.Getenv("HERDR_PLUGIN_CONFIG_DIR") != "" {
		return backendHerdr, ans, nil
	}
	ans = herdrAnswer{asked: true, dir: herdrConfigDir()}
	if ans.dir != "" {
		return backendHerdr, ans, nil
	}
	if findProgram("hc") != "" {
		return backendCore, ans, nil
	}
	return backendHerdr, ans, nil
}

func validBackend(v string) bool { return v == backendHerdr || v == backendCore }

// mustDetectBackend is detectBackend for callers with no error path; a
// setting that names no backend stops the program rather than drive the wrong
// Herdr.
func mustDetectBackend() (string, herdrAnswer) {
	b, ans, err := detectBackend()
	if err != nil {
		fmt.Fprintln(os.Stderr, "herdr-shepherd:", err)
		os.Exit(1)
	}
	return b, ans
}

// standaloneConfigDir is the config dir outside the terminal Herdr's plugin
// directories, or "" when there is no home to put it in.
func standaloneConfigDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "herdr-shepherd")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "herdr-shepherd")
	}
	return ""
}

// configFile names config.toml; it lives in the standalone config dir only,
// so the backend can be read before any Herdr is asked for its directories.
func configFile() string {
	dir := standaloneConfigDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "config.toml")
}

// configBackend reads `backend` from config.toml. A missing file, or a file
// without the key, is no setting.
func configBackend() (string, error) {
	path := configFile()
	if path == "" {
		return "", nil
	}
	var cfg struct {
		Backend string `toml:"backend"`
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	if cfg.Backend != "" && !validBackend(cfg.Backend) {
		return "", fmt.Errorf("%s: backend = %q: want %q or %q", path, cfg.Backend, backendHerdr, backendCore)
	}
	return cfg.Backend, nil
}

// newClient opens the client for the backend in use.
func newClient() (herdrAPI, error) {
	if p := resolvePaths(); p.Backend == backendCore {
		c, err := newCoreClient(p.StateDir)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	c, err := newHerdrClient()
	if err != nil {
		return nil, err
	}
	return c, nil
}
