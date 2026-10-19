package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withMigrations remplace la table de migrations et la version courante pour un test.
func withMigrations(t *testing.T, ms []Migration) {
	t.Helper()
	old := migrations
	migrations = ms
	t.Cleanup(func() { migrations = old })
}

func TestMigrateStampsOldFileAndKeepsComments(t *testing.T) {
	src := "# ma config\n[settings]\nlock_file = \"/x\" # verrou\n\n[[service]]\nname = \"a\"\n"
	out, steps, err := Migrate([]byte(src))
	if err != nil || len(steps) != 1 {
		t.Fatalf("steps=%v err=%v", steps, err)
	}
	want := "# ma config\n[settings]\nconfig_version = 1\nlock_file = \"/x\" # verrou\n\n[[service]]\nname = \"a\"\n"
	if string(out) != want {
		t.Fatalf("sortie :\n%s", out)
	}
	// idempotent : un fichier à jour n'est pas retouché
	again, steps, err := Migrate(out)
	if err != nil || len(steps) != 0 || string(again) != string(out) {
		t.Fatalf("seconde passe : %v %v", steps, err)
	}
}

func TestMigrateAddsSettingsTableWhenMissing(t *testing.T) {
	out, _, err := Migrate([]byte("[[service]]\nname = \"a\"\n"))
	if err != nil || !strings.HasPrefix(string(out), "[settings]\nconfig_version = 1\n") {
		t.Fatalf("%q %v", out, err)
	}
}

func TestMigrateRefusesNewerVersionAndBadValue(t *testing.T) {
	if _, _, err := Migrate([]byte("[settings]\nconfig_version = 99\n")); err == nil || !strings.Contains(err.Error(), "plus récent") {
		t.Fatalf("err=%v", err)
	}
	for _, bad := range []string{"\"x\"", "-1", "true"} {
		if _, _, err := Migrate([]byte("[settings]\nconfig_version = " + bad + "\n")); err == nil {
			t.Fatalf("%s accepté", bad)
		}
	}
}

// Une vraie migration (clé renommée) : l'ancien fichier se charge, les étapes s'enchaînent dans l'ordre et la version est posée.
func TestMigrationChainRenamesKey(t *testing.T) {
	withMigrations(t, append(append([]Migration{}, migrations...), Migration{From: 1, What: "renomme", Apply: func(s string) (string, error) {
		return strings.ReplaceAll(s, "ancien_verrou", "lock_file"), nil
	}}))
	out, steps, err := Migrate([]byte("[settings]\nancien_verrou = \"/y\"\n"))
	if err != nil || len(steps) != 2 || !strings.Contains(string(out), "lock_file = \"/y\"") || !strings.Contains(string(out), "config_version = 2") {
		t.Fatalf("steps=%v err=%v\n%s", steps, err, out)
	}
	// un fichier déjà en version 1 ne passe que par la seconde étape
	out, steps, err = Migrate([]byte("[settings]\nconfig_version = 1\nancien_verrou = \"/y\"\n"))
	if err != nil || len(steps) != 1 || !strings.Contains(string(out), "config_version = 2") || strings.Contains(string(out), "ancien") {
		t.Fatalf("steps=%v err=%v\n%s", steps, err, out)
	}
}

func TestFailingMigrationIsReported(t *testing.T) {
	withMigrations(t, append(append([]Migration{}, migrations...), Migration{From: 1, What: "casse", Apply: func(string) (string, error) {
		return "", os.ErrInvalid
	}}))
	if _, _, err := Migrate([]byte("[settings]\nconfig_version = 1\n")); err == nil || !strings.Contains(err.Error(), "1 -> 2") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoadMigratesInMemoryOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	src := "[settings]\nstate_file = \"/s\"\n[[service]]\nname = \"a\"\ncompose_dir = \"/d\"\nservice = \"a\"\n"
	os.WriteFile(p, []byte(src), 0o600)
	cfg, err := Load(p)
	if err != nil || len(cfg.Migrations) != 1 {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	if b, _ := os.ReadFile(p); string(b) != src {
		t.Fatal("Load a réécrit le fichier")
	}
}

func TestLoadRefusesConfigFromNewerVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.toml")
	os.WriteFile(p, []byte("[settings]\nconfig_version = 2\n"), 0o600)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "plus récent") {
		t.Fatalf("err=%v", err)
	}
}

func TestCurrentFileIsReturnedUntouched(t *testing.T) {
	src := "[settings]\nconfig_version=1   # à moi\n"
	out, steps, err := Migrate([]byte(src))
	if err != nil || len(steps) != 0 || string(out) != src {
		t.Fatalf("%q %v %v", out, steps, err)
	}
}
