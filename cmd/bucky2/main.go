// bucky2 is the bucky client for go-carbon's Pebble store. It talks to the
// buckyd API embedded in go-carbon (:4242) and moves metrics with
// /v1/transfer/sync: compare digests, pull only names that differ, existing
// destination points win.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/go-graphite/buckytools"
)

// Command and NewCommand mirror cmd/bucky so sub-commands read the same.
type Command struct {
	Name  string
	Run   func(c Command) int
	Usage string
	Short string
	Long  string
	Flag  *flag.FlagSet
}

var commands []Command

func NewCommand(run func(c Command) int, name, usage, short, long string) Command {
	c := Command{Name: name, Run: run, Usage: usage, Short: short, Long: long,
		Flag: flag.NewFlagSet(name, flag.ExitOnError)}
	commands = append(commands, c)
	return c
}

func usage() {
	fmt.Fprintf(os.Stderr, "%s <sub-command> [options]\nVersion: %s\n\n"+
		"\tbucky2 works with consistent hashing Graphite clusters whose\n"+
		"\tgo-carbon nodes keep metrics in the Pebble store and serve the\n"+
		"\tembedded buckyd API.\n\n", os.Args[0], buckytools.Version)
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "%s %s\n\t%s\n", c.Name, c.Usage, c.Short)
	}
}

func main() {
	sort.Slice(commands, func(i, j int) bool { return commands[i].Name < commands[j].Name })
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	name := os.Args[1]
	args := os.Args[2:]
	if name == "help" && len(args) > 0 {
		name = args[0]
		args = []string{"-help"}
	}
	for _, c := range commands {
		if c.Name == name {
			c.Flag.Usage = func() {
				fmt.Fprintf(os.Stderr, "Usage: %s %s %s\n\n%s\n\n", os.Args[0], c.Name, c.Usage, c.Long)
				c.Flag.PrintDefaults()
			}
			c.Flag.Parse(args)
			os.Exit(c.Run(c))
		}
	}
	usage()
	os.Exit(1)
}
