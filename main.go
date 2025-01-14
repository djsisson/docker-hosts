package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

var (
	Containers = struct {
		sync.RWMutex
		Host map[string]map[string]string
	}{
		Host: make(map[string]map[string]string)}
)

type Hosts struct {
	ContainerID string
	Network     string
	IP          string
	Hosts       []string
}

type Network struct {
	ContainerID string
	Network     string
	Name        string
	HostName    string
}

func main() {
	ctx := context.Background()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}
	defer cli.Close()
	containers, _ := cli.ContainerList(ctx, container.ListOptions{})
	results := make(chan Hosts)
	var wg sync.WaitGroup
	for _, container := range containers {
		wg.Add(1)
		go func(container types.Container, results chan Hosts) {
			defer wg.Done()
			c, err := cli.ContainerInspect(ctx, container.ID)
			if err != nil {
				return
			}
			inspectContainer(&c, results, "")
		}(container, results)
	}
	go func() {
		wg.Wait()
		close(results)
	}()
	Containers.Lock()
	for host := range results {
		if host.ContainerID == "" {
			delete(Containers.Host, host.ContainerID)
			continue
		}
		if _, ok := Containers.Host[host.ContainerID]; !ok {
			Containers.Host[host.ContainerID] = make(map[string]string)
		}
		Containers.Host[host.ContainerID][host.Network] = strings.Join(append([]string{host.IP}, host.Hosts...), " ")
	}
	Containers.Unlock()
	updateHostsFile()

	event, errs := cli.Events(ctx, events.ListOptions{
		Since: "10s",
		Filters: filters.NewArgs(
			filters.Arg("type", "network"),
		),
	})

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-errs:
			if err != nil {
				println(err)
				return
			}
		case event := <-event:
			if event.Actor.Attributes["name"] == "host" {
				continue
			}
			if event.Action == "connect" {

				c, err := cli.ContainerInspect(ctx, event.Actor.Attributes["container"])
				if err != nil {
					delete(Containers.Host, event.Actor.Attributes["container"])
					updateHostsFile()
					continue
				}
				var wg sync.WaitGroup
				wg.Add(1)
				results := make(chan Hosts)
				go func() {
					defer wg.Done()
					inspectContainer(&c, results, event.Actor.Attributes["name"])
				}()
				go func() {
					wg.Wait()
					close(results)
				}()

				Containers.Lock()
				for host := range results {
					if host.ContainerID == "" {
						delete(Containers.Host, host.ContainerID)
						continue
					}
					if _, ok := Containers.Host[host.ContainerID]; !ok {
						Containers.Host[host.ContainerID] = make(map[string]string)
					}
					Containers.Host[host.ContainerID][host.Network] = strings.Join(append([]string{host.IP}, host.Hosts...), " ")
				}
				Containers.Unlock()
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

func hostsFromNetwork(n *network.EndpointSettings, h Network, c chan Hosts) {
	if len(n.Aliases) == 0 || n.IPAddress == "" {
		return
	}
	aliases := make(map[string]bool)
	for _, alias := range n.Aliases {
		aliases[alias] = true
	}
	aliases[h.HostName] = true
	aliases[h.Name] = true
	var aliasSlice []string
	for alias := range aliases {
		aliasSlice = append(aliasSlice, alias)
	}
	c <- Hosts{
		ContainerID: h.ContainerID,
		Network:     h.Network,
		IP:          n.IPAddress,
		Hosts:       aliasSlice,
	}
}

func inspectContainer(c *types.ContainerJSON, h chan Hosts, n string) {

	containerName := c.Name[1:]
	containerID := c.ID
	containerHostName := c.Config.Hostname
	containerIp := c.NetworkSettings.IPAddress
	if c.Config.Domainname != "" {
		containerHostName = containerHostName + "." + c.Config.Domainname
	}
	if containerIp != "" {
		h <- Hosts{
			ContainerID: containerID,
			Network:     "bridge",
			IP:          containerIp,
			Hosts:       []string{containerHostName, containerName},
		}
	}
	if n != "" {
		hostsFromNetwork(c.NetworkSettings.Networks[n], Network{
			ContainerID: containerID,
			Network:     n,
			Name:        containerName,
			HostName:    containerHostName,
		}, h)
		return
	}
	wg := sync.WaitGroup{}
	for name, network := range c.NetworkSettings.Networks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hostsFromNetwork(network, Network{
				ContainerID: containerID,
				Network:     name,
				Name:        containerName,
				HostName:    containerHostName,
			}, h)
		}()
	}
	wg.Wait()
}

func updateHostsFile() {
	lines := []string{}
	BeginText := "### BEGIN DOCKER CONTAINERS ###"
	EndText := "### END DOCKER CONTAINERS ###"
	dstPath := "/tmp/hosts"
	tmpPath := "/tmp/hosts.tmp"
	file, err := os.Open(dstPath)
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
	tmpfile, err := os.Create(tmpPath)

	if err != nil {
		println("Error creating temp hosts file")
		return
	}
	defer tmpfile.Close()
	for _, line := range lines {
		tmpfile.WriteString(line + "\n")
	}
	tmpfile.Seek(0, 0)

	dst, err := os.Create(dstPath)
	if err != nil {
		println("Error creating hosts file")
		return
	}
	defer dst.Close()
	if _, err = io.Copy(dst, tmpfile); err != nil {
		println("Error updating hosts file")
		return
	}
}
