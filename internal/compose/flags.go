package compose

import (
	"path/filepath"
	"strconv"
)

// PathMapper translates a host path into the form wslc expects.
type PathMapper func(string) string

func identity(p string) string { return p }

// RunArgs builds the arguments for `wslc run` creating one container of a service.
func RunArgs(p *Project, s *Service, index int, detach bool, mapper PathMapper) []string {
	command := []string{"run"}
	if detach {
		command = append(command, "-d")
	}
	return containerArgs(command, p, s, p.ContainerName(s, index), replicaLabels(p, s, index), true, mapper)
}

// CreateArgs builds the arguments for `wslc create`, for a container that is
// connected to more networks (ConnectArgs) before it starts.
func CreateArgs(p *Project, s *Service, index int, mapper PathMapper) []string {
	return containerArgs([]string{"create"}, p, s, p.ContainerName(s, index), replicaLabels(p, s, index), true, mapper)
}

// ConnectArgs builds the `wslc network connect` commands attaching a
// container to the networks of s after the first one, which wslc run and
// create take.
func ConnectArgs(s *Service, name string, aliases bool) [][]string {
	var commands [][]string
	for i := 1; i < len(s.Networks); i++ {
		network := s.Networks[i]
		args := []string{"network", "connect"}
		if aliases {
			for _, alias := range networkAliases(s, network) {
				args = append(args, "--network-alias", alias)
			}
		}
		commands = append(commands, append(args, network, name))
	}
	return commands
}

func replicaLabels(p *Project, s *Service, index int) []string {
	return []string{
		LabelProject + "=" + p.Name,
		LabelService + "=" + s.Name,
		LabelIndex + "=" + strconv.Itoa(index),
		LabelConfigHash + "=" + s.ConfigHash(),
	}
}

// networkAliases returns the service name and the aliases set for network.
func networkAliases(s *Service, network string) []string {
	aliases := map[string]bool{s.Name: true}
	for _, alias := range s.NetworkAliases[network] {
		aliases[alias] = true
	}
	return sortedKeys(aliases)
}

func containerArgs(command []string, p *Project, s *Service, name string, labels []string, aliases bool, mapper PathMapper) []string {
	if mapper == nil {
		mapper = identity
	}
	args := append([]string(nil), command...)
	args = append(args, "--name", name)
	for _, label := range labels {
		args = append(args, "-l", label)
	}
	for _, key := range sortedKeys(s.Labels) {
		args = append(args, "-l", key+"="+s.Labels[key])
	}

	for _, envFile := range s.EnvFiles {
		args = append(args, "--env-file", mapper(envFile))
	}
	for _, key := range sortedKeys(s.Environment) {
		if value := s.Environment[key]; value != nil {
			args = append(args, "-e", key+"="+*value)
		} else {
			args = append(args, "-e", key)
		}
	}

	for _, port := range s.Ports {
		args = append(args, "-p", port.Flag())
	}

	for _, mount := range s.Volumes {
		if mount.Type == "tmpfs" {
			args = append(args, "--tmpfs", mount.Target)
			continue
		}
		source := mount.Source
		if mount.Type == "bind" {
			source = mapper(source)
		}
		spec := source + ":" + mount.Target
		if mount.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "-v", spec)
	}
	for _, target := range s.Tmpfs {
		args = append(args, "--tmpfs", target)
	}

	if len(s.Networks) > 0 {
		network := s.Networks[0]
		args = append(args, "--network", network)
		if aliases {
			for _, alias := range networkAliases(s, network) {
				args = append(args, "--network-alias", alias)
			}
		}
	}

	opt := func(flag, value string) {
		if value != "" {
			args = append(args, flag, value)
		}
	}
	opt("-h", s.Hostname)
	opt("--domainname", s.Domainname)
	for _, dns := range s.DNS {
		args = append(args, "--dns", dns)
	}
	for _, domain := range s.DNSSearch {
		args = append(args, "--dns-search", domain)
	}
	for _, o := range s.DNSOpt {
		args = append(args, "--dns-option", o)
	}

	opt("-u", s.User)
	opt("-w", s.WorkingDir)
	opt("-m", s.MemLimit)
	opt("--cpus", s.CPUs)
	opt("--shm-size", s.ShmSize)
	for _, ulimit := range s.Ulimits {
		args = append(args, "--ulimit", ulimit)
	}
	opt("--stop-signal", s.StopSignal)
	opt("--gpus", s.GPUs)
	if s.StdinOpen {
		args = append(args, "-i")
	}
	if s.TTY {
		args = append(args, "-t")
	}

	if len(s.Entrypoint) > 0 {
		args = append(args, "--entrypoint", s.Entrypoint[0])
	}
	args = append(args, ImageName(p, s))
	if len(s.Entrypoint) > 1 {
		args = append(args, s.Entrypoint[1:]...)
	}
	return append(args, s.Command...)
}

// BuildArgs builds the arguments for `wslc build` for a service with a build section.
func BuildArgs(p *Project, s *Service, noCache bool, mapper PathMapper) []string {
	if mapper == nil {
		mapper = identity
	}
	build := s.Build
	args := []string{"build", "-t", ImageName(p, s)}
	if build.Dockerfile != "" {
		// wslc resolves -f against its own cwd, not the build context: pass the
		// dockerfile as an absolute path run through the same mapper as the context
		dockerfile := build.Dockerfile
		if !isAbs(dockerfile) {
			dockerfile = filepath.Join(build.Context, dockerfile)
		}
		args = append(args, "-f", mapper(dockerfile))
	}
	for _, key := range sortedKeys(build.Args) {
		args = append(args, "--build-arg", key+"="+build.Args[key])
	}
	if build.Target != "" {
		args = append(args, "--target", build.Target)
	}
	if build.Pull {
		args = append(args, "--pull")
	}
	if noCache {
		args = append(args, "--no-cache")
	}
	return append(args, mapper(build.Context))
}

func ImageName(p *Project, s *Service) string {
	if s.Image != "" {
		return s.Image
	}
	return p.Name + "-" + s.Name
}
