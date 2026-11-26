// Command forge-agent is the client agents use to call forge-agentd.
package main

import "github.com/go-vcs/agentd/internal/cli"

func main() { cli.Main(cli.AgentCmd()) }
