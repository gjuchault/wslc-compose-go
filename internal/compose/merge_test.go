package compose

import (
	"encoding/json"
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
	if !reflect.DeepEqual(web.DependsOn, []string{"db"}) || web.DependsOnConditions["db"] != ConditionHealthy {
		t.Errorf("depends_on list and mapping merge, got %q %v", web.DependsOn, web.DependsOnConditions)
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

func TestHealthcheck(t *testing.T) {
	project, _ := loadTestProject(t, `
services:
  exec:
    image: x
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost/it's"]
      interval: 5s
      timeout: 2s
      start_period: 10s
      retries: 3
  shell:
    image: x
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U $$USER"]
  plain:
    image: x
    healthcheck:
      test: redis-cli ping
  none:
    image: x
    healthcheck:
      test: ["NONE"]
  disabled:
    image: x
    healthcheck:
      disable: true
  timings:
    image: x
    healthcheck:
      interval: 1m
`)
	cases := map[string]Healthcheck{
		"exec":     {Test: `curl -f 'http://localhost/it'"'"'s'`, Interval: "5s", Timeout: "2s", StartPeriod: "10s", Retries: 3},
		"shell":    {Test: "pg_isready -U $USER"},
		"plain":    {Test: "redis-cli ping"},
		"none":     {Disable: true},
		"disabled": {Disable: true},
		"timings":  {Interval: "1m"},
	}
	for name, want := range cases {
		if got := project.Services[name].Healthcheck; got == nil || *got != want {
			t.Errorf("%s: healthcheck = %+v, want %+v", name, got, want)
		}
	}
	for _, w := range project.Warnings {
		if strings.Contains(w, "healthcheck") {
			t.Errorf("healthcheck is supported, got warning %q", w)
		}
	}

	args := strings.Join(RunArgs(project, project.Services["exec"], 1, true, nil), " ")
	for _, want := range []string{"--health-cmd curl -f", "--health-interval 5s", "--health-timeout 2s", "--health-start-period 10s", "--health-retries 3"} {
		if !strings.Contains(args, want) {
			t.Errorf("run args lack %q: %s", want, args)
		}
	}
	if args := RunArgs(project, project.Services["none"], 1, true, nil); !contains(args, "--no-healthcheck") {
		t.Errorf("a disabled healthcheck passes --no-healthcheck: %q", args)
	}
}

func TestHealthcheckErrors(t *testing.T) {
	for name, hc := range map[string]string{
		"bad test":    `{test: ["WHAT", "x"]}`,
		"bad retries": `{test: "true", retries: many}`,
		"not a map":   `"true"`,
	} {
		dir := writeCompose(t, "services:\n  web:\n    image: x\n    healthcheck: "+hc+"\n")
		if _, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", ""); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// Fields added to Service must not change the config hash of services that
// do not use them, or every existing container would be recreated.
func TestNewFieldsKeepConfigHash(t *testing.T) {
	project, _ := loadTestProject(t, `
services:
  web:
    image: x
    depends_on:
      db: {condition: service_healthy}
  db:
    image: y
`)
	blob, _ := json.Marshal(project.Services["web"])
	for _, field := range []string{"Healthcheck", "DependsOnConditions"} {
		if strings.Contains(string(blob), field) {
			t.Errorf("%s is part of the hashed JSON: %s", field, blob)
		}
	}
}

func TestDependsOnConditions(t *testing.T) {
	project, _ := loadTestProject(t, `
services:
  web:
    image: x
    depends_on:
      db: {condition: service_healthy}
      migrate: {condition: service_completed_successfully}
      cache: {condition: service_started}
      odd: {condition: whatever}
  db: {image: x}
  migrate: {image: x}
  cache: {image: x}
  odd: {image: x}
`)
	want := map[string]string{"db": ConditionHealthy, "migrate": ConditionCompleted}
	if got := project.Services["web"].DependsOnConditions; !reflect.DeepEqual(got, want) {
		t.Errorf("conditions = %v, want %v", got, want)
	}
	if len(project.Warnings) != 1 || !strings.Contains(project.Warnings[0], "whatever") {
		t.Errorf("warnings = %q", project.Warnings)
	}
}

func TestMultiNetworkArgs(t *testing.T) {
	project, _ := loadTestProject(t, `
name: demo
services:
  web:
    image: x
    networks:
      front: {aliases: [www]}
      back: {aliases: [api]}
networks:
  front:
  back:
`)
	web := project.Services["web"]
	create := CreateArgs(project, web, 1, nil)
	if create[0] != "create" || contains(create, "-d") || after(t, create, "--network") != "demo_front" {
		t.Errorf("create args = %q", create)
	}
	want := [][]string{{"network", "connect", "--network-alias", "api", "--network-alias", "web", "demo_back", "demo-web-1"}}
	if got := ConnectArgs(web, "demo-web-1", true); !reflect.DeepEqual(got, want) {
		t.Errorf("connect args = %q, want %q", got, want)
	}
	if got := ConnectArgs(web, "x", false); contains(got[0], "--network-alias") {
		t.Errorf("connect without aliases = %q", got)
	}

	oneOff := OneOffArgs([]string{"run", "--rm"}, project, web, "demo-web-run-1", nil)
	if contains(oneOff, "--network-alias") || !contains(oneOff, LabelOneOff+"=True") || contains(oneOff, LabelIndex+"=1") {
		t.Errorf("one-off args = %q", oneOff)
	}
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
