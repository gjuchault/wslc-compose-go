// Package cli implements the wslc-compose command line interface.
package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"wslc-go/internal/compose"
	"wslc-go/internal/engine"
)

// Version is overridable at build time with -ldflags "-X wslc-go/internal/cli.Version=...".
var Version = "0.2.1"

var protocols = map[string]string{"6": "tcp", "17": "udp"}

func errorMsg(message string) { fmt.Fprintf(os.Stderr, "wslc-compose: %s\n", message) }

func info(message string) { fmt.Println(message) }

func timeoutFlag(fs *flagSet) *int {
	return fs.decimalIntP("timeout", "t", 10)
}

// --- project loading -------------------------------------------------------

type globals struct {
	file        string
	projectName string
	envFile     string
	dryRun      bool
	profiles    []string
}

func locateComposeFile(explicit string) (string, error) {
	if explicit != "" {
		if info, err := os.Stat(explicit); err != nil || info.IsDir() {
			return "", &compose.ComposeError{Msg: "compose file not found: " + explicit}
		}
		return explicit, nil
	}
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if found := compose.FindComposeFile(directory); found != "" {
			return found, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", &compose.ComposeError{Msg: "no compose file found (looked for compose.yaml / compose.yml / " +
				"docker-compose.yml / docker-compose.yaml in this directory and parents)"}
		}
		directory = parent
	}
}

func (g *globals) load() (*compose.Project, error) {
	composeFile, err := locateComposeFile(g.file)
	if err != nil {
		return nil, err
	}
	project, err := compose.LoadProject(composeFile, g.projectName, g.envFile)
	if err != nil {
		return nil, err
	}
	for _, warning := range project.Warnings {
		errorMsg("warning: " + warning)
	}
	return project, nil
}

// selectServices returns the services to act on, in dependency order: the
// named ones (or every enabled one) plus their dependencies. A service is
// enabled when it has no profiles, one of its profiles is active, or it is
// named explicitly; depending on a disabled service is an error.
func selectServices(project *compose.Project, names, profiles []string) ([]*compose.Service, error) {
	for _, name := range names {
		if _, ok := project.Services[name]; !ok {
			return nil, &compose.ComposeError{Msg: "no such service: " + name}
		}
	}
	enabled := func(s *compose.Service) bool {
		return len(s.Profiles) == 0 || intersects(s.Profiles, profiles) || contains(names, s.Name)
	}
	roots := names
	if len(roots) == 0 {
		for _, name := range sortedKeys(project.Services) {
			if enabled(project.Services[name]) {
				roots = append(roots, name)
			}
		}
		if len(roots) == 0 {
			return nil, nil
		}
	}
	services, err := project.SortedServices(roots)
	if err != nil {
		return nil, err
	}
	for _, svc := range services {
		for _, name := range svc.DependsOn {
			dep, ok := project.Services[name]
			if !ok {
				return nil, &compose.ComposeError{Msg: fmt.Sprintf("service %q depends on undefined service %q", svc.Name, name)}
			}
			if !enabled(dep) {
				return nil, &compose.ComposeError{Msg: fmt.Sprintf(
					"service %q is required by %q but is disabled; enable it with --profile %s",
					name, svc.Name, strings.Join(dep.Profiles, " or --profile "))}
			}
		}
	}
	return services, nil
}

func contains(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func intersects(a, b []string) bool {
	for _, item := range a {
		if contains(b, item) {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- container queries ------------------------------------------------------

type container struct {
	ID      string
	Name    string
	Image   string
	Service string
	Index   int
	Hash    string
	Running bool
	Status  string
	Ports   any // a list of port objects, or the text wslc 3 prints
}

// str renders a JSON scalar as a string ("" for null).
func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(x)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// projectContainers lists the project's containers enriched with inspect
// data. When strict, it fails rather than guess when a container of one of
// services (all when empty) cannot be inspected: an unknown state must never
// be mistaken for a stale (up) or a stopped (stop, restart) container.
// Otherwise it warns and lists the container with an unknown state, so that
// down can still remove it.
func projectContainers(project *compose.Project, strict bool, services []string) ([]container, error) {
	entries, err := engine.ListProjectContainers(project.Name)
	if err != nil {
		return nil, err
	}
	var result []container
	for _, entry := range entries {
		// wslc 3 uses docker's keys: ID, Names, and Ports as a string
		id := firstNonEmpty(str(entry["Id"]), str(entry["ID"]))
		entryName := firstNonEmpty(str(entry["Name"]), str(entry["Names"]))
		ref := firstNonEmpty(id, entryName)
		data, err := engine.Inspect(ref)
		if err != nil {
			err = fmt.Errorf("cannot inspect container %s: %w", ref, err)
			if strict && belongsTo(project, entryName, services) {
				return nil, err
			}
			if err := warnUnlessInterrupted(err, "warning: "+err.Error()); err != nil {
				return nil, err
			}
			data = map[string]any{}
		}
		labels, _ := data["Labels"].(map[string]any)
		state, _ := data["State"].(map[string]any)
		index, err := strconv.Atoi(str(labels[compose.LabelIndex]))
		if err != nil || index == 0 {
			index = 1
		}
		status := "unknown"
		if s, ok := state["Status"]; ok {
			status = str(s)
		}
		running, _ := state["Running"].(bool)
		ports := entry["Ports"]
		result = append(result, container{
			ID:      id,
			Name:    firstNonEmpty(entryName, strings.TrimPrefix(str(data["Name"]), "/"), id),
			Image:   firstNonEmpty(str(entry["Image"]), str(data["Image"])),
			Service: str(labels[compose.LabelService]),
			Index:   index,
			Hash:    str(labels[compose.LabelConfigHash]),
			Running: running,
			Status:  status,
			Ports:   ports,
		})
	}
	return result, nil
}

// belongsTo reports whether the container named name may belong to one of
// services (any service when empty, or when the name is unknown).
func belongsTo(project *compose.Project, name string, services []string) bool {
	if len(services) == 0 || name == "" {
		return true
	}
	for _, svcName := range services {
		svc, ok := project.Services[svcName]
		if !ok {
			continue
		}
		if svc.ContainerName != "" {
			if name == svc.ContainerName {
				return true
			}
			continue
		}
		rest, found := strings.CutPrefix(name, project.Name+"-"+svc.Name+"-")
		if _, err := strconv.Atoi(rest); found && err == nil {
			return true
		}
	}
	return false
}

func filterContainers(containers []container, services []string) []container {
	if len(services) == 0 {
		return containers
	}
	var out []container
	for _, c := range containers {
		if contains(services, c.Service) {
			out = append(out, c)
		}
	}
	return out
}

func formatPorts(v any) string {
	if text, ok := v.(string); ok {
		return text
	}
	ports, _ := v.([]any)
	var parts []string
	for _, p := range ports {
		port, _ := p.(map[string]any)
		proto := str(port["Protocol"])
		if name, ok := protocols[proto]; ok {
			proto = name
		}
		addr := firstNonEmpty(str(port["BindingAddress"]), "0.0.0.0")
		parts = append(parts, fmt.Sprintf("%s:%s->%s/%s", addr, str(port["HostPort"]), str(port["ContainerPort"]), proto))
	}
	return strings.Join(parts, ", ")
}

func printTable(rows [][]string, headers []string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			widths[i] = max(widths[i], len(cell))
		}
	}
	printRow := func(row []string) {
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = fmt.Sprintf("%-*s", widths[i], cell)
		}
		fmt.Println(strings.Join(cells, "  "))
	}
	printRow(headers)
	for _, row := range rows {
		printRow(row)
	}
}

// warnUnlessInterrupted reports a non-fatal failure, but lets an interrupt
// abort the command.
func warnUnlessInterrupted(err error, message string) error {
	if errors.Is(err, engine.ErrInterrupted) {
		return err
	}
	errorMsg(message)
	return nil
}

// --- commands ----------------------------------------------------------------

const upUsage = `usage: wslc-compose up [-d] [--build] [--force-recreate] [--scale SERVICE=N] [-t TIMEOUT] [SERVICE...]

Create and start services.

  -d, --detach          run containers in the background
      --build           rebuild images before starting
      --force-recreate  recreate containers even if their config is unchanged
      --scale SERVICE=N scale a service to N containers (repeatable)
  -t, --timeout N       stop timeout in seconds (default 10)
`

func cmdUp(g *globals, args []string) (int, error) {
	fs := newFlagSet("up", upUsage)
	detach := fs.BoolP("detach", "d", false, "")
	build := fs.Bool("build", false, "")
	forceRecreate := fs.Bool("force-recreate", false, "")
	scaleSpecs := fs.StringArray("scale", nil, "")
	timeout := timeoutFlag(fs)
	names, err := fs.parse(args)
	if err != nil {
		return 0, err
	}

	project, err := g.load()
	if err != nil {
		return 1, err
	}
	services, err := selectServices(project, names, g.profiles)
	if err != nil {
		return 1, err
	}
	if len(services) == 0 {
		errorMsg("no services to start")
		return 1, nil
	}

	scale := map[string]int{}
	for _, spec := range *scaleSpecs {
		name, count, _ := strings.Cut(spec, "=")
		n, err := strconv.Atoi(count)
		if err != nil || n < 0 || strings.HasPrefix(count, "+") {
			return 1, &compose.ComposeError{Msg: fmt.Sprintf("invalid --scale value: %q (expected service=N)", spec)}
		}
		scale[name] = n
	}

	neededNetworks := map[string]bool{}
	neededVolumes := map[string]bool{}
	for _, svc := range services {
		for _, n := range svc.Networks {
			neededNetworks[n] = true
		}
		for _, m := range svc.Volumes {
			if m.Type == "volume" {
				neededVolumes[m.Source] = true
			}
		}
	}

	for _, key := range sortedKeys(project.Networks) {
		net := project.Networks[key]
		if !neededNetworks[net.Name] {
			continue
		}
		if net.External {
			existing, err := engine.NetworkNames()
			if err != nil {
				return 1, err
			}
			if !contains(existing, net.Name) {
				return 1, &compose.ComposeError{Msg: fmt.Sprintf("external network %q not found", net.Name)}
			}
		} else if created, err := engine.EnsureNetwork(net.Name, g.dryRun); err != nil {
			return 1, err
		} else if created {
			info("Network " + net.Name + " created")
		}
	}

	for _, key := range sortedKeys(project.Volumes) {
		vol := project.Volumes[key]
		if !neededVolumes[vol.Name] {
			continue
		}
		if vol.External {
			existing, err := engine.VolumeNames()
			if err != nil {
				return 1, err
			}
			if !contains(existing, vol.Name) {
				return 1, &compose.ComposeError{Msg: fmt.Sprintf("external volume %q not found", vol.Name)}
			}
		} else if created, err := engine.EnsureVolume(vol.Name, g.dryRun); err != nil {
			return 1, err
		} else if created {
			info("Volume " + vol.Name + " created")
		}
	}

	for _, svc := range services {
		// build when explicitly asked, or when the target image is absent
		if svc.Build != nil && (*build || !engine.ImageExists(compose.ImageName(project, svc))) {
			info("Building " + svc.Name + " ...")
			if _, err := engine.Run(compose.BuildArgs(project, svc, false, engine.ToHostPath), engine.Opts{DryRun: g.dryRun}); err != nil {
				return 1, err
			}
		}
	}

	existing := map[string]container{}
	if !g.dryRun {
		var selected []string
		for _, svc := range services {
			selected = append(selected, svc.Name)
		}
		containers, err := projectContainers(project, true, selected)
		if err != nil {
			return 1, err
		}
		for _, c := range containers {
			existing[c.Name] = c
		}
	}
	quiet := engine.Opts{Capture: true, DryRun: g.dryRun}
	var started []string

	for _, svc := range services {
		replicas, ok := scale[svc.Name]
		if !ok {
			replicas = svc.Replicas
		}
		if svc.ContainerName != "" && replicas > 1 {
			return 1, &compose.ComposeError{Msg: svc.Name + ": cannot scale a service with container_name set"}
		}
		desiredHash := svc.ConfigHash()
		for index := 1; index <= replicas; index++ {
			cname := project.ContainerName(svc, index)
			current, found := existing[cname]
			delete(existing, cname)
			if found {
				fresh := current.Hash == desiredHash && !*forceRecreate
				if fresh && current.Running {
					info("Container " + cname + " is up-to-date")
					continue
				}
				if fresh {
					info("Starting " + cname + " ...")
					if _, err := engine.RunRetried([]string{"start", cname}, g.dryRun); err != nil {
						return 1, err
					}
					started = append(started, cname)
					continue
				}
				info("Recreating " + cname + " ...")
				if current.Running {
					if _, err := engine.Run([]string{"stop", "-t", strconv.Itoa(*timeout), cname}, quiet); err != nil {
						return 1, err
					}
				}
				if _, err := engine.Run([]string{"remove", "-f", cname}, quiet); err != nil {
					return 1, err
				}
			} else {
				info("Creating " + cname + " ...")
			}
			if _, err := engine.Run(compose.RunArgs(project, svc, index, true, engine.ToHostPath), engine.Opts{DryRun: g.dryRun}); err != nil {
				return 1, err
			}
			started = append(started, cname)
		}

		// drop replicas beyond the requested scale
		for _, name := range sortedKeys(existing) {
			leftover := existing[name]
			if leftover.Service == svc.Name && leftover.Index > replicas {
				info("Removing surplus " + name + " ...")
				if _, err := engine.Run([]string{"remove", "-f", name}, quiet); err != nil {
					return 1, err
				}
				delete(existing, name)
			}
		}
	}

	if *detach || g.dryRun || len(started) == 0 {
		return 0, nil
	}
	info("Attaching to logs (Ctrl+C to detach; containers keep running)")
	serviceNames := make([]string, len(services))
	for i, s := range services {
		serviceNames[i] = s.Name
	}
	return followLogs(project, serviceNames, logOptions{follow: true, tail: -1})
}

const downUsage = `usage: wslc-compose down [-v] [-t TIMEOUT]

Stop and remove project containers and networks.

  -v, --volumes    also remove named volumes
  -t, --timeout N  stop timeout in seconds (default 10)
`

func cmdDown(g *globals, args []string) (int, error) {
	fs := newFlagSet("down", downUsage)
	volumes := fs.BoolP("volumes", "v", false, "")
	timeout := timeoutFlag(fs)
	if err := fs.parseNoArgs(args); err != nil {
		return 0, err
	}

	project, err := g.load()
	if err != nil {
		return 1, err
	}
	containers, err := projectContainers(project, false, nil)
	if err != nil {
		return 1, err
	}
	quiet := engine.Opts{Capture: true, DryRun: g.dryRun}
	for _, c := range containers {
		if c.Running {
			info("Stopping " + c.Name + " ...")
			if _, err := engine.Run([]string{"stop", "-t", strconv.Itoa(*timeout), c.Name}, quiet); err != nil {
				return 1, err
			}
		}
		info("Removing " + c.Name + " ...")
		if _, err := engine.Run([]string{"remove", "-f", c.Name}, quiet); err != nil {
			return 1, err
		}
	}

	existingNetworks, err := engine.NetworkNames()
	if err != nil {
		return 1, err
	}
	for _, key := range sortedKeys(project.Networks) {
		net := project.Networks[key]
		if !net.External && contains(existingNetworks, net.Name) {
			info("Removing network " + net.Name)
			if _, err := engine.Run([]string{"network", "remove", net.Name}, quiet); err != nil {
				if err := warnUnlessInterrupted(err, fmt.Sprintf("warning: could not remove network %s (still in use?)", net.Name)); err != nil {
					return 1, err
				}
			}
		}
	}

	if *volumes {
		existingVolumes, err := engine.VolumeNames()
		if err != nil {
			return 1, err
		}
		for _, key := range sortedKeys(project.Volumes) {
			vol := project.Volumes[key]
			if !vol.External && contains(existingVolumes, vol.Name) {
				info("Removing volume " + vol.Name)
				if _, err := engine.Run([]string{"volume", "remove", vol.Name}, quiet); err != nil {
					if err := warnUnlessInterrupted(err, "warning: could not remove volume "+vol.Name); err != nil {
						return 1, err
					}
				}
			}
		}
	}
	return 0, nil
}

const psUsage = `usage: wslc-compose ps [-q] [SERVICE...]

List project containers.

  -q, --quiet  only display container IDs
`

func cmdPs(g *globals, args []string) (int, error) {
	fs := newFlagSet("ps", psUsage)
	quiet := fs.BoolP("quiet", "q", false, "")
	names, err := fs.parse(args)
	if err != nil {
		return 0, err
	}
	project, err := g.load()
	if err != nil {
		return 1, err
	}
	containers, err := projectContainers(project, false, nil)
	if err != nil {
		return 1, err
	}
	containers = filterContainers(containers, names)
	if *quiet {
		for _, c := range containers {
			fmt.Println(c.ID)
		}
		return 0, nil
	}
	sort.SliceStable(containers, func(i, j int) bool {
		if containers[i].Service != containers[j].Service {
			return containers[i].Service < containers[j].Service
		}
		return containers[i].Index < containers[j].Index
	})
	var rows [][]string
	for _, c := range containers {
		rows = append(rows, []string{c.Name, c.Service, c.Image, c.Status, formatPorts(c.Ports)})
	}
	printTable(rows, []string{"NAME", "SERVICE", "IMAGE", "STATUS", "PORTS"})
	return 0, nil
}

const logsUsage = `usage: wslc-compose logs [-f] [-n TAIL] [-t] [SERVICE...]

Show container logs.

  -f, --follow      follow log output
  -n, --tail N      number of lines to show from the end of the logs
  -t, --timestamps  show timestamps
`

func cmdLogs(g *globals, args []string) (int, error) {
	fs := newFlagSet("logs", logsUsage)
	follow := fs.BoolP("follow", "f", false, "")
	tail := fs.decimalIntP("tail", "n", -1)
	timestamps := fs.BoolP("timestamps", "t", false, "")
	names, err := fs.parse(args)
	if err != nil {
		return 0, err
	}
	project, err := g.load()
	if err != nil {
		return 1, err
	}
	return followLogs(project, names, logOptions{follow: *follow, tail: *tail, timestamps: *timestamps})
}

const execUsage = `usage: wslc-compose exec [--index N] [-u USER] [-w DIR] [-e KEY=VAL] [-T] SERVICE COMMAND [ARG...]

Run a command in a service container.

      --index N    index of the container if the service has replicas (default 1)
  -u, --user USER  run as this user
  -w, --workdir    working directory inside the container
  -e, --env K=V    set an environment variable (repeatable)
  -T, --no-tty     disable pseudo-TTY allocation
`

func cmdExec(g *globals, args []string) (int, error) {
	fs := newFlagSet("exec", execUsage)
	index := fs.decimalIntP("index", "", 1)
	user := fs.StringP("user", "u", "", "")
	workdir := fs.StringP("workdir", "w", "", "")
	env := fs.StringArrayP("env", "e", nil, "")
	noTTY := fs.BoolP("no-tty", "T", false, "")
	fs.stopAtFirstArg() // everything after the service name is the command
	positional, err := fs.parse(args)
	if err != nil {
		return 0, err
	}
	if len(positional) == 0 {
		return 0, fs.fail(errors.New("the following arguments are required: SERVICE"))
	}
	serviceName, command := positional[0], positional[1:]

	project, err := g.load()
	if err != nil {
		return 1, err
	}
	svc, ok := project.Services[serviceName]
	if !ok {
		return 1, &compose.ComposeError{Msg: "no such service: " + serviceName}
	}
	execArgs := []string{"exec"}
	if !*noTTY && isTerminal(os.Stdin) {
		execArgs = append(execArgs, "-i", "-t")
	}
	if *user != "" {
		execArgs = append(execArgs, "-u", *user)
	}
	if *workdir != "" {
		execArgs = append(execArgs, "-w", *workdir)
	}
	for _, e := range *env {
		execArgs = append(execArgs, "-e", e)
	}
	execArgs = append(execArgs, project.ContainerName(svc, *index))
	execArgs = append(execArgs, command...)
	return engine.RunInteractive(execArgs, g.dryRun)
}

func isTerminal(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }

func lifecycleUsage(action string) string {
	return fmt.Sprintf(`usage: wslc-compose %[1]s [-t TIMEOUT] [SERVICE...]

%[2]s project containers.

  -t, --timeout N  stop timeout in seconds (default 10)
`, action, strings.ToUpper(action[:1])+action[1:])
}

func lifecycle(action string) func(*globals, []string) (int, error) {
	return func(g *globals, args []string) (int, error) {
		fs := newFlagSet(action, lifecycleUsage(action))
		timeout := timeoutFlag(fs)
		names, err := fs.parse(args)
		if err != nil {
			return 0, err
		}
		project, err := g.load()
		if err != nil {
			return 1, err
		}
		all, err := projectContainers(project, true, names)
		if err != nil {
			return 1, err
		}
		containers := filterContainers(all, names)
		if len(containers) == 0 {
			errorMsg("no containers found")
			return 1, nil
		}
		for _, c := range containers {
			if (action == "stop" || action == "restart") && c.Running {
				info("Stopping " + c.Name + " ...")
				if _, err := engine.Run([]string{"stop", "-t", strconv.Itoa(*timeout), c.Name}, engine.Opts{Capture: true, DryRun: g.dryRun}); err != nil {
					return 1, err
				}
			}
			if action == "start" || action == "restart" {
				info("Starting " + c.Name + " ...")
				if _, err := engine.RunRetried([]string{"start", c.Name}, g.dryRun); err != nil {
					return 1, err
				}
			}
		}
		return 0, nil
	}
}

const pullUsage = `usage: wslc-compose pull [SERVICE...]

Pull service images.
`

func cmdPull(g *globals, args []string) (int, error) {
	names, err := newFlagSet("pull", pullUsage).parse(args)
	if err != nil {
		return 0, err
	}
	project, err := g.load()
	if err != nil {
		return 1, err
	}
	services, err := selectServices(project, names, g.profiles)
	if err != nil {
		return 1, err
	}
	for _, svc := range services {
		if svc.Image != "" {
			info("Pulling " + svc.Image + " ...")
			if _, err := engine.Run([]string{"pull", svc.Image}, engine.Opts{DryRun: g.dryRun}); err != nil {
				return 1, err
			}
		}
	}
	return 0, nil
}

const buildUsage = `usage: wslc-compose build [--no-cache] [SERVICE...]

Build service images.

      --no-cache  do not use cache when building the image
`

func cmdBuild(g *globals, args []string) (int, error) {
	fs := newFlagSet("build", buildUsage)
	noCache := fs.Bool("no-cache", false, "")
	names, err := fs.parse(args)
	if err != nil {
		return 0, err
	}
	project, err := g.load()
	if err != nil {
		return 1, err
	}
	services, err := selectServices(project, names, g.profiles)
	if err != nil {
		return 1, err
	}
	built := 0
	for _, svc := range services {
		if svc.Build == nil {
			continue
		}
		info("Building " + svc.Name + " ...")
		if _, err := engine.Run(compose.BuildArgs(project, svc, *noCache, engine.ToHostPath), engine.Opts{DryRun: g.dryRun}); err != nil {
			return 1, err
		}
		built++
	}
	if built == 0 {
		errorMsg("no services with a build section")
		return 1, nil
	}
	return 0, nil
}

func cmdConfig(g *globals, args []string) (int, error) {
	if err := newFlagSet("config", "usage: wslc-compose config\n\nPrint the resolved configuration.\n").parseNoArgs(args); err != nil {
		return 0, err
	}
	project, err := g.load()
	if err != nil {
		return 1, err
	}
	out, err := yaml.Marshal(project)
	if err != nil {
		return 1, err
	}
	os.Stdout.Write(out)
	return 0, nil
}

func cmdVersion(g *globals, args []string) (int, error) {
	if err := newFlagSet("version", "usage: wslc-compose version\n\nShow version information.\n").parseNoArgs(args); err != nil {
		return 0, err
	}
	fmt.Println("wslc-compose " + Version)
	if _, err := engine.Run([]string{"version"}, engine.Opts{NoCheck: true}); err != nil {
		errorMsg(err.Error())
	}
	return 0, nil
}

// --- entry point ---------------------------------------------------------------

type command struct {
	name string
	help string
	run  func(*globals, []string) (int, error)
}

var commands = []command{
	{"up", "create and start services", cmdUp},
	{"down", "stop and remove project containers and networks", cmdDown},
	{"ps", "list project containers", cmdPs},
	{"logs", "show container logs", cmdLogs},
	{"exec", "run a command in a service container", cmdExec},
	{"start", "start project containers", lifecycle("start")},
	{"stop", "stop project containers", lifecycle("stop")},
	{"restart", "restart project containers", lifecycle("restart")},
	{"pull", "pull service images", cmdPull},
	{"build", "build service images", cmdBuild},
	{"config", "print the resolved configuration", cmdConfig},
	{"version", "show version information", cmdVersion},
}

func mainUsage() string {
	var b strings.Builder
	b.WriteString(`usage: wslc-compose [-f FILE] [-p NAME] [--env-file PATH] [--dry-run] [--profile NAME] COMMAND ...

docker-compose style orchestration for WSL containers (wslc)

options:
  -f, --file FILE          compose file (default: auto-detect)
  -p, --project-name NAME  project name (default: directory name)
      --env-file PATH      alternate .env file
      --dry-run            print wslc commands instead of running them
      --profile NAME       enable a compose profile (repeatable)
      --version            show program's version number and exit

commands:
`)
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-9s %s\n", c.name, c.help)
	}
	return b.String()
}

// Main runs wslc-compose with argv (without the program name) and returns the
// process exit code.
func Main(argv []string) int {
	engine.HandleInterrupts()
	g := &globals{}
	fs := newFlagSet("wslc-compose", mainUsage())
	fs.StringVarP(&g.file, "file", "f", "", "")
	fs.StringVarP(&g.projectName, "project-name", "p", "", "")
	fs.StringVar(&g.envFile, "env-file", "", "")
	fs.BoolVar(&g.dryRun, "dry-run", false, "")
	fs.StringArrayVar(&g.profiles, "profile", nil, "")
	version := fs.Bool("version", false, "")
	fs.stopAtFirstArg()

	positional, err := fs.parse(argv)
	if err == nil && *version {
		fmt.Println("wslc-compose " + Version)
		return 0
	}
	if err == nil && len(positional) == 0 {
		err = fs.fail(errors.New("the following arguments are required: COMMAND"))
	}
	if err != nil {
		return exitCode(1, err)
	}

	name, rest := positional[0], positional[1:]
	for _, c := range commands {
		if c.name == name {
			return exitCode(c.run(g, rest))
		}
	}
	return exitCode(1, fs.fail(fmt.Errorf("invalid command: %q", name)))
}

func exitCode(code int, err error) int {
	var exit *exitError
	switch {
	case err == nil:
		return code
	case errors.As(err, &exit):
		return exit.code
	case errors.Is(err, engine.ErrInterrupted):
		return 130
	}
	errorMsg(err.Error())
	return 1
}
