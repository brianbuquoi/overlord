// Package main implements an echo plugin for Orcastrator. It returns
// its input payload as output, useful for testing and as a template
// for plugin authors.
//
// Build:
//
//	go build -buildmode=plugin -o echo.so ./examples/plugins/echo/
package main

import (
	"context"

	"github.com/orcastrator/orcastrator/pkg/plugin"
)

// Plugin is the exported symbol that Orcastrator looks up when loading
// a plugin .so file. It must satisfy the plugin.AgentPlugin interface.
var Plugin plugin.AgentPlugin = &echoPlugin{}

type echoPlugin struct{}

func (p *echoPlugin) ProviderName() string { return "echo" }

func (p *echoPlugin) NewAgent(cfg plugin.PluginAgentConfig) (plugin.Agent, error) {
	return &echoAgent{
		id:       cfg.ID,
		provider: "echo",
	}, nil
}

type echoAgent struct {
	id       string
	provider string
}

func (a *echoAgent) ID() string       { return a.id }
func (a *echoAgent) Provider() string { return a.provider }

// Execute returns the input payload unmodified.
func (a *echoAgent) Execute(_ context.Context, task *plugin.Task) (*plugin.TaskResult, error) {
	return &plugin.TaskResult{
		TaskID:  task.ID,
		Payload: task.Payload,
	}, nil
}

// HealthCheck always succeeds.
func (a *echoAgent) HealthCheck(_ context.Context) error {
	return nil
}

// main is required by `go build` but unused in plugin mode.
func main() {}
