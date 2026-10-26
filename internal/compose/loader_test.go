package compose

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const loaderCompose = `
name: demo
services:
  web:
    image: nginx:alpine
    ports:
      - "8088:80"
      - "127.0.0.1:9000:9000/udp"
      - target: 443
        published: 8443
    volumes:
      - ./html:/usr/share/nginx/html:ro
      - data:/data
    environment:
      - FOO=bar
      - NOVAL
    depends_on:
      - app
    networks:
      front:
        aliases: [www]
  app:
    image: alpine
    command: sh -c 'sleep 999'
    deploy:
      replicas: 2
      resources:
        limits:
          cpus: "0.5"
          memory: 256M
    networks: [front]
networks:
  front:
volumes:
  data:
`

func writeCompose(t *testing.T, content string, dirs ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadTestProject(t *testing.T, content string, dirs ...string) (*Project, string) {
	t.Helper()
	dir := writeCompose(t, content, dirs...)
	project, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	return project, dir
}

func TestLoadProject(t *testing.T) {
	project, dir := loadTestProject(t, loaderCompose, "html")

	t.Run("project name from compose", func(t *testing.T) {
		if project.Name != "demo" {
			t.Errorf("name = %q", project.Name)
		}
	})

	t.Run("networks and volumes prefixed", func(t *testing.T) {
		if got := project.Networks["front"].Name; got != "demo_front" {
			t.Errorf("network = %q", got)
		}
		if got := project.Volumes["data"].Name; got != "demo_data" {
			t.Errorf("volume = %q", got)
		}
	})

	t.Run("ports", func(t *testing.T) {
		var got []string
		for _, p := range project.Services["web"].Ports {
			got = append(got, p.Flag())
		}
		want := []string{"8088:80", "127.0.0.1:9000:9000/udp", "8443:443"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ports = %q, want %q", got, want)
		}
	})

	t.Run("volumes", func(t *testing.T) {
		vols := project.Services["web"].Volumes
		bind, named := vols[0], vols[1]
		if bind.Type != "bind" || bind.Source != filepath.Join(dir, "html") || !bind.ReadOnly {
			t.Errorf("bind = %+v", bind)
		}
		// resolved to project-scoped name
		if named.Type != "volume" || named.Source != "demo_data" {
			t.Errorf("named = %+v", named)
		}
	})

	t.Run("environment list form", func(t *testing.T) {
		env := project.Services["web"].Environment
		noval, hasNoval := env["NOVAL"]
		if len(env) != 2 || env["FOO"] == nil || *env["FOO"] != "bar" || !hasNoval || noval != nil {
			t.Errorf("environment = %v", env)
		}
	})

	t.Run("command string is split", func(t *testing.T) {
		if got := project.Services["app"].Command; !reflect.DeepEqual(got, []string{"sh", "-c", "sleep 999"}) {
			t.Errorf("command = %q", got)
		}
	})

	t.Run("deploy limits", func(t *testing.T) {
		app := project.Services["app"]
		if app.Replicas != 2 || app.CPUs != "0.5" || app.MemLimit != "256M" {
			t.Errorf("app = %+v", app)
		}
	})

	t.Run("network aliases", func(t *testing.T) {
		web := project.Services["web"]
		if !reflect.DeepEqual(web.Networks, []string{"demo_front"}) {
			t.Errorf("networks = %q", web.Networks)
		}
		if !reflect.DeepEqual(web.NetworkAliases["demo_front"], []string{"www"}) {
			t.Errorf("aliases = %q", web.NetworkAliases)
		}
	})

	t.Run("dependency order", func(t *testing.T) {
		order, err := project.SortedServices(nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(order) != 2 || order[0].Name != "app" || order[1].Name != "web" {
			t.Errorf("order = %v", order)
		}
	})

	t.Run("container names", func(t *testing.T) {
		if got := project.ContainerName(project.Services["web"], 1); got != "demo-web-1" {
			t.Errorf("container name = %q", got)
		}
	})
}

func TestCircularDependency(t *testing.T) {
	project, _ := loadTestProject(t, `
services:
  a:
    image: x
    depends_on: [b]
  b:
    image: x
    depends_on: [a]
`)
	if _, err := project.SortedServices(nil); err == nil || !strings.Contains(err.Error(), "circular") {
		t.Errorf("expected circular error, got %v", err)
	}
}

func TestUndefinedNetworkRejected(t *testing.T) {
	dir := writeCompose(t, `
services:
  a:
    image: x
    networks: [nope]
`)
	_, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", "")
	var ce *ComposeError
	if !errors.As(err, &ce) || !strings.Contains(err.Error(), "undefined network") {
		t.Errorf("expected undefined network error, got %v", err)
	}
}

func TestWindowsPathVolume(t *testing.T) {
	project, _ := loadTestProject(t, `
services:
  a:
    image: x
    volumes:
      - E:\data\www:/srv:ro
`)
	mount := project.Services["a"].Volumes[0]
	if mount.Type != "bind" || mount.Source != `E:\data\www` || mount.Target != "/srv" || !mount.ReadOnly {
		t.Errorf("mount = %+v", mount)
	}
}

func TestPortRange(t *testing.T) {
	ports, err := ParsePort("8000-8002:9000-9002")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range ports {
		got = append(got, p.Flag())
	}
	if want := []string{"8000:9000", "8001:9001", "8002:9002"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestUnsupportedKeyWarns(t *testing.T) {
	project, _ := loadTestProject(t, `
services:
  a:
    image: x
    privileged: true
    restart: always
`)
	text := strings.Join(project.Warnings, "\n")
	if !strings.Contains(text, "privileged") || !strings.Contains(text, "restart") {
		t.Errorf("warnings = %q", text)
	}
}

func TestProjectNameFromDirectory(t *testing.T) {
	project, dir := loadTestProject(t, "services:\n  a:\n    image: x\n")
	want, _ := NormalizeProjectName(filepath.Base(dir))
	if project.Name != want {
		t.Errorf("name = %q, want %q", project.Name, want)
	}
	if _, ok := project.Networks["default"]; !ok {
		t.Error("default network not created")
	}
}

func TestDotenvInterpolation(t *testing.T) {
	dir := writeCompose(t, "services:\n  a:\n    image: ${IMG:-none}\n")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("# comment\nexport IMG=\"redis:7\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	project, err := LoadProject(filepath.Join(dir, "compose.yaml"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := project.Services["a"].Image; got != "redis:7" {
		t.Errorf("image = %q", got)
	}
}
