//go:build darwin

// Package the Swift menu-bar app and optionally register it for the current login.
// No administrator privileges or daemon mutations are used by this installer.
package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const label = "io.github.zlobober.tailscale-companion.menubar"
const product = "TailscaleCompanionMenu"
const appName = "Tailscale Companion.app"

const infoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleIdentifier</key><string>io.github.zlobober.tailscale-companion.menubar</string>
  <key>CFBundleName</key><string>Tailscale Companion</string>
  <key>CFBundleDisplayName</key><string>Tailscale Companion</string>
  <key>CFBundleExecutable</key><string>TailscaleCompanionMenu</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.1.0</string>
  <key>CFBundleVersion</key><string>1</string>
  <key>LSMinimumSystemVersion</key><string>14.0</string>
  <key>LSUIElement</key><true/>
  <key>NSHighResolutionCapable</key><true/>
  <key>NSPrincipalClass</key><string>NSApplication</string>
</dict></plist>
`

func command(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func swiftTests(root string) error {
	args := []string{"test", "--package-path", filepath.Join(root, "macos"), "--disable-xctest"}
	// CLT installations can include Swift Testing outside SwiftPM's default
	// search paths. Full Xcode installations do not need this workaround.
	if out, err := exec.Command("/usr/bin/xcode-select", "-p").Output(); err == nil {
		developer := strings.TrimSpace(string(out))
		frameworks := filepath.Join(developer, "Library/Developer/Frameworks")
		if _, err := os.Stat(filepath.Join(frameworks, "Testing.framework")); err == nil {
			args = append(args, "-Xswiftc", "-F", "-Xswiftc", frameworks,
				"-Xlinker", "-F", "-Xlinker", frameworks,
				"-Xlinker", "-rpath", "-Xlinker", frameworks,
				"-Xlinker", "-rpath", "-Xlinker", filepath.Join(developer, "Library/Developer/usr/lib"))
		}
	}
	return command("/usr/bin/swift", args...)
}

func build(root string) (string, error) {
	pkg := filepath.Join(root, "macos")
	if err := command("/usr/bin/swift", "build", "--package-path", pkg, "-c", "release", "--product", product); err != nil {
		return "", err
	}
	out, err := exec.Command("/usr/bin/swift", "build", "--package-path", pkg, "-c", "release", "--show-bin-path").Output()
	if err != nil {
		return "", err
	}
	app := filepath.Join(root, "dist", appName)
	if err := os.RemoveAll(app); err != nil {
		return "", err
	}
	binDir := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", err
	}
	if err := command("/bin/cp", filepath.Join(strings.TrimSpace(string(out)), product), filepath.Join(binDir, product)); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(app, "Contents", "Info.plist"), []byte(infoPlist), 0644); err != nil {
		return "", err
	}
	if err := command("/usr/bin/plutil", "-lint", filepath.Join(app, "Contents", "Info.plist")); err != nil {
		return "", err
	}
	// Locally built app; ad-hoc signing does not imply notarization/distribution signing.
	if err := command("/usr/bin/codesign", "--force", "--sign", "-", "--identifier", label, app); err != nil {
		return "", err
	}
	return app, nil
}

func agentPlist(app, logs string) string {
	escape := func(s string) string { var b bytes.Buffer; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key><array><string>%s</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>LimitLoadToSessionType</key><string>Aqua</string>
  <key>ProcessType</key><string>Interactive</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, label, escape(filepath.Join(app, "Contents/MacOS", product)), escape(logs), escape(logs))
}

func stopExistingApp(binary string) error {
	out, err := exec.Command("/bin/ps", "-axo", "pid=,uid=,comm=").Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != strconv.Itoa(os.Getuid()) || strings.Join(f[2:], " ") != binary {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			return err
		}
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		deadline := time.Now().Add(10 * time.Second)
		for syscall.Kill(pid, 0) == nil {
			if time.Now().After(deadline) {
				return fmt.Errorf("existing menu app PID %d did not stop", pid)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

func install(app string) error {
	if os.Geteuid() == 0 {
		return errors.New("install the menu app as your normal user, not with sudo")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	apps := filepath.Join(home, "Applications")
	agents := filepath.Join(home, "Library/LaunchAgents")
	logs := filepath.Join(home, "Library/Logs/TailscaleCompanion")
	for _, dir := range []string{apps, agents, logs} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	destination := filepath.Join(apps, appName)
	plist := filepath.Join(agents, label+".plist")
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if exec.Command("/bin/launchctl", "print", domain+"/"+label).Run() == nil {
		if err := command("/bin/launchctl", "bootout", domain+"/"+label); err != nil {
			return err
		}
	}
	if err := stopExistingApp(filepath.Join(destination, "Contents/MacOS", product)); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(apps, ".tailscale-companion-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	stagedApp := filepath.Join(stage, appName)
	if err := command("/bin/cp", "-R", app, stagedApp); err != nil {
		return err
	}
	if err := command("/usr/bin/codesign", "--verify", "--strict", stagedApp); err != nil {
		return err
	}
	previous := filepath.Join(stage, "previous.app")
	if _, err := os.Lstat(destination); err == nil {
		if err := os.Rename(destination, previous); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(stagedApp, destination); err != nil {
		if _, statErr := os.Lstat(previous); statErr == nil {
			_ = os.Rename(previous, destination)
		}
		return err
	}
	if err := os.WriteFile(plist, []byte(agentPlist(destination, filepath.Join(logs, "menubar.log"))), 0644); err != nil {
		return err
	}
	if err := command("/usr/bin/plutil", "-lint", plist); err != nil {
		return err
	}
	// Register bundle identity without opening an extra instance.
	if err := command("/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister", "-f", destination); err != nil {
		return err
	}
	if err := command("/bin/launchctl", "enable", domain+"/"+label); err != nil {
		return err
	}
	if err := command("/bin/launchctl", "bootstrap", domain, plist); err != nil {
		return err
	}
	fmt.Printf("Installed and launched %s\nLogin agent: %s\n", destination, plist)
	return nil
}

func run() error {
	doInstall := flag.Bool("install", false, "install the app for this user and launch it now/at login")
	doTest := flag.Bool("test", false, "run Swift tests (handles Command Line Tools framework paths)")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "macos/Package.swift")); err != nil {
		return errors.New("run from the tailscale-companion repository root")
	}
	if *doTest {
		if err := swiftTests(root); err != nil {
			return err
		}
		if !*doInstall {
			return nil
		}
	}
	app, err := build(root)
	if err != nil {
		return err
	}
	fmt.Println("Built:", app)
	if *doInstall {
		return install(app)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
