// Command tm is a lightweight terminal multiplexer manager. Running it with no
// arguments opens the interactive menu; the hidden __daemon and __attach
// subcommands are used internally to run and connect to persistent sessions.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ysmood/tm/pkg/app"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "__daemon":
			exit(app.RunDaemon(subID(os.Args[2:])), "daemon")

			return
		case "__attach":
			exit(app.RunAttach(subAttachID(os.Args[2:])), "attach")

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

// subAttachID parses the --id flag of the __attach subcommand.
func subAttachID(args []string) string {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	id := fs.String("id", "", "session id")
	_ = fs.Parse(args)

	return *id
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
