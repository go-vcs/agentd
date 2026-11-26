// Command forge-agentd is the local agent-VCS daemon with a GitHub backend.
package main

import "github.com/go-vcs/agentd/internal/cli"

func main() { cli.Main(cli.DaemonCmd()) }
