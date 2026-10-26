package cli

import (
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
		if c.Service != "" && project.Services[c.Service] == nil {
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
		existing[c.Name] = c
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
