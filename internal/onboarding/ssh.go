// Package onboarding checks Alpine over USB Ethernet and changes its root password.
package onboarding

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

const USBAddress = "192.168.14.14:22"
const checkCommand = `test "$(id -u)" = 0 && test -f /etc/alpine-release && cat /etc/alpine-release`

var ErrAuthentication = errors.New("не удалось войти по SSH: проверьте текущий пароль")
var ErrHostKeyChanged = errors.New("ключ SSH изменился; проверка остановлена, перепроверьте подключённое устройство")

// PasswordError means delivery may have happened; it never promises rollback.
type PasswordError struct{ Cause error }

func (e *PasswordError) Error() string {
	return "пароль мог уже измениться; проверьте вход с новым паролем: " + e.Cause.Error()
}
func (e *PasswordError) Unwrap() error { return e.Cause }

type Info struct{ Version, HostKey string }
type Client struct {
	address     string
	mu          sync.Mutex
	hostKey     []byte
	fingerprint string
}

// New scopes first-use host-key trust to this setup session, not global known_hosts.
func New(address string) *Client { return &Client{address: address} }
func ValidatePassword(p string) error {
	if !utf8.ValidString(p) || utf8.RuneCountInString(p) < 8 || utf8.RuneCountInString(p) > 128 {
		return errors.New("используйте от 8 до 128 символов")
	}
	for _, r := range p {
		if unicode.IsControl(r) || r == ':' {
			return errors.New("пароль не должен содержать двоеточие или управляющие символы")
		}
	}
	return nil
}
func (c *Client) checkHost(_ string, _ net.Addr, key ssh.PublicKey) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.hostKey) == 0 {
		c.hostKey = append([]byte(nil), key.Marshal()...)
		c.fingerprint = ssh.FingerprintSHA256(key)
		return nil
	}
	if !bytes.Equal(c.hostKey, key.Marshal()) {
		return ErrHostKeyChanged
	}
	return nil
}
func (c *Client) run(ctx context.Context, password, command, input string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", c.address)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return "", err
	}
	cfg := &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.Password(password)}, HostKeyCallback: c.checkHost}
	cc, chans, reqs, err := ssh.NewClientConn(conn, c.address, cfg)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(err, ErrHostKeyChanged) {
			return "", ErrHostKeyChanged
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return "", ErrAuthentication
		}
		return "", err
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	session.Stdin = strings.NewReader(input)
	// Remote output is bounded, and chpasswd output is never returned to the UI.
	var out boundedOutput
	session.Stdout = &out
	session.Stderr = io.Discard
	err = session.Run(command)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return out.String(), err
}

// Wait retries only connection/service availability. Authentication and key errors
// need user action and must not be hidden in a long readiness loop.
func (c *Client) Wait(ctx context.Context, password string) (Info, error) {
	for {
		output, err := c.run(ctx, password, checkCommand, "")
		if err == nil {
			version := strings.TrimSpace(output)
			if version == "" || len(version) > 64 || strings.ContainsAny(version, "\r\n\x1b") {
				return Info{}, errors.New("SSH доступен, но Alpine не подтверждён")
			}
			c.mu.Lock()
			key := c.fingerprint
			c.mu.Unlock()
			return Info{Version: version, HostKey: key}, nil
		}
		var exit *ssh.ExitError
		if errors.Is(err, ErrAuthentication) || errors.Is(err, ErrHostKeyChanged) || errors.As(err, &exit) {
			return Info{}, err
		}
		if ctx.Err() != nil {
			return Info{}, ctx.Err()
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Info{}, ctx.Err()
		case <-timer.C:
		}
	}
}
func (c *Client) ChangePassword(ctx context.Context, current, next string) error {
	if err := ValidatePassword(next); err != nil {
		return err
	}
	if _, err := c.run(ctx, current, "chpasswd && sync", "root:"+next+"\n"); err != nil {
		return &PasswordError{Cause: err}
	}
	if _, err := c.run(ctx, next, checkCommand, ""); err != nil {
		return &PasswordError{Cause: err}
	}
	return nil
}

// boundedOutput consumes excess data without unbounded memory or a stuck server.
type boundedOutput struct{ bytes.Buffer }

func (w *boundedOutput) Write(b []byte) (int, error) {
	n := len(b)
	if w.Len() < 4096 {
		_, _ = w.Buffer.Write(b[:min(len(b), 4096-w.Len())])
	}
	return n, nil
}
