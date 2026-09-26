//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	serviceLabel      = "me.zlobober.tailscale-personal"
	serviceDir        = "/usr/local/libexec/tailscale-personal"
	installedWrapper  = serviceDir + "/tailscale-companion"
	installedDaemon   = serviceDir + "/tailscaled-private"
	installedCLI      = serviceDir + "/tailscale"
	configDir         = "/Library/Application Support/TailscalePersonal"
	installedConfig   = configDir + "/tailscaled.json"
	installedSettings = configDir + "/companion.json"
	plistPath         = "/Library/LaunchDaemons/" + serviceLabel + ".plist"
	serviceLog        = "/var/log/tailscale-personal.log"
)

const servicePlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>me.zlobober.tailscale-personal</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/libexec/tailscale-personal/tailscale-companion</string>
    <string>start</string>
  </array>
  <key>UserName</key><string>root</string>
  <key>GroupName</key><string>wheel</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>ExitTimeOut</key><integer>45</integer>
  <key>ProcessType</key><string>Background</string>
  <key>AbandonProcessGroup</key><false/>
  <key>Umask</key><integer>63</integer>
  <key>WorkingDirectory</key><string>/usr/local/libexec/tailscale-personal</string>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
  <key>StandardOutPath</key><string>/var/log/tailscale-personal.log</string>
  <key>StandardErrorPath</key><string>/var/log/tailscale-personal.log</string>
</dict>
</plist>
`

// Safe restart after an unclean socket shutdown. Never remove a live listener,
// a non-socket, or a socket belonging to another user. Called under wrapper lock.
func prepareSocket() error {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		return fmt.Errorf("refusing non-root/non-socket path %s", socketPath)
	}
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err == nil {
		conn.Close()
		return fmt.Errorf("personal socket is live; stop that daemon or use routes mode: %s", socketPath)
	}
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cannot establish socket is stale: %w", err)
	}
	log.Printf("Removing stale root-owned socket %s", socketPath)
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func secureDir(path string) error {
	info, err := os.Lstat(path)
	if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("refusing non-directory/symlink %s", path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	if err := os.Chown(path, 0, 0); err != nil {
		return err
	}
	return os.Chmod(path, 0755)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular source file: %s", src)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}

type oldProcess struct {
	pid    int
	daemon bool
}

// Match only the known private executables and explicit personal socket/state.
// In particular, never signal sudo parents, the App Store app, or a stock daemon.
func parseOldProcesses(output string, p paths) ([]oldProcess, error) {
	var result []oldProcess
	for _, line := range strings.Split(output, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			return nil, err
		}
		if f[1] != "0" || pid == os.Getpid() {
			continue
		}
		binary := filepath.Clean(f[2])
		if (binary == p.self || binary == installedWrapper) && (f[3] == "routes" || f[3] == "start") {
			result = append(result, oldProcess{pid, false})
		}
		if binary == p.daemon || binary == installedDaemon {
			hasSocket, hasState := false, false
			for _, arg := range f[3:] {
				if arg == "--socket="+socketPath {
					hasSocket = true
				}
				if arg == "--statedir="+stateDir {
					hasState = true
				}
			}
			if hasSocket && hasState {
				result = append(result, oldProcess{pid, true})
			}
		}
	}
	return result, nil
}

func stopOldProcesses(p paths) error {
	out, err := runCommand(context.Background(), "/bin/ps", "-axo", "pid=,uid=,command=")
	if err != nil {
		return err
	}
	processes, err := parseOldProcesses(string(out), p)
	if err != nil {
		return err
	}
	// Route supervisor goes first, allowing it to remove its route before utun exits.
	for _, daemon := range []bool{false, true} {
		for _, proc := range processes {
			if proc.daemon != daemon {
				continue
			}
			// Revalidate the PID's identity immediately before signaling it.
			current, err := runCommand(context.Background(), "/bin/ps", "-axo", "pid=,uid=,command=")
			if err != nil {
				return err
			}
			matches, err := parseOldProcesses(string(current), p)
			if err != nil {
				return err
			}
			found := false
			for _, match := range matches {
				if match == proc {
					found = true
				}
			}
			if !found {
				continue
			}
			log.Printf("Stopping old personal process PID %d", proc.pid)
			if err := syscall.Kill(proc.pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
			deadline := time.Now().Add(35 * time.Second)
			for syscall.Kill(proc.pid, 0) == nil {
				if time.Now().After(deadline) {
					return fmt.Errorf("PID %d did not stop gracefully; refusing concurrent startup", proc.pid)
				}
				time.Sleep(200 * time.Millisecond)
			}
		}
	}
	return nil
}

func launchctl(args ...string) error {
	cmd := exec.Command("/bin/launchctl", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func installService(p paths) error {
	for _, dir := range []string{serviceDir, configDir} {
		if err := secureDir(dir); err != nil {
			return err
		}
	}
	stage, err := os.MkdirTemp(serviceDir, ".install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	files := []struct {
		source, target string
		mode           os.FileMode
	}{
		{p.self, installedWrapper, 0755}, {p.daemon, installedDaemon, 0755},
		{p.cli, installedCLI, 0755}, {p.config, installedConfig, 0644},
		{p.settings, installedSettings, 0644},
	}
	// Copy and validate everything before stopping the running service.
	for _, f := range files {
		if err := copyFile(f.source, filepath.Join(stage, filepath.Base(f.target)), f.mode); err != nil {
			return err
		}
	}
	stagedPlist := filepath.Join(stage, "service.plist")
	if err := os.WriteFile(stagedPlist, []byte(servicePlist), 0644); err != nil {
		return err
	}
	if _, err := runCommand(context.Background(), "/usr/bin/plutil", "-lint", stagedPlist); err != nil {
		return err
	}
	if err := validateConfig(filepath.Join(stage, "tailscaled.json")); err != nil {
		return err
	}
	if _, err := readSettings(filepath.Join(stage, "companion.json")); err != nil {
		return err
	}
	if _, err := runCommand(context.Background(), filepath.Join(stage, "tailscale"), "version"); err != nil {
		return err
	}

	if exec.Command("/bin/launchctl", "print", "system/"+serviceLabel).Run() == nil {
		if err := launchctl("bootout", "system/"+serviceLabel); err != nil {
			return err
		}
	}
	if err := stopOldProcesses(p); err != nil {
		return err
	}
	lock, err := acquireLock()
	if err != nil {
		return err
	}
	// This installer must release the lock before launchd starts the new supervisor.
	defer lock.Close()
	if err := prepareSocket(); err != nil {
		return err
	}

	// Preserve the same state location; keep a one-time root-only backup of its key file.
	stateFile := filepath.Join(stateDir, "tailscaled.state")
	backup := stateFile + ".pre-launchd"
	if _, err := os.Stat(stateFile); err == nil {
		if _, err := os.Lstat(backup); errors.Is(err, os.ErrNotExist) {
			if err := copyFile(stateFile, backup, 0600); err != nil {
				return err
			}
		}
	}
	for _, f := range files {
		source := filepath.Join(stage, filepath.Base(f.target))
		// Rename avoids following pre-existing destination symlinks.
		if err := os.Rename(source, f.target); err != nil {
			return err
		}
		if err := os.Chmod(f.target, f.mode); err != nil {
			return err
		}
	}
	if err := os.Rename(stagedPlist, plistPath); err != nil {
		return err
	}
	if err := os.Chmod(plistPath, 0644); err != nil {
		return err
	}
	fd, err := syscall.Open(serviceLog, syscall.O_CREAT|syscall.O_APPEND|syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0640)
	if err != nil {
		return err
	}
	if err := syscall.Fchown(fd, 0, 0); err != nil {
		syscall.Close(fd)
		return err
	}
	if err := syscall.Fchmod(fd, 0640); err != nil {
		syscall.Close(fd)
		return err
	}
	syscall.Close(fd)
	if err := lock.Close(); err != nil {
		return err
	}
	if err := launchctl("enable", "system/"+serviceLabel); err != nil {
		return err
	}
	if err := launchctl("bootstrap", "system", plistPath); err != nil {
		return err
	}
	log.Printf("LaunchDaemon installed. State retained at %s; logs: %s", stateDir, serviceLog)

	cliPath = installedCLI
	m := newRouteManager()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		iface, services, err := m.desiredRoutes(context.Background())
		if err == nil {
			routes, err := m.snapshot(context.Background())
			if err == nil {
				poolReady, serviceReady := false, false
				readyVIPs := make(map[netip.Prefix]bool)
				for _, r := range routes {
					poolReady = poolReady || r.prefix == pool && r.iface == iface
					serviceReady = serviceReady || r.prefix == servicePrefix && r.iface == iface
					if r.iface == iface {
						readyVIPs[r.prefix] = true
					}
				}
				allVIPsReady := true
				for _, service := range services {
					allVIPsReady = allVIPsReady && readyVIPs[service.prefix]
				}
				resolverPath, resolverErr := m.resolverPath()
				resolverData, readErr := os.ReadFile(resolverPath)
				if poolReady && serviceReady && allVIPsReady && resolverErr == nil && readErr == nil && string(resolverData) == string(resolverContents()) {
					log.Printf("Verified personal tailnet, base routes and %d Service route(s) through %s, and split DNS", len(services), iface)
					return nil
				}
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("service installed but routing not ready within 45 seconds; inspect %s and launchctl print system/%s", serviceLog, serviceLabel)
}
