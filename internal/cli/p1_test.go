package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestComposeFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"compose.yaml", "compose.override.yaml", "extra.yaml"} {
		os.WriteFile(filepath.Join(dir, name), []byte("services: {}\n"), 0o644)
	}
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	chdir(t, sub)
	t.Setenv("COMPOSE_FILE", "")

	files, err := (&globals{}).composeFiles()
	if err != nil || len(files) != 2 || filepath.Base(files[0]) != "compose.yaml" || filepath.Base(files[1]) != "compose.override.yaml" {
		t.Errorf("auto-detection: %q, %v", files, err)
	}

	files, err = (&globals{files: []string{"../extra.yaml"}}).composeFiles()
	if err != nil || !reflect.DeepEqual(files, []string{"../extra.yaml"}) {
		t.Errorf("-f disables the override file: %q, %v", files, err)
	}

	t.Setenv("COMPOSE_FILE", "../compose.yaml|../extra.yaml")
	t.Setenv("COMPOSE_PATH_SEPARATOR", "|")
	files, err = (&globals{}).composeFiles()
	if err != nil || !reflect.DeepEqual(files, []string{"../compose.yaml", "../extra.yaml"}) {
		t.Errorf("COMPOSE_FILE: %q, %v", files, err)
	}

	if _, err := (&globals{files: []string{"missing.yaml"}}).composeFiles(); err == nil {
		t.Error("a missing -f file is an error")
	}
}

func TestComposeFileFromDotenv(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env"), []byte("COMPOSE_FILE=a.yaml\n"), 0o644)
	chdir(t, dir)
	t.Setenv("COMPOSE_FILE", "")
	os.Unsetenv("COMPOSE_FILE") // restored by t.Setenv's cleanup
	t.Setenv("COMPOSE_PATH_SEPARATOR", "")
	if got := composeFileVariable(""); !reflect.DeepEqual(got, []string{"a.yaml"}) {
		t.Errorf("got %q", got)
	}
}
