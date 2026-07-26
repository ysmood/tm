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
			exit(app.RunDaemon(sessionID("daemon", os.Args[2:])), "daemon")

			return
		case "__attach":
			exit(app.RunAttach(sessionID("attach", os.Args[2:])), "attach")

			return
		}
	}

	exit(app.Run(), "")
}

// sessionID parses the --id flag both hidden subcommands take, naming the flag
// set after the subcommand (sub) so a usage error reports the right one.
func sessionID(sub string, args []string) string {
	fs := flag.NewFlagSet(sub, flag.ExitOnError)
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
