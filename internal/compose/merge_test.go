package compose

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// loadFiles writes the files (compose.yaml first) and loads them merged.
func loadFiles(t *testing.T, contents ...string) (*Project, string) {
	t.Helper()
	dir := t.TempDir()
	var files []string
	for i, content := range contents {
		path := filepath.Join(dir, "compose.yaml")
		if i > 0 {
			path = filepath.Join(dir, "override"+strings.Repeat("x", i)+".yaml")
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, path)
	}
	project, err := LoadProjectFiles(files, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return project, dir
}

func TestMergeOverrideFile(t *testing.T) {
	project, dir := loadFiles(t, `
name: demo
services:
  web:
    image: nginx
    command: ["nginx", "-g", "daemon off;"]
    environment:
      - A=1
      - B=2
    labels: {tier: front}
    ports:
      - "8080:80"
      - "9000:9000"
    volumes:
      - ./html:/usr/share/nginx/html
      - data:/data
    depends_on: [db]
    networks: [front]
  db:
    image: postgres
networks:
  front:
  back:
volumes:
  data:
`, `
services:
  web:
    command: nginx-debug
    environment:
      B: override
      C: "3"
    labels:
      - extra=yes
    ports:
      - target: 80
        published: 8080
        protocol: tcp
      - "9443:443"
    volumes:
      - ./other:/usr/share/nginx/html:ro
    depends_on:
      db:
        condition: service_healthy
    networks:
      back:
        aliases: [api]
  cache:
    image: redis
`)
	web := project.Services["web"]

	if !reflect.DeepEqual(web.Command, []string{"nginx-debug"}) {
		t.Errorf("command is replaced, got %q", web.Command)
	}
	env := map[string]string{}
	for k, v := range web.Environment {
		env[k] = *v
	}
	if want := map[string]string{"A": "1", "B": "override", "C": "3"}; !reflect.DeepEqual(env, want) {
		t.Errorf("environment = %v, want %v", env, want)
	}
	if want := map[string]string{"tier": "front", "extra": "yes"}; !reflect.DeepEqual(web.Labels, want) {
		t.Errorf("labels = %v, want %v", web.Labels, want)
	}
	var ports []string
	for _, p := range web.Ports {
		ports = append(ports, p.Flag())
	}
	if want := []string{"8080:80", "9000:9000", "9443:443"}; !reflect.DeepEqual(ports, want) {
		t.Errorf("ports are unique by published port and target, got %q", ports)
	}
	if len(web.Volumes) != 2 || web.Volumes[0].Source != filepath.Join(dir, "other") || !web.Volumes[0].ReadOnly {
		t.Errorf("volumes are unique by target, got %+v", web.Volumes)
	}
	if !reflect.DeepEqual(web.DependsOn, []string{"db"}) {
		t.Errorf("depends_on list and mapping merge, got %q", web.DependsOn)
	}
	if want := []string{"demo_front", "demo_back"}; !reflect.DeepEqual(web.Networks, want) {
		t.Errorf("networks = %q, want %q", web.Networks, want)
	}
	if !reflect.DeepEqual(web.NetworkAliases["demo_back"], []string{"api"}) {
		t.Errorf("aliases = %v", web.NetworkAliases)
	}
	if project.Services["cache"] == nil || project.Services["db"] == nil {
		t.Error("services of both files are kept")
	}
}

func TestMergeResetAndOverrideTags(t *testing.T) {
	project, _ := loadFiles(t, `
services:
  web:
    image: nginx
    ports: ["8080:80"]
    environment: {A: "1", B: "2"}
    user: nobody
`, `
services:
  web:
    ports: !reset []
    environment: !override
      C: "3"
    user: !reset null
`)
	web := project.Services["web"]
	if len(web.Ports) != 0 {
		t.Errorf("!reset removes the ports, got %+v", web.Ports)
	}
	if _, ok := web.Environment["A"]; ok || len(web.Environment) != 1 {
		t.Errorf("!override replaces the environment, got %v", web.Environment)
	}
	if web.User != "" {
		t.Errorf("!reset removes user, got %q", web.User)
	}
}

func TestMergeBuildShortAndLongSyntax(t *testing.T) {
	project, dir := loadFiles(t, `
services:
  app:
    build: ./app
`, `
services:
  app:
    build:
      target: dev
      args: [MODE=debug]
`)
	b := project.Services["app"].Build
	if b.Context != filepath.Join(dir, "app") || b.Target != "dev" || b.Args["MODE"] != "debug" {
		t.Errorf("build = %+v", b)
	}
}

func TestMergeEmptyOverrideAndPathsFromFirstFile(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: x\n"), 0o644)
	os.WriteFile(filepath.Join(sub, "empty.yaml"), nil, 0o644)
	os.WriteFile(filepath.Join(sub, "more.yaml"), []byte("services:\n  web:\n    volumes: [./data:/data]\n"), 0o644)
	project, err := LoadProjectFiles([]string{
		filepath.Join(dir, "compose.yaml"), filepath.Join(sub, "empty.yaml"), filepath.Join(sub, "more.yaml"),
	}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := project.Services["web"].Volumes[0].Source; got != filepath.Join(dir, "data") {
		t.Errorf("relative paths resolve against the first file's directory, got %q", got)
	}

	os.WriteFile(filepath.Join(sub, "list.yaml"), []byte("- a\n"), 0o644)
	_, err = LoadProjectFiles([]string{filepath.Join(dir, "compose.yaml"), filepath.Join(sub, "list.yaml")}, "", "")
	if err == nil || !strings.Contains(err.Error(), "expected a mapping") {
		t.Errorf("a non-mapping override file is an error, got %v", err)
	}
}

func TestSingleFileResetTag(t *testing.T) {
	project, _ := loadTestProject(t, "services:\n  web:\n    image: x\n    user: !reset root\n")
	if project.Services["web"].User != "" {
		t.Errorf("!reset applies within a single file, got %q", project.Services["web"].User)
	}
}
