package compose

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Regression tests for the issues found in the first Opus/Astra review.

func TestNetworkMappingKeepsDeclarationOrder(t *testing.T) {
	project, _ := loadTestProject(t, `
name: demo
services:
  a:
    image: x
    networks:
      backend: {}
      admin:
        aliases: [adm]
networks:
  backend:
  admin:
`)
	a := project.Services["a"]
	if want := []string{"demo_backend", "demo_admin"}; !reflect.DeepEqual(a.Networks, want) {
		t.Errorf("networks = %q, want %q", a.Networks, want)
	}
	if got := after(t, RunArgs(project, a, 1, true, nil), "--network"); got != "demo_backend" {
		t.Errorf("--network = %q, want demo_backend", got)
	}
}

func TestYAML11BooleansInBoolFields(t *testing.T) {
	project, _ := loadTestProject(t, `
name: demo
services:
  a:
    image: x
    tty: no
    stdin_open: off
    volumes:
      - type: volume
        source: data
        target: /data
        read_only: no
    networks: [ext]
  b:
    build:
      context: .
      pull: No
    tty: yes
    stdin_open: On
networks:
  ext:
    external: no
volumes:
  data:
`)
	a, b := project.Services["a"], project.Services["b"]
	if a.TTY || a.StdinOpen || a.Volumes[0].ReadOnly {
		t.Errorf("a: tty=%v stdin_open=%v read_only=%v, want all false", a.TTY, a.StdinOpen, a.Volumes[0].ReadOnly)
	}
	if net := project.Networks["ext"]; net.External || net.Name != "demo_ext" {
		t.Errorf("network ext = %+v, want non-external demo_ext", net)
	}
	if b.Build.Pull || !b.TTY || !b.StdinOpen {
		t.Errorf("b: pull=%v tty=%v stdin_open=%v, want false true true", b.Build.Pull, b.TTY, b.StdinOpen)
	}
}

func TestMalformedSectionsRejected(t *testing.T) {
	cases := map[string]struct{ body, field string }{
		"volumes as mapping": {"services:\n  a:\n    image: x\n    volumes: {source: data, target: /data}\nvolumes:\n  data:\n", "volumes"},
		"ports as scalar":    {"services:\n  a:\n    image: x\n    ports: \"8080:80\"\n", "ports"},
		"services as list":   {"services:\n  - a\n", "services"},
		"deploy as scalar":   {"services:\n  a:\n    image: x\n    deploy: 3\n", "deploy"},
		"network cfg scalar": {"services:\n  a:\n    image: x\nnetworks:\n  front: yes\n", "front"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeCompose(t, c.body)
			_, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", "")
			var ce *ComposeError
			if !errors.As(err, &ce) || !strings.Contains(err.Error(), c.field) {
				t.Errorf("expected ComposeError mentioning %q, got %v", c.field, err)
			}
		})
	}
}

func TestDeployReplicas(t *testing.T) {
	project, _ := loadTestProject(t, "services:\n  a:\n    image: x\n    deploy:\n      replicas: 0\n")
	if got := project.Services["a"].Replicas; got != 0 {
		t.Errorf("replicas = %d, want 0", got)
	}

	dir := writeCompose(t, "services:\n  a:\n    image: x\n    deploy:\n      replicas: -1\n")
	if _, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", ""); err == nil || !strings.Contains(err.Error(), "replicas") {
		t.Errorf("expected replicas error, got %v", err)
	}
}

func TestTimestampScalarsKeepTheirText(t *testing.T) {
	project, _ := loadTestProject(t, `
services:
  a:
    image: x
    environment:
      DATE: 2024-01-15
      WHEN: 2024-01-15T10:00:00Z
    labels:
      built: 2024-01-15
`)
	a := project.Services["a"]
	if got := *a.Environment["DATE"]; got != "2024-01-15" {
		t.Errorf("DATE = %q", got)
	}
	if got := *a.Environment["WHEN"]; got != "2024-01-15T10:00:00Z" {
		t.Errorf("WHEN = %q", got)
	}
	if got := a.Labels["built"]; got != "2024-01-15" {
		t.Errorf("label = %q", got)
	}
}

func TestAnchorsAndMergeKeys(t *testing.T) {
	project, _ := loadTestProject(t, `
x-base: &base
  image: x
  environment: {A: "1"}
services:
  a:
    <<: *base
    environment: {B: "2"}
`)
	a := project.Services["a"]
	if a.Image != "x" {
		t.Errorf("image = %q", a.Image)
	}
	if _, ok := a.Environment["A"]; ok || a.Environment["B"] == nil {
		t.Errorf("environment = %v, want only B (explicit keys override merged ones)", a.Environment)
	}
}

func TestCaseInsensitiveEnvironment(t *testing.T) {
	orig := caseInsensitiveEnv
	caseInsensitiveEnv = true
	t.Cleanup(func() { caseInsensitiveEnv = orig })

	got, err := Interpolate("${Path}-${path}", map[string]string{"PATH": "p"})
	if err != nil || got != "p-p" {
		t.Errorf("Interpolate = %q, %v; want p-p", got, err)
	}

	t.Setenv("WSLCGO_Mixed", "process")
	dir := writeCompose(t, "services:\n  a:\n    image: ${WSLCGO_MIXED}\n")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("wslcgo_mixed=dotenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := project.Services["a"].Image; got != "process" {
		t.Errorf("image = %q, want process (process env wins regardless of case)", got)
	}
}

// Regression tests for the issues found in the second Opus/Astra review.

func TestQuotedBooleanStringsAreTruthy(t *testing.T) {
	project, _ := loadTestProject(t, `
name: demo
services:
  a:
    image: x
    tty: "no"
    stdin_open: "false"
    environment:
      DEBUG: yes
      QUIET: "off"
    volumes:
      - type: volume
        source: data
        target: /data
        read_only: "no"
volumes:
  data:
`)
	a := project.Services["a"]
	if !a.TTY || !a.StdinOpen || !a.Volumes[0].ReadOnly {
		t.Errorf("tty=%v stdin_open=%v read_only=%v, want all true (non-empty strings, like Python)",
			a.TTY, a.StdinOpen, a.Volumes[0].ReadOnly)
	}
	if got := *a.Environment["DEBUG"]; got != "yes" {
		t.Errorf("DEBUG = %q, want yes", got)
	}
	if got := *a.Environment["QUIET"]; got != "off" {
		t.Errorf("QUIET = %q, want off", got)
	}
}

func TestMergeKeyOrderMatchesPyYAML(t *testing.T) {
	project, _ := loadTestProject(t, `
name: demo
x-a: &a {first: {}, shared: {}}
x-b: &b {second: {}, shared: {aliases: [from-b]}}
services:
  one:
    image: x
    networks:
      admin: {}
      <<: {backend: {}}
  two:
    image: x
    networks:
      <<: [*a, *b]
      own: {}
networks:
  admin:
  backend:
  first:
  second:
  shared:
  own:
`)
	if got, want := project.Services["one"].Networks, []string{"demo_backend", "demo_admin"}; !reflect.DeepEqual(got, want) {
		t.Errorf("one: networks = %q, want %q", got, want)
	}
	two := project.Services["two"]
	if got, want := two.Networks, []string{"demo_second", "demo_shared", "demo_first", "demo_own"}; !reflect.DeepEqual(got, want) {
		t.Errorf("two: networks = %q, want %q", got, want)
	}
	if got := two.NetworkAliases["demo_shared"]; len(got) != 0 {
		t.Errorf("two: shared aliases = %q, want none (earlier merge source wins)", got)
	}
}

func TestRecursiveAliasesRejected(t *testing.T) {
	cases := map[string]string{
		"cycle": "x-loop: &loop [*loop]\nservices:\n  a:\n    image: x\n",
		"laughs": `x-a: &a [l, l, l, l, l, l, l, l, l, l]
x-b: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a, *a]
x-c: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b, *b]
x-d: &d [*c, *c, *c, *c, *c, *c, *c, *c, *c, *c]
x-e: &e [*d, *d, *d, *d, *d, *d, *d, *d, *d, *d]
x-f: &f [*e, *e, *e, *e, *e, *e, *e, *e, *e, *e]
x-g: &g [*f, *f, *f, *f, *f, *f, *f, *f, *f, *f]
services:
  a:
    image: x
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeCompose(t, body)
			if _, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", ""); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestNestedValuesInStringFieldsRejected(t *testing.T) {
	cases := map[string]string{
		"env_file": "env_file: [[a.env]]",
		"profiles": "profiles: {dev: true}",
		"image":    "image: {name: x}",
		"dns":      "dns: [[1.1.1.1]]",
		"command":  "command: [{a: b}]",
	}
	for key, line := range cases {
		t.Run(key, func(t *testing.T) {
			dir := writeCompose(t, "services:\n  a:\n    build: .\n    "+line+"\n")
			_, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", "")
			var ce *ComposeError
			if !errors.As(err, &ce) || !strings.Contains(err.Error(), "services.a."+key) {
				t.Errorf("expected ComposeError mentioning services.a.%s, got %v", key, err)
			}
		})
	}
}

func TestInterpolateTreeCopiesKeys(t *testing.T) {
	m := newMapping()
	m.Keys = make([]string, 0, 4)
	m.set("a", "1")
	out, err := InterpolateTree(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	out.(*Mapping).set("b", "2")
	if m.Keys[:2][1] == "b" {
		t.Error("appending to the copy wrote into the source keys")
	}
}

// Regression tests for the issues found in the third Opus/Astra review.

func TestRestartNoIsNotWarned(t *testing.T) {
	for _, value := range []string{`"no"`, `no`, `'"no"'`} {
		project, _ := loadTestProject(t, "services:\n  a:\n    image: x\n    restart: "+value+"\n")
		if len(project.Warnings) != 0 {
			t.Errorf("restart: %s: warnings = %q", value, project.Warnings)
		}
	}
	project, _ := loadTestProject(t, "services:\n  a:\n    image: x\n    restart: always\n")
	if len(project.Warnings) != 1 {
		t.Errorf("restart: always: warnings = %q, want one", project.Warnings)
	}
}

func TestTaggedYAML11Booleans(t *testing.T) {
	project, _ := loadTestProject(t, "services:\n  a:\n    image: x\n    tty: !!bool off\n    stdin_open: !!bool yes\n")
	if a := project.Services["a"]; a.TTY || !a.StdinOpen {
		t.Errorf("tty=%v stdin_open=%v, want false true", a.TTY, a.StdinOpen)
	}
}

func TestMergeSequenceThroughAnAlias(t *testing.T) {
	project, _ := loadTestProject(t, `
x-defaults: &defaults
  - {image: alpine, user: one}
  - {user: two, working_dir: /w}
services:
  web:
    <<: *defaults
`)
	web := project.Services["web"]
	if web.Image != "alpine" || web.User != "one" || web.WorkingDir != "/w" {
		t.Errorf("image=%q user=%q working_dir=%q", web.Image, web.User, web.WorkingDir)
	}

	dir := writeCompose(t, "x-s: &s\n  - {<<: *s}\nservices:\n  a:\n    image: x\n")
	if _, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", ""); err == nil {
		t.Error("recursive merge: expected an error")
	}
}

func TestNestedValuesDeeperInTheServiceRejected(t *testing.T) {
	cases := map[string]string{
		"labels.x":                    "labels: {x: {y: z}}",
		"networks.default.aliases[0]": "networks: {default: {aliases: [[a]]}}",
		"environment[0]":              "environment: [[A]]",
		"build.args.X":                "build: {context: ., args: {X: [1]}}",
		"ports[0].published":          "ports: [{target: 80, published: [8080]}]",
	}
	for where, line := range cases {
		t.Run(where, func(t *testing.T) {
			dir := writeCompose(t, "services:\n  a:\n    image: x\n    "+line+"\n")
			_, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", "")
			var ce *ComposeError
			if !errors.As(err, &ce) || !strings.Contains(err.Error(), "services.a."+where) {
				t.Errorf("expected ComposeError mentioning services.a.%s, got %v", where, err)
			}
		})
	}
}

// Regression tests for the issues found in the fourth Opus/Astra review.

func TestPortsOutOfRangeRejected(t *testing.T) {
	for _, spec := range []string{"9223372036854775807", "70000", "8080:70000", "1-9223372036854775807", "-1"} {
		if _, err := ParsePort(spec); err == nil {
			t.Errorf("%s: expected an error", spec)
		}
	}
}

func TestGPUsDeviceRequests(t *testing.T) {
	project, _ := loadTestProject(t, `
services:
  a:
    image: x
    gpus: [{driver: nvidia, count: 2}]
  b:
    image: x
    gpus: [{driver: nvidia}]
  c:
    image: x
    gpus: all
`)
	for name, want := range map[string]string{"a": "2", "b": "all", "c": "all"} {
		if got := project.Services[name].GPUs; got != want {
			t.Errorf("%s: gpus = %q, want %q", name, got, want)
		}
	}
}

func TestDotenvLongLines(t *testing.T) {
	dir := writeCompose(t, "services:\n  a:\n    image: ${IMG}\n")
	long := strings.Repeat("x", 100_000)
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("# "+long+"\nIMG="+long+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := project.Services["a"].Image; got != long {
		t.Errorf("image has %d characters, want %d", len(got), len(long))
	}
}

func TestListsWhereKeysOrStringsExpectedRejected(t *testing.T) {
	cases := map[string]string{
		"ulimits.nofile": "ulimits: {nofile: [1024, 2048]}",
		"depends_on[0]":  "depends_on: [{b: {}}]",
		"networks[0]":    "networks: [[a]]",
		"gpus[0]":        "gpus: [[1]]",
		"env_file[0]":    "env_file: [[a.env]]",
	}
	for where, line := range cases {
		t.Run(where, func(t *testing.T) {
			dir := writeCompose(t, "services:\n  a:\n    image: x\n    "+line+"\n  b:\n    image: x\n")
			_, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", "")
			var ce *ComposeError
			if !errors.As(err, &ce) || !strings.Contains(err.Error(), "services.a."+where) {
				t.Errorf("expected ComposeError mentioning services.a.%s, got %v", where, err)
			}
		})
	}
}

func TestEnvFileLongSyntax(t *testing.T) {
	dir := writeCompose(t, `
services:
  a:
    image: x
    env_file:
      - a.env
      - path: b.env
      - path: missing.env
        required: false
  b:
    image: x
    env_file: a.env
`)
	for _, name := range []string{"a.env", "b.env"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	project, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "a.env"), filepath.Join(dir, "b.env")}
	if got := project.Services["a"].EnvFiles; !reflect.DeepEqual(got, want) {
		t.Errorf("a: env files = %q, want %q", got, want)
	}
	if got := project.Services["b"].EnvFiles; !reflect.DeepEqual(got, want[:1]) {
		t.Errorf("b: env files = %q, want %q", got, want[:1])
	}
}

func TestNumbersKeepPythonText(t *testing.T) {
	project, _ := loadTestProject(t, `
services:
  a:
    image: x
    environment:
      EXP: 1e3
      OCT: 0o17
      ONE: 1.0
      HALF: 1.5
      SIGNED: 1.0e+3
      INT: 42
`)
	env := project.Services["a"].Environment
	for key, want := range map[string]string{"EXP": "1e3", "OCT": "0o17", "ONE": "1.0", "HALF": "1.5", "SIGNED": "1000.0", "INT": "42"} {
		if got := *env[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}
