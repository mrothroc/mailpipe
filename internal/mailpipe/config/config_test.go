package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mrothroc/mailpipe/internal/mailpipe/config"
)

const (
	clientID = "11111111-1111-4111-8111-111111111111"
	tenantID = "22222222-2222-4222-8222-222222222222"
)

func TestSaveThenLoadRoundTripsWithPrivateModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "config.json")
	want := config.Config{ClientID: clientID, TenantID: tenantID}
	if err := config.Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := config.Load(path)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Load = %+v, %v", got, err)
	}
	for p, mode := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
		info, err := os.Stat(p)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s mode %v, want %v", p, info.Mode().Perm(), mode)
		}
	}
}

func TestMissingConfigSaysHowToCreateIt(t *testing.T) {
	_, err := config.Load(filepath.Join(t.TempDir(), "config.json"))
	if !errors.Is(err, config.ErrNotConfigured) || !strings.Contains(config.InitHint, "mailpipe config init") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnvironmentOverridesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(path, config.Config{ClientID: clientID, TenantID: tenantID}); err != nil {
		t.Fatal(err)
	}
	other := "33333333-3333-4333-8333-333333333333"
	t.Setenv("MAILPIPE_CLIENT_ID", other)
	got, err := config.Load(path)
	if err != nil || got.ClientID != other || got.TenantID != tenantID {
		t.Fatalf("Load = %+v, %v", got, err)
	}
}

func TestEnvironmentAloneIsEnough(t *testing.T) {
	t.Setenv("MAILPIPE_CLIENT_ID", clientID)
	t.Setenv("MAILPIPE_TENANT_ID", tenantID)
	got, err := config.Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil || got.ClientID != clientID || got.TenantID != tenantID {
		t.Fatalf("Load = %+v, %v", got, err)
	}
}

func TestIDsMustBeUUIDs(t *testing.T) {
	if err := config.Save(filepath.Join(t.TempDir(), "c.json"), config.Config{ClientID: "nope", TenantID: tenantID}); err == nil {
		t.Fatal("invalid client id accepted")
	}
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte(`{"client_id":"x","tenant_id":"y"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultPathHonoursOverride(t *testing.T) {
	t.Setenv("MAILPIPE_CONFIG", "/tmp/elsewhere.json")
	if got := config.DefaultPath(); got != "/tmp/elsewhere.json" {
		t.Fatalf("DefaultPath = %q", got)
	}
	t.Setenv("MAILPIPE_CONFIG", "")
	home, _ := os.UserHomeDir()
	if got := config.DefaultPath(); got != filepath.Join(home, "mail", "config.json") {
		t.Fatalf("DefaultPath = %q", got)
	}
}
