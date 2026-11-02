package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func installedSmoke(ctx context.Context, binary, root, examples string) error {
	if !filepath.IsAbs(binary) || !filepath.IsAbs(root) || !filepath.IsAbs(examples) {
		return errInvalid
	}
	for _, name := range []string{"stages.example.json", "templates.example.json"} {
		raw, e := os.ReadFile(filepath.Join(examples, name))
		if e != nil || len(raw) > 1<<20 || !json.Valid(raw) {
			return errors.New("acceptance: installed examples invalid")
		}
	}
	st, e := os.Stat(filepath.Join(examples, "stages/newsletter.sh"))
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return errors.New("acceptance: installed example executable missing")
	}
	if e = os.Mkdir(root, 0700); e != nil {
		return errInvalid
	}
	invoke := func(profile string, args ...string) ([]byte, error) {
		home := filepath.Join(root, profile)
		if e := os.MkdirAll(home, 0700); e != nil {
			return nil, errInvalid
		}
		env := cleanEnv(map[string]string{"HOME": home, "MAILPIPE_CONFIG": filepath.Join(home, "config.json"), "MAILPIPE_DB": filepath.Join(home, "mirror.db"), "MAILPIPE_OUT_DIR": filepath.Join(home, "out"), "MAILPIPE_STAGES": filepath.Join(home, "stages.json"), "MAILPIPE_TEMPLATES": filepath.Join(home, "templates.json")})
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = env
		b, e := cmd.Output()
		if e != nil || len(b) > 1<<20 {
			return nil, errors.New("acceptance: installed command failed")
		}
		return b, nil
	}
	for _, args := range [][]string{{"--help"}, {"config", "init", "--help"}, {"auth", "login", "--help"}, {"account", "--help"}} {
		b, e := invoke("help", args...)
		if e != nil {
			return e
		}
		if len(args) == 1 && (!strings.Contains(string(b), "Graph") || !strings.Contains(string(b), "read-only Yahoo")) {
			return errors.New("acceptance: installed help contract failed")
		}
	}
	if _, e = invoke("graph", "config", "init", "--client-id", "11111111-1111-4111-8111-111111111111", "--tenant-id", "22222222-2222-4222-8222-222222222222"); e != nil {
		return e
	}
	b, e := invoke("graph", "config", "show", "--json")
	if e != nil || !strings.Contains(string(b), "11111111-1111-4111-8111-111111111111") {
		return errors.New("acceptance: Graph synthetic config failed")
	}
	if _, e = invoke("yahoo", "config", "init", "--provider", "yahoo", "--endpoint", "mail.example.org:993", "--login", "alex@example.com"); e != nil {
		return e
	}
	if _, e = invoke("yahoo", "account", "show", "--json"); e != nil {
		return e
	}
	b, e = invoke("yahoo", "account", "capabilities", "--json")
	if e != nil {
		return e
	}
	var caps struct {
		Provider string `json:"provider"`
		Read     bool   `json:"read"`
		Sync     bool   `json:"sync"`
		Trash    bool   `json:"trash"`
		Archive  bool   `json:"archive"`
		Native   bool   `json:"native_rules"`
		Save     bool   `json:"save_execute"`
	}
	if strictJSON(b, &caps) != nil || caps.Provider != "yahoo" || !caps.Read || !caps.Sync || caps.Trash || caps.Archive || caps.Native || caps.Save {
		return errors.New("acceptance: Yahoo capability smoke failed")
	}
	for _, profile := range []string{"graph", "yahoo"} {
		p := filepath.Join(root, profile, "config.json")
		st, e := os.Lstat(p)
		if e != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
			return errInvalid
		}
		raw, e := os.ReadFile(p)
		if e != nil {
			return errInvalid
		}
		var cfg map[string]any
		if json.Unmarshal(raw, &cfg) != nil {
			return errInvalid
		}
		processing, ok := cfg["processing"].(map[string]any)
		if !ok || processing["models"] != false || processing["external_stages"] != false {
			return errors.New("acceptance: fresh processing is not disabled")
		}
		for k := range cfg {
			if strings.Contains(k, "password") || strings.Contains(k, "token") {
				return errInvalid
			}
		}
		if profile == "yahoo" {
			imap, ok := cfg["imap"].(map[string]any)
			if !ok || imap["endpoint"] != "mail.example.org:993" || imap["login"] != "alex@example.com" || cfg["provider"] != "yahoo" {
				return errInvalid
			}
		}
	}
	return nil
}
