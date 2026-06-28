// Command tm is a lightweight terminal multiplexer manager. Running it with no
// arguments opens the interactive menu; the hidden __daemon and __attach
// subcommands are used internally to run and connect to persistent sessions.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ysmood/tm/pkg/app"
	"github.com/ysmood/tm/pkg/proto"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "__daemon":
			exit(app.RunDaemon(subID(os.Args[2:])), "daemon")

			return
		case "__attach":
			id, hist, lines := subAttach(os.Args[2:])
			exit(app.RunAttach(id, proto.HistMode(hist), uint32(lines)), "attach")

			return
		}
	}

	exit(app.Run(), "")
}

// subID parses the --id flag of the __daemon subcommand.
func subID(args []string) string {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	id := fs.String("id", "", "session id")
	_ = fs.Parse(args)

	return *id
}

// subAttach parses the flags of the __attach subcommand.
func subAttach(args []string) (id string, hist, lines int) {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	idp := fs.String("id", "", "session id")
	histp := fs.Int("hist", 0, "history mode")
	linesp := fs.Int("lines", 0, "history lines")
	_ = fs.Parse(args)

	return *idp, *histp, *linesp
}

func exit(err error, what string) {
	if err == nil {
		return
	}

	if what == "" {
		fmt.Fprintln(os.Stderr, "tm:", err)
	} else {
		fmt.Fprintf(os.Stderr, "tm %s: %v\n", what, err)
	}

	os.Exit(1)
}
