package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"wslc-go/internal/compose"
	"wslc-go/internal/engine"
)

const upUsage = `usage: wslc-compose up [-d] [--build] [--force-recreate] [--remove-orphans] [--scale SERVICE=N] [-t TIMEOUT] [SERVICE...]

Create and start services.

  -d, --detach          run containers in the background
      --build           rebuild images before starting
      --force-recreate  recreate containers even if their config is unchanged
      --remove-orphans  remove containers of services no longer in the compose file
                        (also enabled by COMPOSE_REMOVE_ORPHANS=1)
      --scale SERVICE=N scale a service to N containers (repeatable)
  -t, --timeout N       stop timeout in seconds (default 10)
`

func cmdUp(g *globals, args []string) (int, error) {
	fs := newFlagSet("up", upUsage)
	detach := fs.BoolP("detach", "d", false, "")
	build := fs.Bool("build", false, "")
	forceRecreate := fs.Bool("force-recreate", false, "")
	removeOrphans := fs.Bool("remove-orphans", false, "")
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

	if err := g.ensureResources(project, services); err != nil {
		return 1, err
	}
	if err := g.buildImages(project, services, *build); err != nil {
		return 1, err
	}
	containers, err := g.listContainers(project, services)
	if err != nil {
		return 1, err
	}
	if err := g.handleOrphans(project, containers, *removeOrphans || envFlag("COMPOSE_REMOVE_ORPHANS"), *timeout); err != nil {
		return 1, err
	}
	started, err := g.converge(project, services, containers, convergeOptions{
		forceRecreate: *forceRecreate,
		scale:         scale,
		timeout:       *timeout,
	})
	if err != nil {
		return 1, err
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

// envFlag reports whether the environment variable is set to a true value.
func envFlag(name string) bool {
	b, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && b
}

// ensureResources creates the networks and volumes the services use, and
// checks that the external ones exist.
func (g *globals) ensureResources(project *compose.Project, services []*compose.Service) error {
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
				return err
			}
			if !contains(existing, net.Name) {
				return &compose.ComposeError{Msg: fmt.Sprintf("external network %q not found", net.Name)}
			}
		} else if created, err := engine.EnsureNetwork(net.Name, g.dryRun); err != nil {
			return err
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
				return err
			}
			if !contains(existing, vol.Name) {
				return &compose.ComposeError{Msg: fmt.Sprintf("external volume %q not found", vol.Name)}
			}
		} else if created, err := engine.EnsureVolume(vol.Name, g.dryRun); err != nil {
			return err
		} else if created {
			info("Volume " + vol.Name + " created")
		}
	}
	return nil
}

// buildImages builds the images of services with a build section, when force
// is set or when the image is absent.
func (g *globals) buildImages(project *compose.Project, services []*compose.Service, force bool) error {
	for _, svc := range services {
		if svc.Build != nil && (force || !engine.ImageExists(compose.ImageName(project, svc))) {
			info("Building " + svc.Name + " ...")
			if _, err := engine.Run(compose.BuildArgs(project, svc, false, engine.ToHostPath), engine.Opts{DryRun: g.dryRun}); err != nil {
				return err
			}
		}
	}
	return nil
}

// listContainers lists the project's containers, strict about the given
// services (see projectContainers). It lists none in dry-run mode.
func (g *globals) listContainers(project *compose.Project, services []*compose.Service) ([]container, error) {
	if g.dryRun {
		return nil, nil
	}
	selected := make([]string, len(services))
	for i, svc := range services {
		selected[i] = svc.Name
	}
	return projectContainers(project, true, selected)
}

// handleOrphans warns about the containers of services that are no longer in
// the compose file, or removes them. Services disabled by a profile are still
// in the file, so their containers are not orphans.
func (g *globals) handleOrphans(project *compose.Project, containers []container, remove bool, timeout int) error {
	var orphans []container
	for _, c := range containers {
		if !c.OneOff && c.Service != "" && project.Services[c.Service] == nil {
			orphans = append(orphans, c)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	if !remove {
		names := make([]string, len(orphans))
		for i, c := range orphans {
			names[i] = c.Name
		}
		errorMsg(fmt.Sprintf("warning: found orphan containers (%s) for this project. If you removed or renamed "+
			"this service in your compose file, you can run this command with the --remove-orphans flag to clean it up.",
			strings.Join(names, ", ")))
		return nil
	}
	quiet := engine.Opts{Capture: true, DryRun: g.dryRun}
	for _, c := range orphans {
		if c.Running {
			info("Stopping orphan " + c.Name + " ...")
			if _, err := engine.Run([]string{"stop", "-t", strconv.Itoa(timeout), c.Name}, quiet); err != nil {
				return err
			}
		}
		info("Removing orphan " + c.Name + " ...")
		if _, err := engine.Run([]string{"remove", "-f", c.Name}, quiet); err != nil {
			return err
		}
	}
	return nil
}

type convergeOptions struct {
	forceRecreate bool
	scale         map[string]int // replicas by service, overriding the compose file
	timeout       int
}

func (o convergeOptions) replicas(svc *compose.Service) int {
	if n, ok := o.scale[svc.Name]; ok {
		return n
	}
	return svc.Replicas
}

// converge creates, recreates or starts the containers of services (in
// dependency order) to match the compose file, and returns those it started.
func (g *globals) converge(project *compose.Project, services []*compose.Service, containers []container, o convergeOptions) ([]string, error) {
	existing := map[string]container{}
	for _, c := range containers {
		if !c.OneOff {
			existing[c.Name] = c
		}
	}
	quiet := engine.Opts{Capture: true, DryRun: g.dryRun}
	var started []string

	for _, svc := range services {
		replicas := o.replicas(svc)
		if svc.ContainerName != "" && replicas > 1 {
			return nil, &compose.ComposeError{Msg: svc.Name + ": cannot scale a service with container_name set"}
		}
		// wait for the dependencies only when a container is to be started
		waited := false
		waitDeps := func() error {
			if waited {
				return nil
			}
			waited = true
			return waitForDependencies(project, svc, o.replicas, g.dryRun)
		}
		desiredHash := svc.ConfigHash()
		for index := 1; index <= replicas; index++ {
			cname := project.ContainerName(svc, index)
			current, found := existing[cname]
			delete(existing, cname)
			if found {
				fresh := current.Hash == desiredHash && !o.forceRecreate
				if fresh && current.Running {
					info("Container " + cname + " is up-to-date")
					continue
				}
				if err := waitDeps(); err != nil {
					return nil, err
				}
				if fresh {
					info("Starting " + cname + " ...")
					if _, err := engine.RunRetried([]string{"start", cname}, g.dryRun); err != nil {
						return nil, err
					}
					started = append(started, cname)
					continue
				}
				info("Recreating " + cname + " ...")
				if current.Running {
					if _, err := engine.Run([]string{"stop", "-t", strconv.Itoa(o.timeout), cname}, quiet); err != nil {
						return nil, err
					}
				}
				if _, err := engine.Run([]string{"remove", "-f", cname}, quiet); err != nil {
					return nil, err
				}
			} else {
				if err := waitDeps(); err != nil {
					return nil, err
				}
				info("Creating " + cname + " ...")
			}
			if err := g.createContainer(project, svc, index); err != nil {
				return nil, err
			}
			started = append(started, cname)
		}

		// drop replicas beyond the requested scale
		for _, name := range sortedKeys(existing) {
			leftover := existing[name]
			if leftover.Service == svc.Name && leftover.Index > replicas {
				info("Removing surplus " + name + " ...")
				if _, err := engine.Run([]string{"remove", "-f", name}, quiet); err != nil {
					return nil, err
				}
				delete(existing, name)
			}
		}
	}
	return started, nil
}

// createContainer creates and starts a container of svc. wslc run attaches a
// single network, so a service on several networks is created, connected to
// the other ones, then started: it sees all of them from its first process.
func (g *globals) createContainer(project *compose.Project, svc *compose.Service, index int) error {
	if len(svc.Networks) <= 1 {
		_, err := engine.Run(compose.RunArgs(project, svc, index, true, engine.ToHostPath), engine.Opts{DryRun: g.dryRun})
		return err
	}
	name := project.ContainerName(svc, index)
	if err := g.createConnected(compose.CreateArgs(project, svc, index, engine.ToHostPath), svc, name, true); err != nil {
		return err
	}
	_, err := engine.RunRetried([]string{"start", name}, g.dryRun)
	return err
}

// createConnected runs a `wslc create` command and connects the container to
// the networks of svc after the first. On failure the container is removed,
// so that it is not later started with networks missing.
func (g *globals) createConnected(createArgs []string, svc *compose.Service, name string, aliases bool) error {
	quiet := engine.Opts{Capture: true, DryRun: g.dryRun}
	if _, err := engine.Run(createArgs, quiet); err != nil {
		return err
	}
	for _, args := range compose.ConnectArgs(svc, name, aliases) {
		if _, err := engine.Run(args, quiet); err != nil {
			if _, rmErr := engine.Run([]string{"remove", "-f", name}, quiet); rmErr != nil {
				errorMsg(fmt.Sprintf("warning: could not remove %s: %v", name, rmErr))
			}
			return err
		}
	}
	return nil
}

// overridable in tests
var (
	pollInterval = time.Second
	inspectFn    = engine.Inspect
)

// waitForDependencies blocks until every replica of the dependencies of svc
// declared with condition service_healthy is healthy, and with
// service_completed_successfully has exited with status 0.
func waitForDependencies(project *compose.Project, svc *compose.Service, replicas func(*compose.Service) int, dryRun bool) error {
	for _, depName := range svc.DependsOn {
		condition := svc.DependsOnConditions[depName]
		dep := project.Services[depName]
		if condition == "" || dep == nil {
			continue
		}
		for index := 1; index <= replicas(dep); index++ {
			name := project.ContainerName(dep, index)
			if dryRun {
				info(fmt.Sprintf("Waiting for %s (%s)", name, condition))
				continue
			}
			if err := waitForContainer(name, condition); err != nil {
				return err
			}
		}
	}
	return nil
}

func waitForContainer(name, condition string) error {
	announced := false
	for {
		data, err := inspectFn(name)
		if err != nil {
			return fmt.Errorf("dependency %s: %w", name, err)
		}
		done, err := conditionMet(name, condition, data)
		if done || err != nil {
			if announced && err == nil {
				info("Container " + name + " is " + map[string]string{
					compose.ConditionHealthy:   "healthy",
					compose.ConditionCompleted: "done",
				}[condition])
			}
			return err
		}
		if !announced {
			info("Waiting for " + name + " ...")
			announced = true
		}
		select {
		case <-engine.Interrupted():
			return engine.ErrInterrupted
		case <-time.After(pollInterval):
		}
	}
}

// conditionMet checks a depends_on condition against inspect data; a
// condition that can no longer be met is an error.
func conditionMet(name, condition string, data map[string]any) (bool, error) {
	state, _ := data["State"].(map[string]any)
	running, _ := state["Running"].(bool)
	exitCode, _ := state["ExitCode"].(float64)
	switch condition {
	case compose.ConditionCompleted:
		if running {
			return false, nil
		}
		if exitCode != 0 {
			return false, fmt.Errorf("dependency %s did not complete successfully: exit %d", name, int(exitCode))
		}
		return true, nil
	case compose.ConditionHealthy:
		health, _ := state["Health"].(map[string]any)
		if health == nil {
			config, _ := data["Config"].(map[string]any)
			hc, _ := config["Healthcheck"].(map[string]any)
			if test, _ := hc["Test"].([]any); len(test) == 0 || str(test[0]) == "NONE" {
				return false, fmt.Errorf("dependency %s has no healthcheck configured", name)
			}
		}
		switch str(health["Status"]) {
		case "healthy":
			return true, nil
		case "unhealthy":
			return false, fmt.Errorf("dependency %s is unhealthy", name)
		}
		if !running && str(state["Status"]) != "created" {
			return false, fmt.Errorf("dependency %s exited (%d) before becoming healthy", name, int(exitCode))
		}
		return false, nil
	}
	return true, nil
}

const runUsage = `usage: wslc-compose run [-d] [--rm] [--name NAME] [--no-deps] [--build] [-e KEY=VAL] [-l KEY=VAL]
                        [-v VOLUME] [-p PORT] [-P] [--entrypoint CMD] [-u USER] [-w DIR] [-T]
                        SERVICE [COMMAND [ARG...]]

Run a one-off command in a new container of a service. The services it
depends on are started first; its ports are not published unless -P is given.

  -d, --detach          run the container in the background and print its name
      --rm              remove the container when it exits
      --name NAME       container name (default: PROJECT-SERVICE-run-ID)
      --no-deps         do not start the services it depends on
      --build           build the image before starting the container
  -e, --env KEY=VAL     set an environment variable (repeatable)
  -l, --label KEY=VAL   add a label (repeatable)
  -v, --volume VOLUME   mount a volume or a host path (repeatable)
  -p, --publish PORT    publish a container port (repeatable)
  -P, --service-ports   publish the ports of the service
      --entrypoint CMD  override the entrypoint of the image
  -u, --user USER       run as this user
  -w, --workdir DIR     working directory inside the container
  -T, --no-tty          disable pseudo-TTY allocation
`

func cmdRun(g *globals, args []string) (int, error) {
	fs := newFlagSet("run", runUsage)
	detach := fs.BoolP("detach", "d", false, "")
	rm := fs.Bool("rm", false, "")
	name := fs.String("name", "", "")
	noDeps := fs.Bool("no-deps", false, "")
	build := fs.Bool("build", false, "")
	env := fs.StringArrayP("env", "e", nil, "")
	labels := fs.StringArrayP("label", "l", nil, "")
	volumes := fs.StringArrayP("volume", "v", nil, "")
	publish := fs.StringArrayP("publish", "p", nil, "")
	servicePorts := fs.BoolP("service-ports", "P", false, "")
	entrypoint := fs.String("entrypoint", "", "")
	user := fs.StringP("user", "u", "", "")
	workdir := fs.StringP("workdir", "w", "", "")
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

	one := *svc // the one-off container's service, with the overrides
	if len(command) > 0 {
		one.Command = command
	}
	if fs.Changed("entrypoint") {
		if one.Entrypoint, err = compose.ParseCommand(*entrypoint); err != nil {
			return 1, &compose.ComposeError{Msg: fmt.Sprintf("--entrypoint: %v", err)}
		}
	}
	if !*servicePorts {
		one.Ports = nil
	}
	one.Ports = append([]compose.PortMapping(nil), one.Ports...)
	for _, spec := range *publish {
		mappings, err := compose.ParsePort(spec)
		if err != nil {
			return 1, err
		}
		one.Ports = append(one.Ports, mappings...)
	}
	one.Environment = map[string]*string{}
	for k, v := range svc.Environment {
		one.Environment[k] = v
	}
	for _, e := range *env {
		key, value, found := strings.Cut(e, "=")
		if found {
			one.Environment[key] = &value
		} else {
			one.Environment[key] = nil
		}
	}
	one.Labels = map[string]string{}
	for k, v := range svc.Labels {
		one.Labels[k] = v
	}
	for _, l := range *labels {
		key, value, _ := strings.Cut(l, "=")
		one.Labels[key] = value
	}
	one.Volumes = append([]*compose.VolumeMount(nil), svc.Volumes...)
	for _, spec := range *volumes {
		mount, err := compose.ParseVolume(spec, project.Directory)
		if err != nil {
			return 1, err
		}
		if vol, ok := project.Volumes[mount.Source]; ok && mount.Type == "volume" {
			mount.Source = vol.Name
		}
		one.Volumes = append(one.Volumes, mount)
	}
	if *user != "" {
		one.User = *user
	}
	if *workdir != "" {
		one.WorkingDir = *workdir
	}
	if *detach {
		one.TTY = one.TTY && !*noTTY
	} else {
		one.StdinOpen = true
		one.TTY = !*noTTY && isTerminal(os.Stdin)
	}
	containerName := *name
	if containerName == "" {
		suffix := make([]byte, 6)
		rand.Read(suffix)
		containerName = fmt.Sprintf("%s-%s-run-%s", project.Name, svc.Name, hex.EncodeToString(suffix))
	}

	var deps []*compose.Service
	if !*noDeps {
		selected, err := selectServices(project, []string{svc.Name}, g.profiles)
		if err != nil {
			return 1, err
		}
		for _, s := range selected {
			if s.Name != svc.Name {
				deps = append(deps, s)
			}
		}
	}
	if err := g.ensureResources(project, append(deps, &one)); err != nil {
		return 1, err
	}
	if err := g.buildImages(project, deps, false); err != nil {
		return 1, err
	}
	if err := g.buildImages(project, []*compose.Service{&one}, *build); err != nil {
		return 1, err
	}
	if len(deps) > 0 {
		containers, err := g.listContainers(project, deps)
		if err != nil {
			return 1, err
		}
		if _, err := g.converge(project, deps, containers, convergeOptions{timeout: 10}); err != nil {
			return 1, err
		}
		if err := waitForDependencies(project, &one, convergeOptions{}.replicas, g.dryRun); err != nil {
			return 1, err
		}
	}

	if len(one.Networks) <= 1 {
		runCmd := []string{"run"}
		if *detach {
			runCmd = append(runCmd, "-d")
		}
		if *rm {
			runCmd = append(runCmd, "--rm")
		}
		runArgs := compose.OneOffArgs(runCmd, project, &one, containerName, engine.ToHostPath)
		if !*detach {
			return engine.RunInteractive(runArgs, g.dryRun)
		}
		if _, err := engine.Run(runArgs, engine.Opts{Capture: true, DryRun: g.dryRun}); err != nil {
			return 1, err
		}
		info(containerName)
		return 0, nil
	}

	createCmd := []string{"create"}
	if *rm {
		createCmd = append(createCmd, "--rm")
	}
	createArgs := compose.OneOffArgs(createCmd, project, &one, containerName, engine.ToHostPath)
	if err := g.createConnected(createArgs, &one, containerName, false); err != nil {
		return 1, err
	}
	if *detach {
		if _, err := engine.RunRetried([]string{"start", containerName}, g.dryRun); err != nil {
			return 1, err
		}
		info(containerName)
		return 0, nil
	}
	startArgs := []string{"start", "-a"}
	if one.StdinOpen {
		startArgs = append(startArgs, "-i")
	}
	return engine.RunInteractive(append(startArgs, containerName), g.dryRun)
}
