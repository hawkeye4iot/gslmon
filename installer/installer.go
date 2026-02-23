// gslmon Self-Installer
// Copyright (C) 2026 GetSetLive Pvt Ltd
// Source Code Free to Distribute under GPL License
//
// Created: 2026-02-23
// Purpose: Standalone Go binary installer that auto-detects RAID configuration,
//          checks for Go compiler, installs dependencies, compiles gslmon from
//          embedded source, and deploys as a systemd service.
//
// Build: go build -ldflags="-s -w" -o gslmon-installer installer.go
// Usage: sudo ./gslmon-installer
//        sudo ./gslmon-installer --uninstall

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// ==================== Installation Paths ====================
const (
	installDir = "/usr/local/bin"
	configDir  = "/etc/gslmon"
	logDir     = "/var/log/gslmon"
	stateDir   = "/var/lib/gslmon"
	tmpDir     = "/var/lib/gslmon/tmp"
	pidDir     = "/var/run/gslmon"
)

// ==================== ANSI Colors ====================
const (
	colorRed    = "\033[0;31m"
	colorGreen  = "\033[0;32m"
	colorYellow = "\033[1;33m"
	colorBlue   = "\033[0;34m"
	colorCyan   = "\033[0;36m"
	colorReset  = "\033[0m"
)

func logInfo(msg string)  { fmt.Printf("%s[INFO]%s  %s\n", colorBlue, colorReset, msg) }
func logOK(msg string)    { fmt.Printf("%s[OK]%s    %s\n", colorGreen, colorReset, msg) }
func logWarn(msg string)  { fmt.Printf("%s[WARN]%s  %s\n", colorYellow, colorReset, msg) }
func logErr(msg string)   { fmt.Printf("%s[ERROR]%s %s\n", colorRed, colorReset, msg) }
func logStep(msg string)  { fmt.Printf("%s[STEP]%s  %s\n", colorCyan, colorReset, msg) }

// ==================== Command Execution ====================
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func readInput(prompt, defaultVal string) string {
	reader := bufio.NewReader(os.Stdin)
	if defaultVal != "" {
		fmt.Printf("%s [%s]: ", prompt, defaultVal)
	} else {
		fmt.Printf("%s: ", prompt)
	}
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)
	if input == "" {
		return defaultVal
	}
	return input
}

// ==================== OS Detection ====================
type OSInfo struct {
	ID      string
	Name    string
	PkgMgr  string
}

func detectOS() OSInfo {
	info := OSInfo{ID: "unknown", Name: "Unknown Linux", PkgMgr: "unknown"}

	data, err := os.ReadFile("/etc/os-release")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "ID=") {
				info.ID = strings.Trim(strings.SplitN(line, "=", 2)[1], "\"")
			}
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				info.Name = strings.Trim(strings.SplitN(line, "=", 2)[1], "\"")
			}
		}
	}

	// Detect package manager
	if commandExists("dnf") {
		info.PkgMgr = "dnf"
	} else if commandExists("yum") {
		info.PkgMgr = "yum"
	} else if commandExists("apt-get") {
		info.PkgMgr = "apt"
	} else if commandExists("zypper") {
		info.PkgMgr = "zypper"
	} else if commandExists("pacman") {
		info.PkgMgr = "pacman"
	}

	logInfo(fmt.Sprintf("Detected OS: %s (package manager: %s)", info.Name, info.PkgMgr))
	return info
}

// ==================== Dependency Installation ====================
func installPkg(osInfo OSInfo, pkgName string) error {
	var cmd string
	var args []string

	switch osInfo.PkgMgr {
	case "dnf":
		cmd = "dnf"
		args = []string{"install", "-y", pkgName}
	case "yum":
		cmd = "yum"
		args = []string{"install", "-y", pkgName}
	case "apt":
		cmd = "apt-get"
		args = []string{"install", "-y", pkgName}
	case "zypper":
		cmd = "zypper"
		args = []string{"install", "-y", pkgName}
	case "pacman":
		cmd = "pacman"
		args = []string{"-S", "--noconfirm", pkgName}
	default:
		return fmt.Errorf("unknown package manager — install %s manually", pkgName)
	}

	logStep(fmt.Sprintf("Installing %s...", pkgName))
	_, err := runCmd(cmd, args...)
	if err != nil {
		return fmt.Errorf("failed to install %s: %v", pkgName, err)
	}
	logOK(fmt.Sprintf("%s installed", pkgName))
	return nil
}

func ensureSmartmontools(osInfo OSInfo) {
	if commandExists("smartctl") {
		out, _ := runCmd("smartctl", "--version")
		logOK(fmt.Sprintf("smartmontools already installed (%s)", strings.Split(out, "\n")[0]))
		return
	}
	if err := installPkg(osInfo, "smartmontools"); err != nil {
		logErr(err.Error())
	}
}

func ensureMdadm(osInfo OSInfo) {
	if commandExists("mdadm") {
		out, _ := runCmd("mdadm", "--version")
		logOK(fmt.Sprintf("mdadm already installed (%s)", strings.Split(out, "\n")[0]))
		return
	}
	if err := installPkg(osInfo, "mdadm"); err != nil {
		logErr(err.Error())
	}
}

func ensureGo(osInfo OSInfo) {
	goVersion := "1.22.5"
	minVersion := "1.21"

	// Check existing Go
	if commandExists("go") {
		out, _ := runCmd("go", "version")
		re := regexp.MustCompile(`go(\d+\.\d+)`)
		if m := re.FindStringSubmatch(out); len(m) > 1 {
			if m[1] >= minVersion {
				logOK(fmt.Sprintf("Go %s already installed (>= %s required)", m[1], minVersion))
				return
			}
			logWarn(fmt.Sprintf("Go %s found but %s+ required. Upgrading...", m[1], minVersion))
		}
	}

	// Check /usr/local/go
	if _, err := os.Stat("/usr/local/go/bin/go"); err == nil {
		os.Setenv("PATH", "/usr/local/go/bin:"+os.Getenv("PATH"))
		out, _ := runCmd("/usr/local/go/bin/go", "version")
		re := regexp.MustCompile(`go(\d+\.\d+)`)
		if m := re.FindStringSubmatch(out); len(m) > 1 && m[1] >= minVersion {
			logOK(fmt.Sprintf("Go %s found at /usr/local/go", m[1]))
			return
		}
	}

	// Try package manager first
	var goPkg string
	switch osInfo.PkgMgr {
	case "apt":
		goPkg = "golang-go"
	case "dnf", "yum":
		goPkg = "golang"
	case "zypper", "pacman":
		goPkg = "go"
	}

	if goPkg != "" {
		if err := installPkg(osInfo, goPkg); err == nil {
			if commandExists("go") {
				out, _ := runCmd("go", "version")
				re := regexp.MustCompile(`go(\d+\.\d+)`)
				if m := re.FindStringSubmatch(out); len(m) > 1 && m[1] >= minVersion {
					logOK(fmt.Sprintf("Go %s installed from package manager", m[1]))
					return
				}
			}
		}
		logWarn("Package manager Go version too old. Installing from tarball...")
	}

	// Fallback: install from tarball
	arch := runtime.GOARCH
	tarball := fmt.Sprintf("go%s.linux-%s.tar.gz", goVersion, arch)
	url := fmt.Sprintf("https://go.dev/dl/%s", tarball)
	tmpFile := fmt.Sprintf("/tmp/gslmon_%s", tarball)

	logStep(fmt.Sprintf("Downloading Go %s for %s...", goVersion, arch))

	var dlCmd string
	var dlArgs []string
	if commandExists("wget") {
		dlCmd = "wget"
		dlArgs = []string{"-q", "-O", tmpFile, url}
	} else if commandExists("curl") {
		dlCmd = "curl"
		dlArgs = []string{"-fSL", "-o", tmpFile, url}
	} else {
		logErr("Neither wget nor curl found. Cannot download Go.")
		os.Exit(1)
	}

	if _, err := runCmd(dlCmd, dlArgs...); err != nil {
		logErr(fmt.Sprintf("Failed to download Go: %v", err))
		os.Exit(1)
	}

	logStep("Installing Go to /usr/local/go...")
	os.RemoveAll("/usr/local/go")
	if _, err := runCmd("tar", "-C", "/usr/local", "-xzf", tmpFile); err != nil {
		logErr(fmt.Sprintf("Failed to extract Go: %v", err))
		os.Exit(1)
	}
	os.Remove(tmpFile)

	os.Setenv("PATH", "/usr/local/go/bin:"+os.Getenv("PATH"))

	// Write profile.d entry
	os.WriteFile("/etc/profile.d/golang.sh", []byte("export PATH=/usr/local/go/bin:$PATH\n"), 0644)

	logOK(fmt.Sprintf("Go %s installed successfully", goVersion))
}

// ==================== RAID Detection ====================
type RAIDInfo struct {
	Type        string // "software", "hardware", "disks_only"
	ArrayDevice string
	ArrayName   string
	Level       string
	MountPoint  string
	MemberDisks []map[string]string
}

func detectRAID() RAIDInfo {
	logStep("Auto-detecting RAID configuration...")
	info := RAIDInfo{Type: "none"}

	// Check software RAID
	data, err := os.ReadFile("/proc/mdstat")
	if err == nil {
		re := regexp.MustCompile(`^(md\d+)\s`)
		for _, line := range strings.Split(string(data), "\n") {
			if m := re.FindStringSubmatch(line); len(m) > 1 {
				arrName := m[1]
				arrDev := "/dev/" + arrName

				detail, err := runCmd("mdadm", "--detail", arrDev)
				if err != nil {
					continue
				}

				info.Type = "software"
				info.ArrayName = arrName
				info.ArrayDevice = arrDev

				// Extract RAID level
				for _, dline := range strings.Split(detail, "\n") {
					if strings.Contains(dline, "Raid Level") {
						parts := strings.SplitN(dline, ":", 2)
						if len(parts) == 2 {
							info.Level = strings.TrimSpace(parts[1])
						}
					}
				}

				// Extract mount point
				mountOut, _ := runCmd("findmnt", "-n", "-o", "TARGET", arrDev)
				info.MountPoint = strings.TrimSpace(mountOut)
				if info.MountPoint == "" {
					info.MountPoint = "/data"
				}

				// Extract member disks
				for _, dline := range strings.Split(detail, "\n") {
					dline = strings.TrimSpace(dline)
					if strings.HasPrefix(dline, "/dev/") {
						fields := strings.Fields(dline)
						disk := fields[len(fields)-1]
						dname := filepath.Base(disk)
						info.MemberDisks = append(info.MemberDisks, map[string]string{
							"device": disk,
							"name":   dname,
						})
					}
				}

				logOK(fmt.Sprintf("Detected software RAID: %s (%s) on %s", arrDev, info.Level, info.MountPoint))
				break
			}
		}
	}

	// Check hardware RAID
	if info.Type == "none" && commandExists("smartctl") {
		scanOut, _ := runCmd("smartctl", "--scan")
		if strings.Contains(scanOut, "megaraid") {
			info.Type = "hardware"
			info.Level = "Hardware RAID (MegaRAID)"
			diskNum := 0
			for _, line := range strings.Split(scanOut, "\n") {
				if strings.Contains(line, "megaraid") {
					fields := strings.Fields(line)
					if len(fields) >= 3 {
						info.MemberDisks = append(info.MemberDisks, map[string]string{
							"device": fields[0],
							"type":   fields[2],
							"name":   fmt.Sprintf("Disk %d", diskNum),
						})
						diskNum++
					}
				}
			}
			logOK(fmt.Sprintf("Detected hardware RAID (MegaRAID) with %d disks", diskNum))
		}
	}

	// Fallback: individual disks
	if info.Type == "none" {
		info.Type = "disks_only"
		info.Level = "No RAID"
		logWarn("No RAID detected. Detecting individual disks for SMART monitoring...")

		lsblkOut, _ := runCmd("lsblk", "-dno", "NAME,TYPE")
		for _, line := range strings.Split(lsblkOut, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == "disk" {
				name := fields[0]
				if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "sr") {
					continue
				}
				info.MemberDisks = append(info.MemberDisks, map[string]string{
					"device": "/dev/" + name,
					"name":   name,
				})
			}
		}
		logInfo(fmt.Sprintf("Found %d disks for SMART monitoring", len(info.MemberDisks)))
	}

	return info
}

// ==================== Config Generation ====================
type GslmonConfig struct {
	Email   map[string]interface{} `json:"email"`
	RAID    map[string]interface{} `json:"raid"`
	Monitor map[string]interface{} `json:"monitoring"`
	LogFile   string `json:"log_file"`
	StateFile string `json:"state_file"`
	TmpDir    string `json:"tmp_dir"`
	PidFile   string `json:"pid_file"`
}

func generateConfig(raid RAIDInfo, smtpServer string, smtpPort int, emailFrom, emailTo, serverName string) {
	logStep("Generating configuration...")

	arrayDevice := ""
	arrayName := ""
	mountPoint := ""
	if raid.Type == "software" {
		arrayDevice = raid.ArrayDevice
		arrayName = raid.ArrayName
		mountPoint = raid.MountPoint
	}

	// Build member disks array
	var disks []map[string]string
	for _, d := range raid.MemberDisks {
		disk := map[string]string{"device": d["device"], "name": d["name"]}
		if t, ok := d["type"]; ok && t != "" {
			disk["type"] = t
		}
		disks = append(disks, disk)
	}

	cfg := GslmonConfig{
		Email: map[string]interface{}{
			"smtp_server": smtpServer,
			"smtp_port":   smtpPort,
			"from":        emailFrom,
			"to":          emailTo,
			"server_name": serverName,
		},
		RAID: map[string]interface{}{
			"array_device": arrayDevice,
			"array_name":   arrayName,
			"raid_level":   raid.Level,
			"mount_point":  mountPoint,
			"member_disks": disks,
		},
		Monitor: map[string]interface{}{
			"log_check_interval_seconds":    30,
			"mdstat_check_interval_seconds": 3600,
			"smart_test_interval_days":      2,
			"smart_check_interval_hours":    6,
			"rebuild_check_interval_minutes": 30,
			"alert_cooldown_minutes":        15,
			"log_patterns": []string{
				"DID_BAD_TARGET", "I/O error", "degraded", "Disk failure",
				"faulty", "removed", "hardreset", "frozen", "NCQ",
				"ata[0-9].*error", "ata[0-9].*exception", "md/raid",
				"super_written", "journal abort", "EXT4-fs.*error",
				"SMART.*error", "Reallocated", "Uncorrectable",
				"Current_Pending", "read error", "write error",
				"recovering", "disable device", "FPDMA",
				"hard resetting link", "link is slow",
			},
			"smart_critical_attribute_ids": []int{5, 10, 171, 172, 184, 187, 188, 197, 198, 199, 201},
		},
		LogFile:   logDir + "/gslmon.log",
		StateFile: stateDir + "/state.json",
		TmpDir:    tmpDir,
		PidFile:   pidDir + "/gslmon.pid",
	}

	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		logErr(fmt.Sprintf("Failed to marshal config: %v", err))
		os.Exit(1)
	}

	configPath := configDir + "/config.json"
	if err := os.WriteFile(configPath, data, 0640); err != nil {
		logErr(fmt.Sprintf("Failed to write config: %v", err))
		os.Exit(1)
	}

	logOK(fmt.Sprintf("Configuration written to %s", configPath))
}

// ==================== Compile and Install ====================
func compileAndInstall() {
	logStep("Compiling gslmon...")

	// Find main.go
	sourceFile := ""
	candidates := []string{"./main.go", "../main.go"}

	execPath, _ := os.Executable()
	if execPath != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(execPath), "..", "main.go"))
	}

	for _, c := range candidates {
		abs, _ := filepath.Abs(c)
		if _, err := os.Stat(abs); err == nil {
			sourceFile = abs
			break
		}
	}

	if sourceFile == "" {
		logErr("Cannot find main.go. Run from the gslmon repository root or installer/ directory.")
		os.Exit(1)
	}

	logInfo(fmt.Sprintf("Source: %s", sourceFile))

	// Create temp build directory
	buildDir, err := os.MkdirTemp("", "gslmon-build-")
	if err != nil {
		logErr(fmt.Sprintf("Failed to create temp dir: %v", err))
		os.Exit(1)
	}
	defer os.RemoveAll(buildDir)

	// Copy source
	data, _ := os.ReadFile(sourceFile)
	os.WriteFile(filepath.Join(buildDir, "main.go"), data, 0644)

	// Init module and build
	goCmd := "go"
	if _, err := os.Stat("/usr/local/go/bin/go"); err == nil {
		goCmd = "/usr/local/go/bin/go"
	}

	cmd := exec.Command(goCmd, "mod", "init", "gslmon")
	cmd.Dir = buildDir
	cmd.Run()

	cmd = exec.Command(goCmd, "build", "-ldflags=-s -w", "-o", "gslmon", "main.go")
	cmd.Dir = buildDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		logErr(fmt.Sprintf("Compilation failed: %v", err))
		os.Exit(1)
	}

	logOK("Compilation successful")

	// Install binary
	builtBinary := filepath.Join(buildDir, "gslmon")
	destBinary := filepath.Join(installDir, "gslmon")

	binData, _ := os.ReadFile(builtBinary)
	if err := os.WriteFile(destBinary, binData, 0755); err != nil {
		logErr(fmt.Sprintf("Failed to install binary: %v", err))
		os.Exit(1)
	}

	logOK(fmt.Sprintf("Binary installed to %s", destBinary))
}

// ==================== Service Installation ====================
func installService() {
	logStep("Installing systemd service...")

	serviceContent := fmt.Sprintf(`# gslmon - RAID & Disk Health Monitor Daemon
# Copyright (C) 2026 GetSetLive Pvt Ltd
# Source Code Free to Distribute under GPL License
# Auto-generated by gslmon installer

[Unit]
Description=GSL RAID & Disk Health Monitor Daemon
After=network-online.target mdmonitor.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s/gslmon %s/config.json
WorkingDirectory=%s
Restart=always
RestartSec=15
StandardOutput=journal
StandardError=journal
SyslogIdentifier=gslmon
NoNewPrivileges=no
ProtectSystem=false
ProtectHome=false

[Install]
WantedBy=multi-user.target
`, installDir, configDir, stateDir)

	if err := os.WriteFile("/etc/systemd/system/gslmon.service", []byte(serviceContent), 0644); err != nil {
		logErr(fmt.Sprintf("Failed to write service file: %v", err))
		os.Exit(1)
	}

	runCmd("systemctl", "daemon-reload")
	logOK("Service installed as gslmon.service")
}

// ==================== Uninstall ====================
func doUninstall() {
	logStep("Uninstalling gslmon...")
	runCmd("systemctl", "stop", "gslmon")
	runCmd("systemctl", "disable", "gslmon")
	os.Remove("/etc/systemd/system/gslmon.service")
	runCmd("systemctl", "daemon-reload")
	os.Remove(filepath.Join(installDir, "gslmon"))
	os.RemoveAll(logDir)
	os.RemoveAll(stateDir)
	os.RemoveAll(pidDir)
	logWarn(fmt.Sprintf("Configuration preserved at %s/config.json (remove manually if needed)", configDir))
	logOK("gslmon uninstalled successfully")
}

// ==================== Main ====================
func main() {
	fmt.Printf("\n%s========================================%s\n", colorCyan, colorReset)
	fmt.Printf("%s  gslmon Installer%s\n", colorCyan, colorReset)
	fmt.Printf("%s  RAID & Disk Health Monitor Daemon%s\n", colorCyan, colorReset)
	fmt.Printf("%s  Copyright (C) 2026 GetSetLive Pvt Ltd%s\n", colorCyan, colorReset)
	fmt.Printf("%s========================================%s\n\n", colorCyan, colorReset)

	// Check root
	if os.Geteuid() != 0 {
		logErr("This installer must be run as root (sudo ./gslmon-installer)")
		os.Exit(1)
	}

	// Handle --uninstall
	if len(os.Args) > 1 && os.Args[1] == "--uninstall" {
		doUninstall()
		return
	}

	// Check if already running
	out, _ := runCmd("systemctl", "is-active", "gslmon")
	if strings.TrimSpace(out) == "active" {
		logWarn("gslmon is already running!")
		answer := readInput("Reinstall? This will stop the current instance. [y/N]", "N")
		if !strings.HasPrefix(strings.ToUpper(answer), "Y") {
			logInfo("Installation cancelled")
			return
		}
		runCmd("systemctl", "stop", "gslmon")
	}

	// Phase 1: Dependencies
	logStep("=== Phase 1: Dependencies ===")
	osInfo := detectOS()
	ensureSmartmontools(osInfo)
	ensureMdadm(osInfo)
	ensureGo(osInfo)

	// Phase 2: RAID Detection
	logStep("=== Phase 2: RAID Detection ===")
	raid := detectRAID()

	// Phase 3: Email Configuration
	logStep("=== Phase 3: Email Configuration ===")
	hostname, _ := os.Hostname()

	fmt.Printf("\n%s========================================%s\n", colorCyan, colorReset)
	fmt.Printf("%s  Email Alert Configuration%s\n", colorCyan, colorReset)
	fmt.Printf("%s========================================%s\n\n", colorCyan, colorReset)

	smtpServer := readInput("SMTP server hostname", "localhost")
	smtpPortStr := readInput("SMTP port", "25")
	smtpPort := 25
	fmt.Sscanf(smtpPortStr, "%d", &smtpPort)

	emailFrom := readInput("Sender email (from)", fmt.Sprintf("gslmon@%s", hostname))
	emailTo := readInput("Alert recipient email (to)", "")
	for emailTo == "" {
		logErr("Recipient email is required")
		emailTo = readInput("Alert recipient email (to)", "")
	}
	serverName := readInput("Server name for email subjects", hostname)

	// Phase 4: Install
	logStep("=== Phase 4: Installation ===")

	// Create directories
	for _, dir := range []string{configDir, logDir, stateDir, tmpDir, pidDir} {
		os.MkdirAll(dir, 0755)
	}
	logOK("Directories created")

	generateConfig(raid, smtpServer, smtpPort, emailFrom, emailTo, serverName)
	compileAndInstall()
	installService()

	// Summary
	fmt.Printf("\n%s========================================%s\n", colorGreen, colorReset)
	fmt.Printf("%s  gslmon Installation Complete%s\n", colorGreen, colorReset)
	fmt.Printf("%s========================================%s\n\n", colorGreen, colorReset)

	fmt.Printf("  Binary:      %s/gslmon\n", installDir)
	fmt.Printf("  Config:      %s/config.json\n", configDir)
	fmt.Printf("  Log file:    %s/gslmon.log\n", logDir)
	fmt.Printf("  Service:     gslmon.service\n\n")

	if raid.Type == "software" {
		fmt.Printf("  RAID:        %s (%s) on %s\n", raid.ArrayDevice, raid.Level, raid.MountPoint)
	} else if raid.Type == "hardware" {
		fmt.Printf("  RAID:        %s\n", raid.Level)
	} else {
		fmt.Printf("  RAID:        None detected (SMART monitoring only)\n")
	}
	fmt.Printf("  Email:       %s -> %s via %s:%d\n\n", emailFrom, emailTo, smtpServer, smtpPort)

	fmt.Printf("  %sCommands:%s\n", colorCyan, colorReset)
	fmt.Printf("    sudo systemctl start gslmon      # Start the daemon\n")
	fmt.Printf("    sudo systemctl enable gslmon     # Enable auto-start\n")
	fmt.Printf("    sudo systemctl status gslmon     # Check status\n")
	fmt.Printf("    tail -f %s/gslmon.log    # Follow logs\n\n", logDir)

	answer := readInput("Start gslmon now? [Y/n]", "Y")
	if strings.HasPrefix(strings.ToUpper(answer), "Y") {
		runCmd("systemctl", "enable", "gslmon")
		runCmd("systemctl", "start", "gslmon")

		out, _ := runCmd("systemctl", "is-active", "gslmon")
		if strings.TrimSpace(out) == "active" {
			logOK(fmt.Sprintf("gslmon is running! Startup health email sent to %s", emailTo))
		} else {
			logErr("gslmon failed to start. Check: journalctl -u gslmon -n 20")
		}
	} else {
		logInfo("To start later: sudo systemctl enable --now gslmon")
	}
}
