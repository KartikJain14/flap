package sdnotify

import (
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestNotify(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if ok, err := Notify("READY=1"); ok || err != nil {
		t.Fatalf("without a socket: %v %v", ok, err)
	}
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	t.Setenv("NOTIFY_SOCKET", path)
	if ok, err := Notify("READY=1\nSTATUS=test"); !ok || err != nil {
		t.Fatalf("notify: %v %v", ok, err)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 128)
	n, _, err := conn.ReadFromUnix(buf)
	if err != nil || string(buf[:n]) != "READY=1\nSTATUS=test" {
		t.Fatalf("received %q, %v", buf[:n], err)
	}
}
