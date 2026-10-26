package compose

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

const flagsCompose = `
name: demo
services:
  web:
    image: nginx:alpine
    ports: ["8088:80"]
    volumes:
      - ./html:/usr/share/nginx/html:ro
      - data:/data
    environment:
      FOO: bar
    entrypoint: ["/docker-entrypoint.sh", "nginx"]
    command: ["-g", "daemon off;"]
    networks:
      front:
        aliases: [www]
    mem_limit: 256M
    cpus: 0.5
    user: "101"
    working_dir: /srv
  job:
    build:
      context: ./src
      dockerfile: Dockerfile.dev
      args: {VERSION: "1.2"}
      target: dev
networks:
  front:
volumes:
  data:
`

func win(p string) string { return "WIN(" + p + ")" }

// after returns the argument following flag.
func after(t *testing.T, args []string, flag string) string {
	t.Helper()
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("%s not found in %q", flag, args)
	}
	return args[i+1]
}

func TestRunArgs(t *testing.T) {
	project, dir := loadTestProject(t, flagsCompose, "html", "src")
	args := RunArgs(project, project.Services["web"], 1, true, win)

	if args[0] != "run" || !slices.Contains(args, "-d") {
		t.Errorf("args = %q", args)
	}
	if got := after(t, args, "--name"); got != "demo-web-1" {
		t.Errorf("--name = %q", got)
	}
	for _, want := range []string{
		LabelProject + "=demo",
		"FOO=bar",
		"8088:80",
		win(filepath.Join(dir, "html")) + ":/usr/share/nginx/html:ro",
		"demo_data:/data",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("%q not in %q", want, args)
		}
	}

	if got := after(t, args, "--network"); got != "demo_front" {
		t.Errorf("--network = %q", got)
	}
	var aliases []string
	for i, a := range args {
		if a == "--network-alias" {
			aliases = append(aliases, args[i+1])
		}
	}
	slices.Sort(aliases)
	if !reflect.DeepEqual(aliases, []string{"web", "www"}) {
		t.Errorf("aliases = %q", aliases)
	}

	if got := after(t, args, "--entrypoint"); got != "/docker-entrypoint.sh" {
		t.Errorf("--entrypoint = %q", got)
	}
	// image followed by remaining entrypoint parts then command
	imageIdx := slices.Index(args, "nginx:alpine")
	if got := args[imageIdx+1:]; !reflect.DeepEqual(got, []string{"nginx", "-g", "daemon off;"}) {
		t.Errorf("tail = %q", got)
	}

	for flag, want := range map[string]string{"-m": "256M", "--cpus": "0.5", "-u": "101", "-w": "/srv"} {
		if got := after(t, args, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
}

func TestBuildArgs(t *testing.T) {
	project, dir := loadTestProject(t, flagsCompose, "html", "src")
	job := project.Services["job"]
	if got := ImageName(project, job); got != "demo-job" {
		t.Errorf("image name = %q", got)
	}

	args := BuildArgs(project, job, true, win)
	if !reflect.DeepEqual(args[:3], []string{"build", "-t", "demo-job"}) {
		t.Errorf("args = %q", args)
	}
	if got, want := after(t, args, "-f"), win(filepath.Join(dir, "src", "Dockerfile.dev")); got != want {
		t.Errorf("-f = %q, want %q", got, want)
	}
	if !slices.Contains(args, "VERSION=1.2") || !slices.Contains(args, "--no-cache") {
		t.Errorf("args = %q", args)
	}
	if got := after(t, args, "--target"); got != "dev" {
		t.Errorf("--target = %q", got)
	}
	if got, want := args[len(args)-1], win(filepath.Join(dir, "src")); got != want {
		t.Errorf("context = %q, want %q", got, want)
	}
}

func TestConfigHashTracksChanges(t *testing.T) {
	project, _ := loadTestProject(t, flagsCompose, "html", "src")
	web := project.Services["web"]
	before := web.ConfigHash()
	if before != web.ConfigHash() || len(before) != 16 {
		t.Fatalf("unstable hash %q", before)
	}
	web.Image = "nginx:latest"
	if web.ConfigHash() == before {
		t.Error("hash did not change")
	}
}
