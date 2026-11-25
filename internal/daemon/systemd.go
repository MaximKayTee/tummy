package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// UnitName is the systemd user unit of tummy serve.
const UnitName = "tummy.service"

// CanInstall reports whether systemctl is on PATH.
func CanInstall() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// UnitPath is the user unit file.
func UnitPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "systemd", "user", UnitName), nil
}

// Unit is the unit file that runs cmd.
func Unit(cmd []string) string {
	args := make([]string, len(cmd))
	for i, a := range cmd {
		args[i] = unitQuote(a)
	}
	return `[Unit]
Description=tummy scheduler

[Service]
ExecStart=` + strings.Join(args, " ") + `
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure

[Install]
WantedBy=default.target
`
}

// unitQuote quotes a as one ExecStart word, no specifier or variable expansion.
func unitQuote(a string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`, "\n", `\n`)
	return `"` + r.Replace(a) + `"`
}

// Install writes the user unit for cmd, enables and restarts it, and turns on lingering
// so it runs without a login session.
func Install(ctx context.Context, cmd []string) error {
	path, err := UnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(Unit(cmd)), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", UnitName},
		{"systemctl", "--user", "restart", UnitName},
		{"loginctl", "enable-linger"},
	} {
		if err := run(ctx, args); err != nil {
			return err
		}
	}
	return nil
}

func run(ctx context.Context, args []string) error {
	var out bytes.Buffer
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	c.Stdout, c.Stderr = &out, &out
	if err := c.Run(); err != nil {
		if msg := strings.TrimSpace(out.String()); msg != "" {
			return fmt.Errorf("%s: %s", strings.Join(args, " "), msg)
		}
		return fmt.Errorf("%s: %w", strings.Join(args, " "), err)
	}
	return nil
}
