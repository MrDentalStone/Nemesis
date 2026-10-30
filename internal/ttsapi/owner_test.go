package ttsapi

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

func TestNetstatPID(t *testing.T) {
	out := `
Active Connections

  Proto  Local Address          Foreign Address        State           PID
  TCP    127.0.0.1:39999        0.0.0.0:0              LISTENING       5120
  TCP    127.0.0.1:39998        127.0.0.1:51234        TIME_WAIT       0
  TCP    127.0.0.1:39998        0.0.0.0:0              LISTENING       2208
  TCP    127.0.0.1:139980       0.0.0.0:0              LISTENING       77
`
	if got := netstatPID(out, "39998"); got != "2208" {
		t.Fatalf("netstatPID = %q, want 2208", got)
	}
	if got := netstatPID(out, "1234"); got != "" {
		t.Fatalf("netstatPID for a free port = %q, want empty", got)
	}
}

// The bind error names the process holding the port: here, this test.
func TestListenNamesPortHolder(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, err = Listen("127.0.0.1:1", held.Addr().String())
	if err == nil {
		t.Fatal("second Listen on a held port succeeded")
	}
	if want := fmt.Sprintf("PID %d", os.Getpid()); !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not name %s", err, want)
	}
}
