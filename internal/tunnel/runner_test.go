package tunnel

import (
	"strings"
	"testing"

	"github.com/ArnaudGuiovanna/ouvrier/internal/deploy"
)

func TestSSHArgsShape(t *testing.T) {
	opts := deploy.ConnectOpts{
		Host:       "worker.example.com",
		User:       "deploy",
		Port:       2222,
		Identity:   "/keys/id_ed25519",
		KnownHosts: "/proj/ouvrier.known_hosts",
	}
	fwd := Forward{Network: "unix", LocalAddr: "/run/u/t/w1.sock", RemoteAddr: "127.0.0.1:9090"}
	args := sshArgs(opts, fwd)
	joined := " " + strings.Join(args, " ") + " "

	if args[0] != "-N" {
		t.Fatalf("argv must start with -N, got %v", args)
	}
	for _, want := range []string{
		" -o BatchMode=yes ",
		" -o ExitOnForwardFailure=yes ",
		" -o ServerAliveInterval=15 ",
		" -o StrictHostKeyChecking=yes ",
		" -o PasswordAuthentication=no ",
		" -o KbdInteractiveAuthentication=no ",
		" -o UserKnownHostsFile=/proj/ouvrier.known_hosts ",
		" -o StreamLocalBindMask=0177 ",
		" -i /keys/id_ed25519 ",
		" -p 2222 ",
		" -L /run/u/t/w1.sock:127.0.0.1:9090 ",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %v", strings.TrimSpace(want), args)
		}
	}
	if args[len(args)-1] != "deploy@worker.example.com" {
		t.Fatalf("argv must end with user@host, got %v", args)
	}
	// No remote command: user@host is the final word, and nothing in the
	// argv could carry a token (none is ever passed in).
	if strings.Contains(joined, "TOKEN") || strings.Contains(joined, "token") {
		t.Fatalf("argv mentions tokens: %v", args)
	}
}

func TestSSHArgsTCPOmitsSocketMask(t *testing.T) {
	opts := deploy.ConnectOpts{Host: "h1"}
	fwd := Forward{Network: "tcp", LocalAddr: "127.0.0.1:43210", RemoteAddr: "127.0.0.1:9090"}
	args := sshArgs(opts, fwd)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "StreamLocalBindMask") {
		t.Fatalf("tcp forward must not set the unix socket mask: %v", args)
	}
	if !strings.Contains(joined, "-L 127.0.0.1:43210:127.0.0.1:9090") {
		t.Fatalf("tcp forward argv = %v", args)
	}
	if args[len(args)-1] != "h1" {
		t.Fatalf("empty user must yield bare host, got %v", args)
	}
}
