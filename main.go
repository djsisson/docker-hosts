package main

import (
	"bufio"
	"context"
	"os"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

var Containers = struct {
	sync.RWMutex
	Host map[string]map[string]string
}{
	Host: make(map[string]map[string]string)}

func main() {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}
	containers, _ := cli.ContainerList(ctx, container.ListOptions{})
	var wait sync.WaitGroup
	wait.Add(len(containers))
	for _, container := range containers {
		go func(container types.Container) {
			inspectContainer(cli, container.ID)
			wait.Done()
		}(container)
	}
	wait.Wait()
	updateHostsFile()

	event, errs := cli.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("type", "network"),
			filters.Arg("since", "-10s"),
		),
	})
	for {
		select {
		case <-ctx.Done():
			cli.Close()
			return
		case err := <-errs:
			if err != nil {
				cli.Close()
				return
			}
		case event := <-event:
			if event.Actor.Attributes["name"] == "host" {
				continue
			}
			if event.Action == "connect" {
				inspectContainer(cli, event.Actor.Attributes["container"])
				updateHostsFile()
			} else if event.Action == "disconnect" {
				Containers.Lock()
				delete(Containers.Host[event.Actor.Attributes["container"]], event.Actor.Attributes["name"])
				if len(Containers.Host[event.Actor.Attributes["container"]]) == 0 {
					delete(Containers.Host, event.Actor.Attributes["container"])
				}
				Containers.Unlock()
				updateHostsFile()
			}
		}
	}

}

func inspectContainer(cli *client.Client, containerID string) {
	ctx := context.Background()
	container, err := cli.ContainerInspect(ctx, containerID)
	Containers.Lock()
	defer Containers.Unlock()
	delete(Containers.Host, containerID)
	if err != nil {
		return
	}
	containerName := container.Name[1:]
	containerHostName := container.Config.Hostname
	containerIp := container.NetworkSettings.IPAddress
	if container.Config.Domainname != "" {
		containerHostName = containerHostName + "." + container.Config.Domainname
	}
	Containers.Host[containerID] = make(map[string]string)
	for name, network := range container.NetworkSettings.Networks {
		if len(network.Aliases) == 0 || network.IPAddress == "" {
			continue
		}
		aliases := make(map[string]bool)
		for _, alias := range network.Aliases {
			aliases[alias] = true
		}
		aliases[containerHostName] = true
		aliases[containerName] = true
		var aliasSlice []string
		for alias := range aliases {
			aliasSlice = append(aliasSlice, alias)
		}

		Containers.Host[containerID][name] = strings.Join([]string{network.IPAddress, strings.Join(aliasSlice, " ")}, " ")
	}
	if containerIp != "" {
		Containers.Host[containerID]["bridge"] = strings.Join([]string{containerIp, containerHostName, containerName}, " ")
	}
}

func updateHostsFile() {
	lines := []string{}
	BeginText := "### BEGIN DOCKER CONTAINERS ###"
	EndText := "### END DOCKER CONTAINERS ###"
	file, err := os.Open("/tmp/hosts")
	if err != nil {
		println("Error opening hosts file")
		return
	}
	Containers.RLock()
	defer Containers.RUnlock()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, BeginText) {
			for scanner.Scan() {
				line = scanner.Text()
				if strings.HasPrefix(line, EndText) {
					break
				}
			}
		} else {
			lines = append(lines, line)
		}
	}

	lines = append(lines, BeginText)
	for _, host := range Containers.Host {
		for _, aliases := range host {
			lines = append(lines, aliases)
		}
	}
	lines = append(lines, EndText)
	file.Close()
	tmpfile, err := os.Create("/tmp/hosts.tmp")
	if err != nil {
		println("Error creating temp hosts file")
		return
	}
	for _, line := range lines {
		tmpfile.WriteString(line + "\n")
	}
	tmpfile.Close()
	err = os.Rename("/tmp/hosts.tmp", "/tmp/hosts")
	if err != nil {
		println("Error renaming hosts file ", err)
		return
	}
}
