package app

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"strconv"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/ysmood/tm/pkg/attach"
	"github.com/ysmood/tm/pkg/config"
	"github.com/ysmood/tm/pkg/naming"
	"github.com/ysmood/tm/pkg/proto"
	"github.com/ysmood/tm/pkg/store"
	"github.com/ysmood/tm/pkg/tui"
)

// controller implements tui.Controller using the store and process spawning.
type controller struct{ st *store.Store }

// AttachCmd builds the `tm __attach` relay command for a session.
func (c *controller) AttachCmd(id string, hist proto.HistMode, lines uint32) *exec.Cmd {
	self, _ := os.Executable()

	return exec.Command(self, "__attach",
		"--id", id,
		"--hist", strconv.Itoa(int(hist)),
		"--lines", strconv.Itoa(int(lines)),
	)
}

// DefaultSessionName proposes a unique default name for a new session in ns.
func (c *controller) DefaultSessionName(ns string) string {
	cwd, _ := os.Getwd()
	base := naming.Generate(cwd, time.Now())
	taken := map[string]bool{}

	if sessions, err := c.st.ListByNamespace(ns); err == nil {
		for _, s := range sessions {
			taken[s.Name] = true
		}
	}

	return naming.Unique(base, taken)
}

// CreateAndSpawn writes a new session record and starts its detached daemon.
func (c *controller) CreateAndSpawn(ns, name string) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}

	cwd, _ := os.Getwd()

	sess := store.Session{
		ID:        id,
		Name:      name,
		Namespace: ns,
		Shell:     shellPath(),
		Cwd:       cwd,
		CreatedAt: time.Now(),
	}
	if err := c.st.SaveSession(sess); err != nil {
		return "", err
	}

	if err := Spawn(c.st.Paths(), sess); err != nil {
		_ = c.st.DeleteSession(id)

		return "", err
	}

	return id, nil
}

// Run launches the interactive menu. It prunes dead sessions on entry.
func Run() error {
	st, err := store.Open()
	if err != nil {
		return err
	}

	_ = st.Prune(func(s store.Session) bool { return s.PID <= 0 || processAlive(s.PID) })

	prog := tea.NewProgram(tui.New(st, &controller{st: st}))
	_, err = prog.Run()

	return err
}

// RunAttach is the entrypoint for the hidden `tm __attach` subcommand.
func RunAttach(id string, hist proto.HistMode, lines uint32) error {
	p, err := config.New()
	if err != nil {
		return err
	}

	return attach.Run(proto.SockAddr(p, id), attach.Options{Hist: hist, Lines: lines})
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}
