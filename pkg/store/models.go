package store

import "time"

// Reserved namespace names.
const (
	// DefaultNamespace is the namespace tm starts in.
	DefaultNamespace = "default"
	// AllNamespaces is the virtual namespace that matches every session.
	AllNamespaces = "*"
)

// Session is the persisted metadata for one shell session. Volatile locations
// (socket, log) are derived from ID at runtime rather than stored, so the data
// stays valid if the storage root moves.
type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	PID       int       `json:"pid"`
	Shell     string    `json:"shell"`
	Cwd       string    `json:"cwd"`
	CreatedAt time.Time `json:"createdAt"`
}
