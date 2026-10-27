package onboarding

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

type sshFixture struct {
	mu          sync.Mutex
	password    string
	payload     string
	command     string
	changeFails bool
	verifyFails bool
	host        ssh.Signer
}

func startSSH(t *testing.T, f *sshFixture) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.host, err = ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, e := l.Accept()
			if e != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return l.Addr().String()
}
func (f *sshFixture) serve(conn net.Conn) {
	defer conn.Close()
	f.mu.Lock()
	host := f.host
	f.mu.Unlock()
	cfg := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if meta.User() != "root" || string(p) != f.password || (f.verifyFails && f.payload != "") {
			return nil, errors.New("denied")
		}
		return nil, nil
	}}
	cfg.AddHostKey(host)
	c, channels, requests, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer c.Close()
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		ch, reqs, e := incoming.Accept()
		if e != nil {
			return
		}
		go func() {
			defer ch.Close()
			for req := range reqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				var command struct{ Command string }
				ssh.Unmarshal(req.Payload, &command)
				req.Reply(true, nil)
				code := uint32(0)
				if command.Command == "chpasswd && sync" {
					b, _ := io.ReadAll(ch)
					f.mu.Lock()
					f.command = command.Command
					f.payload = string(b)
					if f.changeFails {
						code = 1
					} else {
						f.password = strings.TrimSuffix(strings.TrimPrefix(string(b), "root:"), "\n")
					}
					f.mu.Unlock()
				} else if strings.Contains(command.Command, "/etc/alpine-release") {
					io.WriteString(ch, "3.24.1\n")
				} else {
					code = 127
				}
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
				return
			}
		}()
	}
}
func TestWaitAndLiteralPasswordChange(t *testing.T) {
	f := &sshFixture{password: "1"}
	c := New(startSSH(t, f))
	info, err := c.Wait(context.Background(), "1")
	if err != nil || info.Version != "3.24.1" || !strings.HasPrefix(info.HostKey, "SHA256:") {
		t.Fatalf("info=%+v err=%v", info, err)
	}
	next := " spaced '$(touch /tmp/no)' <3 пароль "
	if err := c.ChangePassword(context.Background(), "1", next); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.command != "chpasswd && sync" || f.payload != "root:"+next+"\n" || f.password != next {
		t.Fatal("password was not delivered literally via stdin")
	}
}
func TestAuthenticationFailureDoesNotWaitAndPasswordErrorsArePrivate(t *testing.T) {
	c := New(startSSH(t, &sshFixture{password: "different"}))
	started := time.Now()
	_, err := c.Wait(context.Background(), "SECRET")
	if !errors.Is(err, ErrAuthentication) || strings.Contains(err.Error(), "SECRET") || time.Since(started) > time.Second {
		t.Fatalf("%v", err)
	}
}
func TestPasswordFailureExplainsUncertainState(t *testing.T) {
	for _, changeFails := range []bool{false, true} {
		t.Run(map[bool]string{true: "remote change failed", false: "fresh login failed"}[changeFails], func(t *testing.T) {
			f := &sshFixture{password: "1", changeFails: changeFails, verifyFails: !changeFails}
			c := New(startSSH(t, f))
			err := c.ChangePassword(context.Background(), "1", "secret-new-value")
			var state *PasswordError
			if !errors.As(err, &state) || strings.Contains(err.Error(), "secret-new-value") {
				t.Fatalf("expected uncertain state, got %v", err)
			}
		})
	}
}
func TestRejectsChangedHostKeyBeforePasswordChange(t *testing.T) {
	f := &sshFixture{password: "1"}
	c := New(startSSH(t, f))
	if _, err := c.Wait(context.Background(), "1"); err != nil {
		t.Fatal(err)
	}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	next, _ := ssh.NewSignerFromKey(key)
	f.mu.Lock()
	f.host = next
	f.mu.Unlock()
	_, err := c.Wait(context.Background(), "1")
	if !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("%v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.payload != "" {
		t.Fatal("changed password on unknown host")
	}
}
func TestCancellationStopsSilentSSHHandshake(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, e := l.Accept()
		if e == nil {
			defer c.Close()
			io.Copy(io.Discard, c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = New(l.Addr().String()).Wait(ctx, "1")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("%v", err)
	}
}
func TestPasswordValidationRejectsAccountAndLineInjection(t *testing.T) {
	for _, p := range []string{"1", "1234567", "valid123\nroot:evil", "valid123\r", "valid123\x00", "root:other-password", strings.Repeat("a", 129)} {
		if ValidatePassword(p) == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	for _, p := range []string{"12345678", " привет <3 ", "shell'$()`\\password"} {
		if err := ValidatePassword(p); err != nil {
			t.Fatalf("rejected valid password: %v", err)
		}
	}
}
