//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const stateDir = "/var/lib/tailscale-personal"
const lockPath = "/var/run/tailscale-personal-wrapper.lock"

type paths struct{ self, daemon, config, cli, settings string }

func localPaths() (paths, error) {
	self, err := os.Executable()
	if err != nil {
		return paths{}, err
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return paths{}, err
	}
	return pathsForExecutable(self), nil
}

func pathsForExecutable(self string) paths {
	if self == installedWrapper {
		return paths{self, installedDaemon, installedConfig, installedCLI, installedSettings}
	}
	project := filepath.Dir(self)
	return paths{self, filepath.Clean(filepath.Join(project, "../../tailscale/dist/tailscaled-private")), filepath.Join(project, "tailscaled.json"), "/opt/homebrew/opt/tailscale/bin/tailscale", filepath.Join(project, "companion.json")}
}

func usage() {
	fmt.Println(`Usage: ./tailscale-companion [start | routes | login | cli COMMAND ... | --dry-run]
  start       Supervise daemon, personal route, and split DNS (sudo); Ctrl-C cleans up.
  routes      Manage routes/DNS for the already-running daemon (sudo); leaves it running.
  login       Request browser login through the independent socket.
  cli ...     Run the Homebrew CLI through ONLY the independent socket.
  --dry-run   Check config/binary and show startup settings; no sudo or changes.
  install-service  Install root-owned copies and migrate to a system LaunchDaemon.
  service-status   Show launchd's service status.
  ui-status        Emit read-only JSON status for the menu-bar app.
  --print-plist    Print the LaunchDaemon definition without installing it.

Manager waits for personal login, discovers utun dynamically, and refuses conflicting
or unmanaged personal/service routes. Split DNS covers only the companion MagicDNS
suffix; IPv6 routes are not managed.`)
}

func daemonArgs(p paths) []string {
	return []string{"--private-no-routes", "--tun=utun", "--port=0", "--statedir=" + stateDir, "--socket=" + socketPath, "--config=" + p.config}
}

func validateConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var c struct {
		Version           string
		AcceptDNS         *bool `json:"acceptDNS"`
		AcceptRoutes      *bool `json:"acceptRoutes"`
		ExitNode          *string
		AdvertiseRoutes   *[]string
		AdvertiseExitNode *bool
		AutoUpdate        *struct{ Apply *bool }
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	isFalse := func(b *bool) bool { return b != nil && !*b }
	if c.Version != "alpha0" || !isFalse(c.AcceptDNS) || !isFalse(c.AcceptRoutes) ||
		c.ExitNode == nil || *c.ExitNode != "" || c.AdvertiseRoutes == nil || len(*c.AdvertiseRoutes) != 0 ||
		!isFalse(c.AdvertiseExitNode) || c.AutoUpdate == nil || !isFalse(c.AutoUpdate.Apply) {
		return errors.New("config must explicitly disable DNS, subnet acceptance/advertisement, exit nodes, and automatic update installation")
	}
	return nil
}

func acquireLock() (*os.File, error) {
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), lockPath)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another personal wrapper is active: %w", err)
	}
	// Never unlink this file: that could allow two processes to lock different inodes.
	return f, nil
}

func passthrough(args ...string) error {
	cmd := exec.Command(cliPath, append([]string{"--socket=" + socketPath}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func supervise(ctx context.Context, p paths, m *routeManager) error {
	if err := prepareSocket(); err != nil {
		return err
	}
	if info, err := os.Lstat(stateDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("state path is not a real directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	if err := os.Chown(stateDir, 0, 0); err != nil {
		return err
	}
	if err := os.Chmod(stateDir, 0700); err != nil {
		return err
	}

	cmd := exec.Command(p.daemon, daemonArgs(p)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Foreground mode handles terminal signals before stopping the child. Under
	// launchd keep the child in the job's process group so an unexpected parent
	// exit cannot leave an orphan daemon holding the state/socket indefinitely.
	if p.self != installedWrapper {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("Started private daemon PID %d. Run ./tailscale-companion login in another terminal.", cmd.Process.Pid)
	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	var childErr error
	go func() { childErr = cmd.Wait(); close(done); cancel() }()
	routeErr := m.watch(watchCtx)
	select {
	case <-done:
		if ctx.Err() == nil {
			return errors.Join(routeErr, childErr)
		}
	default:
		_ = cmd.Process.Signal(syscall.SIGTERM)
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			_ = cmd.Process.Kill()
			<-done
		}
	}
	return routeErr
}

func run(args []string) error {
	p, err := localPaths()
	if err != nil {
		return err
	}
	cliPath = p.cli
	action := "start"
	if len(args) != 0 {
		action, args = args[0], args[1:]
	}
	switch action {
	case "--help", "-h", "help":
		usage()
		return nil
	case "start", "routes", "login", "--dry-run", "install-service", "service-status", "ui-status", "--print-plist":
		if len(args) != 0 {
			return errors.New("unexpected arguments")
		}
	case "cli":
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			return errors.New("cli requires a subcommand, not global flags")
		}
		return passthrough(args...)
	default:
		return fmt.Errorf("unknown command %q", action)
	}
	if action == "ui-status" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return json.NewEncoder(os.Stdout).Encode(collectUIStatus(ctx))
	}
	if action == "--print-plist" {
		fmt.Print(servicePlist)
		return nil
	}
	if action == "service-status" {
		cmd := exec.Command("/bin/launchctl", "print", "system/"+serviceLabel)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		return cmd.Run()
	}
	if err := applySettings(p.settings); err != nil {
		return err
	}
	if action == "login" {
		hostname, err := configuredHostname(p.config)
		if err != nil {
			return err
		}
		fmt.Println("Authenticate with the configured companion tailnet: " + personalTailnet)
		return passthrough("up", "--accept-dns=false", "--accept-routes=false", "--exit-node=", "--hostname="+hostname)
	}
	if action != "routes" {
		if err := validateConfig(p.config); err != nil {
			return err
		}
		out, err := runCommand(context.Background(), p.daemon, "--help")
		if err != nil {
			return err
		}
		if !strings.Contains(string(out), "-private-no-routes") {
			return errors.New("binary lacks --private-no-routes")
		}
		if !strings.Contains(string(out), companionServiceIP) {
			return fmt.Errorf("binary lacks companion DNS service IP %s", companionServiceIP)
		}
	}
	if action == "--dry-run" {
		fmt.Printf("Daemon: %s\nArguments: %q\nManaged routes: %s, %s\nExpected tailnet: %s\n", p.daemon, daemonArgs(p), personalCIDR, servicePrefix, personalTailnet)
		fmt.Println("Requires sudo; monitors every 3s, discovers utun, cleans up on SIGINT/SIGTERM. No changes made.")
		return nil
	}
	if os.Geteuid() != 0 {
		return syscall.Exec("/usr/bin/sudo", []string{"sudo", "--", p.self, action}, os.Environ())
	}
	if action == "install-service" {
		return installService(p)
	}
	lock, err := acquireLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	m := newRouteManager()
	if action == "routes" {
		log.Println("Managing routes for existing personal daemon; Ctrl-C removes the owned route but leaves the daemon running.")
		return m.watch(ctx)
	}
	return supervise(ctx, p, m)
}

func main() {
	log.SetFlags(log.LstdFlags)
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
