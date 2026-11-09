package deploy

import "github.com/versolauth/versola-cli/internal/docker"

// OpenbaoContainerStatus also finds OpenBao retained from an earlier Compose
// project (Configure deliberately leaves it running). Only the same strict
// status fields are read; never its environment or command.
func OpenbaoContainerStatus(target string) (*ContainerStatus, error) {
	return openbaoContainerStatus(target, docker.Inspect)
}

func openbaoContainerStatus(target string, inspect func(string, string) (string, bool, error)) (*ContainerStatus, error) {
	out, found, err := inspect(OpenbaoContainerName(target), containerInspectFormat)
	if err != nil || !found {
		return nil, err
	}
	containers, err := parseContainerInspect([]byte(out))
	if err != nil {
		return nil, err
	}
	if len(containers) == 0 {
		return nil, nil
	}
	c := containers[0]
	c.Service, c.composeService, c.Slot = "openbao", "openbao", 0
	return &c, nil
}

// OpenbaoLogProject keeps `versola logs openbao` useful for that retained
// container too. Selecting its project still uses Compose's log formatting
// and all the existing flags, without reading any credentials.
func OpenbaoLogProject(target string) (string, error) {
	project, _, err := docker.Inspect(OpenbaoContainerName(target), `{{index .Config.Labels "com.docker.compose.project"}}`)
	return project, err
}
