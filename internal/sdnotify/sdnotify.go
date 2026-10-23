// Package sdnotify implements the systemd readiness protocol (sd_notify)
// without cgo or external dependencies.
package sdnotify

import (
	"net"
	"os"
)

// Notify sends state (for example "READY=1") to the service manager. It is a
// no-op returning false when not running under systemd with Type=notify.
func Notify(state string) (bool, error) {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return false, nil
	}
	addr := &net.UnixAddr{Name: socket, Net: "unixgram"}
	if socket[0] == '@' {
		// Abstract namespace socket.
		addr.Name = "\x00" + socket[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, addr)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}
