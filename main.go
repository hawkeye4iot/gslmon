// gslmon - RAID & Disk Health Monitor Daemon
// Copyright (C) 2026 GetSetLive Pvt Ltd
// Source Code Free to Distribute under GPL License
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.
//
// Created: 2026-02-11
// Purpose: Monitors software RAID (mdadm) and hardware RAID (megaraid) array health
//          via dmesg, journalctl, /proc/mdstat, and SMART data.
//          Sends HTML email alerts for suspicious activity and periodic SMART test reports.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/smtp"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ==================== Configuration Types ====================

type Config struct {
	Email   EmailConfig   `json:"email"`
	RAID    RAIDConfig    `json:"raid"`
	Monitor MonitorConfig `json:"monitoring"`
	LogFile   string      `json:"log_file"`
	StateFile string      `json:"state_file"`
	TmpDir    string      `json:"tmp_dir"`
	PidFile   string      `json:"pid_file"`
}

type EmailConfig struct {
	SMTPServer string `json:"smtp_server"`
	SMTPPort   int    `json:"smtp_port"`
	From       string `json:"from"`
	To         string `json:"to"`
	ServerName string `json:"server_name"`
}

type RAIDConfig struct {
	ArrayDevice string       `json:"array_device"`
	ArrayName   string       `json:"array_name"`
	RAIDLevel   string       `json:"raid_level"`
	MountPoint  string       `json:"mount_point"`
	MemberDisks []DiskConfig `json:"member_disks"`
}

// DiskConfig describes a monitored disk for SMART health checks.
// Created: 2026-02-12 — Supports both direct and megaraid pass-through SMART access
type DiskConfig struct {
	Device string `json:"device"` // Device path: /dev/sdc or /dev/bus/0
	Type   string `json:"type"`   // SMART device type: "" for direct, "megaraid,0" for HW RAID
	Name   string `json:"name"`   // Display name: "sdc" or "Disk 0"
}

type MonitorConfig struct {
	LogCheckIntervalSec      int      `json:"log_check_interval_seconds"`
	MdstatCheckIntervalSec   int      `json:"mdstat_check_interval_seconds"`
	SmartTestIntervalDays    int      `json:"smart_test_interval_days"`
	SmartCheckIntervalHrs    int      `json:"smart_check_interval_hours"`
	RebuildCheckIntervalMin  int      `json:"rebuild_check_interval_minutes"`
	AlertCooldownMin         int      `json:"alert_cooldown_minutes"`
	LogPatterns              []string `json:"log_patterns"`
	LogExcludePatterns       []string `json:"log_exclude_patterns"`
	SmartCriticalIDs         []int    `json:"smart_critical_attribute_ids"`
}

// ==================== State Management ====================

type State struct {
	LastLogCheck      string `json:"last_log_check"`
	LastMdstat        string `json:"last_mdstat_state"`
	LastSmartTest     string `json:"last_smart_test"`
	SmartTestActive   bool   `json:"smart_test_active"`
	RebuildWasActive  bool   `json:"rebuild_was_active"`
	LastRebuildPct    string `json:"last_rebuild_pct"`
	LastAlertTimes    map[string]string `json:"last_alert_times"`
	mu                sync.Mutex
}

// ==================== SMART Parsing Types ====================

type SmartDiskReport struct {
	Device       string
	Model        string
	Serial       string
	Health       string
	Attributes   []SmartAttribute
	SelfTests    []SmartTestEntry
	RawOutput    string
	Command      string
	HasIssues    bool
	Issues       []string
}

type SmartAttribute struct {
	ID        int
	Name      string
	Value     int
	Worst     int
	Threshold int
	RawValue  string
	Failed    bool
	Critical  bool
}

type SmartTestEntry struct {
	Num       string
	Type      string
	Status    string
	Remaining string
	Lifetime  string
	LBA       string
	HasError  bool
}

// ==================== Log Alert Types ====================

type LogAlert struct {
	Timestamp   string
	RawLine     string
	Pattern     string
	Explanation string
	Severity    string
}

type MdstatInfo struct {
	ArrayState  string
	DiskStatus  string
	ActiveDisks int
	TotalDisks  int
	Rebuild     string
	RebuildPct  string
	RebuildETA  string
	RebuildSpd  string
	IsRebuilding bool
	RawOutput   string
}

// HWRaidRebuildInfo holds rebuild progress data from perccli64/storcli64 for hardware RAID controllers.
// Created: 2026-02-28 — Parallel to MdstatInfo for hardware RAID (Dell PERC) rebuild monitoring
type HWRaidRebuildInfo struct {
	DriveID      string  // e.g. "/c0/e32/s3"
	DriveName    string  // e.g. "Disk 3" (from config member_disks)
	RebuildPct   string  // e.g. "20%"
	IsRebuilding bool
	RawOutput    string  // full perccli64 output for email body
}

// ==================== Globals ====================

var (
	cfg    Config
	state  State
	logger *log.Logger
	hwRaidCLI string // Runtime-detected path to perccli64 or storcli64

	// Error explanations keyed by pattern
	errorExplanations = map[string]string{
		"DID_BAD_TARGET":       "SCSI host byte indicating the SATA target device is not responding. The drive may have a cable, power, or firmware issue.",
		"I/O error":            "Input/Output error — data could not be read from or written to the disk. May indicate drive failure or controller issue.",
		"degraded":             "RAID array operating with fewer active disks than configured. Redundancy is reduced; another disk failure risks data loss.",
		"Disk failure":         "mdadm has detected a disk failure. The disk is being removed from the array.",
		"faulty":               "A RAID member disk has been marked as faulty and removed from active service in the array.",
		"removed":              "A disk has been removed from the RAID array. It is no longer participating in data storage or redundancy.",
		"hardreset":            "Kernel is performing a hard reset on the SATA link. This occurs when a drive stops responding to commands.",
		"frozen":               "The drive's NCQ (Native Command Queuing) queue has frozen. Pending I/O commands cannot complete.",
		"NCQ":                  "Native Command Queuing error. The drive's command queue encountered an issue processing parallel I/O requests.",
		"ata[0-9].*error":      "ATA subsystem error detected on a SATA device. May indicate drive communication or hardware issues.",
		"ata[0-9].*exception":  "ATA exception on a SATA device. The drive encountered an abnormal condition requiring error handling.",
		"md/raid":              "MD RAID subsystem event. A significant change occurred in the software RAID layer.",
		"super_written":        "Error writing the RAID superblock to a member disk. The disk may be failing or unreachable.",
		"journal abort":        "The ext4 filesystem journal has been aborted due to I/O errors. The filesystem may be remounted read-only.",
		"EXT4-fs.*error":       "ext4 filesystem error detected. Usually caused by underlying disk I/O failures affecting the storage device.",
		"SMART.*error":         "SMART subsystem reported an error on a drive. The drive's self-monitoring has detected an issue.",
		"Reallocated":          "Drive has remapped bad sectors to spare area. High or increasing counts indicate drive degradation.",
		"Uncorrectable":        "Uncorrectable read errors detected. Data in affected sectors could not be recovered by the drive's ECC.",
		"Current_Pending":      "Sectors pending reallocation. These sectors produced read errors and will be remapped on next successful write.",
		"read error":           "A read operation failed on the device. May indicate bad sectors or drive failure.",
		"write error":          "A write operation failed on the device. May indicate drive failure or filesystem issues.",
		"recovering":           "RAID array is rebuilding data onto a new or re-added disk. Performance may be reduced during recovery.",
		"disable device":       "Kernel has completely disabled a SATA device after exhausting all recovery attempts. The drive is offline.",
		"FPDMA":                "First-party DMA (FPDMA) command timeout. NCQ read/write commands did not complete in time.",
		"hard resetting link":  "Kernel is performing a hard reset on the SATA physical link to attempt drive recovery.",
		"link is slow":         "SATA link is responding slowly. The kernel is waiting for the drive to become ready after a reset.",
		// Pre-degradation warning patterns — added 2026-03-30
		// Purpose: Detect early symptoms of disk failure before RAID enters degraded state
		"exception Emask":             "ATA exception with error mask — indicates the drive reported an abnormal condition. Often the first sign before NCQ freeze and drive removal.",
		"link is slow to respond":     "SATA link responding slowly during reset. The drive is struggling to re-establish communication — a pre-failure warning sign.",
		"reset failed":                "Hard reset on the SATA link failed completely. The kernel could not recover the drive — imminent disk loss.",
		"Buffer I/O error":            "Block layer I/O failure — the kernel could not complete a read/write at the block device level. Indicates underlying disk or controller failure.",
		"Remounting filesystem read-only": "Filesystem forced into read-only mode due to I/O errors. Data writes are no longer possible — immediate attention required.",
		"sector.*error":               "Sector-level error detected on a storage device. May indicate media degradation or failing drive firmware.",
		"medium error":                "SCSI medium error — the drive's physical media returned an unrecoverable error. Bad sectors or surface damage suspected.",
		"task abort":                  "SATA command was aborted by the drive. The drive rejected or could not process the I/O request — may indicate firmware hang or failure.",
		"revalidation failed":         "Drive revalidation failed after a SATA link reset. The kernel could not re-identify the device — the drive may be offline or dying.",
		"lost page write":             "Write data lost due to I/O failure. Written data did not reach the disk — potential data corruption and drive failure indicator.",
		"Input/output error":          "Generic I/O error from userspace or kernel subsystem. Indicates a storage device could not complete the requested operation.",
		"SATA link down":              "SATA physical link has gone down. The drive is no longer electrically connected or responding on the SATA bus.",
	}
)

// ==================== Configuration Loading ====================

// loadConfig reads and parses the JSON configuration file.
// Created: 2026-02-11 — Loads all monitoring parameters from config.json
func loadConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse config file: %w", err)
	}
	return nil
}

// ==================== State Persistence ====================

// loadState reads the daemon state from disk for persistence across restarts.
// Created: 2026-02-11 — Tracks last check times, mdstat state, SMART test status
func loadState() {
	state.mu.Lock()
	defer state.mu.Unlock()

	data, err := os.ReadFile(cfg.StateFile)
	if err != nil {
		logger.Printf("No existing state file, initializing fresh state")
		state.LastLogCheck = time.Now().Add(-1 * time.Minute).Format(time.RFC3339)
		state.LastMdstat = ""
		state.LastSmartTest = time.Now().Add(-24 * time.Duration(cfg.Monitor.SmartTestIntervalDays) * time.Hour).Format(time.RFC3339)
		state.SmartTestActive = false
		state.LastAlertTimes = make(map[string]string)
		return
	}
	if err := json.Unmarshal(data, &state); err != nil {
		logger.Printf("Failed to parse state file, reinitializing: %v", err)
		state.LastLogCheck = time.Now().Add(-1 * time.Minute).Format(time.RFC3339)
		state.LastMdstat = ""
		state.LastSmartTest = time.Now().Add(-24 * time.Duration(cfg.Monitor.SmartTestIntervalDays) * time.Hour).Format(time.RFC3339)
		state.SmartTestActive = false
		state.LastAlertTimes = make(map[string]string)
	}
	if state.LastAlertTimes == nil {
		state.LastAlertTimes = make(map[string]string)
	}
}

// saveState persists the daemon state to disk.
// Created: 2026-02-11 — Ensures state survives daemon restarts
func saveState() {
	state.mu.Lock()
	defer state.mu.Unlock()

	data, err := json.MarshalIndent(state, "", "    ")
	if err != nil {
		logger.Printf("Failed to marshal state: %v", err)
		return
	}
	tmpFile := cfg.StateFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0644); err != nil {
		logger.Printf("Failed to write state file: %v", err)
		return
	}
	if err := os.Rename(tmpFile, cfg.StateFile); err != nil {
		logger.Printf("Failed to rename state file: %v", err)
	}
}

// ==================== Command Execution ====================

// runCommand executes a shell command and returns stdout, stderr, and error.
// Created: 2026-02-11 — Wrapper for executing system commands with timeout
func runCommand(name string, args ...string) (string, string, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// runSmartctl executes a smartctl command with optional device type flag for megaraid pass-through.
// Created: 2026-02-12 — Supports both direct disk and hardware RAID controller SMART access
func runSmartctl(args []string, disk DiskConfig) (string, string, error) {
	fullArgs := make([]string, len(args))
	copy(fullArgs, args)
	if disk.Type != "" {
		fullArgs = append(fullArgs, "-d", disk.Type)
	}
	fullArgs = append(fullArgs, disk.Device)
	return runCommand("smartctl", fullArgs...)
}

// ==================== Email Sending ====================

// sendEmail connects to the configured SMTP server and sends an HTML email.
// Created: 2026-02-11 — Delivers HTML-formatted alert and report emails
func sendEmail(subject, htmlBody string) error {
	addr := fmt.Sprintf("%s:%d", cfg.Email.SMTPServer, cfg.Email.SMTPPort)

	headers := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=UTF-8\r\nX-Mailer: gslmon/1.1\r\nDate: %s\r\n\r\n",
		cfg.Email.From,
		cfg.Email.To,
		subject,
		time.Now().Format(time.RFC1123Z),
	)

	msg := headers + htmlBody

	c, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("SMTP dial failed: %w", err)
	}
	defer c.Close()

	if err := c.Hello(cfg.Email.ServerName); err != nil {
		return fmt.Errorf("SMTP HELO failed: %w", err)
	}
	if err := c.Mail(cfg.Email.From); err != nil {
		return fmt.Errorf("SMTP MAIL FROM failed: %w", err)
	}
	if err := c.Rcpt(cfg.Email.To); err != nil {
		return fmt.Errorf("SMTP RCPT TO failed: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA failed: %w", err)
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("SMTP write failed: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP close failed: %w", err)
	}
	return c.Quit()
}

// canSendAlert checks if enough time has passed since the last alert of this type.
// Created: 2026-02-11 — Prevents alert flooding with configurable cooldown
func canSendAlert(alertType string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()

	lastStr, exists := state.LastAlertTimes[alertType]
	if !exists {
		return true
	}
	last, err := time.Parse(time.RFC3339, lastStr)
	if err != nil {
		return true
	}
	return time.Since(last) >= time.Duration(cfg.Monitor.AlertCooldownMin)*time.Minute
}

// recordAlert records the current time for an alert type.
// Created: 2026-02-11 — Updates cooldown tracking for alert deduplication
func recordAlert(alertType string) {
	state.mu.Lock()
	state.LastAlertTimes[alertType] = time.Now().Format(time.RFC3339)
	state.mu.Unlock()
	saveState()
}

// ==================== HTML Template Helpers ====================

// escHTML safely escapes a string for HTML output.
func escHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// buildHTMLPage wraps content in a styled HTML page shell.
// Created: 2026-02-11 — Generates consistent HTML email structure (black/blue/grey)
func buildHTMLPage(title, headerIcon, content string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<style>
body { margin:0; padding:0; font-family:'Segoe UI',Arial,Helvetica,sans-serif; color:#1a1a1a; background:#f0f0f0; }
.wrapper { max-width:1200px; margin:0 auto; background:#ffffff; }
.header { background:#1a3a5c; color:#ffffff; padding:28px 40px; }
.header h1 { margin:0; font-size:30px; font-weight:600; }
.header .subtitle { color:#b0c4de; font-size:18px; margin-top:6px; }
.content { padding:30px 40px; }
.section { margin-bottom:28px; }
.section-title { color:#1a3a5c; font-size:24px; font-weight:600; border-bottom:2px solid #1a3a5c; padding-bottom:8px; margin-bottom:16px; }
.log-block { background:#e8e8e8; padding:18px; font-family:'Courier New',monospace; font-size:14px; border-left:4px solid #1a3a5c; overflow-x:auto; white-space:pre-wrap; word-wrap:break-word; color:#1a1a1a; margin:10px 0; }
.command-block { background:#d0d0d0; padding:14px 18px; font-family:'Courier New',monospace; font-size:14px; color:#1a1a1a; margin:10px 0; border-radius:3px; }
.info-text { color:#4a4a4a; font-size:18px; line-height:1.6; }
.critical-text { color:#1a1a1a; font-weight:700; }
.highlight { background:#c8d8e8; padding:2px 8px; font-weight:600; }
table { border-collapse:collapse; width:100%%; margin:10px 0; }
th { background:#1a3a5c; color:#ffffff; padding:12px 16px; text-align:left; font-size:18px; }
td { padding:10px 16px; border-bottom:1px solid #e0e0e0; font-size:18px; }
tr:nth-child(even) { background:#f5f5f5; }
.status-ok { color:#1a3a5c; font-weight:600; }
.status-warn { color:#1a1a1a; font-weight:700; background:#d0d0d0; padding:2px 8px; }
.status-crit { color:#1a1a1a; font-weight:700; background:#b0b0b0; padding:2px 10px; }
.footer { background:#e8e8e8; padding:18px 40px; font-size:15px; color:#808080; border-top:1px solid #d0d0d0; }
.badge { display:inline-block; padding:4px 14px; font-size:15px; font-weight:600; margin-right:8px; }
.badge-crit { background:#808080; color:#ffffff; }
.badge-warn { background:#b0b0b0; color:#1a1a1a; }
.badge-info { background:#1a3a5c; color:#ffffff; }
.progress-bar { background:#e0e0e0; height:32px; border-radius:4px; overflow:hidden; margin:10px 0; }
.progress-fill { background:#1a3a5c; height:100%%; border-radius:4px; transition:width 0.3s; }
.progress-text { font-size:22px; font-weight:700; color:#1a3a5c; }
</style>
</head>
<body>
<div class="wrapper">
<div class="header">
<h1>%s %s</h1>
<div class="subtitle">%s | %s</div>
</div>
<div class="content">
%s
</div>
<div class="footer">
gslmon RAID Monitor | %s | Report generated: %s
</div>
</div>
</body>
</html>`,
		headerIcon, escHTML(title),
		escHTML(cfg.Email.ServerName), func() string {
			if cfg.RAID.ArrayDevice != "" {
				return escHTML(cfg.RAID.ArrayDevice)
			}
			return "Disk Health Monitor"
		}(),
		content,
		escHTML(cfg.Email.ServerName), time.Now().Format("2006-01-02 15:04:05 MST"),
	)
}

// ==================== Log Monitoring ====================

// monitorLogs periodically checks kernel logs for RAID-related error patterns.
// Created: 2026-02-11 — Scans journalctl for suspicious RAID/disk activity
func monitorLogs(stopCh <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	logger.Printf("Log monitor started (interval: %ds)", cfg.Monitor.LogCheckIntervalSec)

	ticker := time.NewTicker(time.Duration(cfg.Monitor.LogCheckIntervalSec) * time.Second)
	defer ticker.Stop()

	// Compile log patterns into regexps
	var patterns []*regexp.Regexp
	var patternNames []string
	for _, p := range cfg.Monitor.LogPatterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			logger.Printf("Invalid log pattern '%s': %v", p, err)
			continue
		}
		patterns = append(patterns, re)
		patternNames = append(patternNames, p)
	}

	// Compile exclude patterns — lines matching any of these are skipped even if they match alert patterns
	// Added: 2026-03-01 — Filter out non-RAID device noise (e.g. nbd, loop, dm devices)
	var excludePatterns []*regexp.Regexp
	for _, p := range cfg.Monitor.LogExcludePatterns {
		re, err := regexp.Compile("(?i)" + p)
		if err != nil {
			logger.Printf("Invalid log exclude pattern '%s': %v", p, err)
			continue
		}
		excludePatterns = append(excludePatterns, re)
	}
	if len(excludePatterns) > 0 {
		logger.Printf("Log exclude patterns: %d loaded", len(excludePatterns))
	}

	for {
		select {
		case <-stopCh:
			logger.Printf("Log monitor stopping")
			return
		case <-ticker.C:
			checkLogs(patterns, patternNames, excludePatterns)
		}
	}
}

// checkLogs runs a single log check cycle.
// Created: 2026-02-11 — Fetches new kernel log entries and matches against patterns
// Updated: 2026-03-01 — Added excludePatterns to skip non-RAID device noise (nbd, loop, etc.)
func checkLogs(patterns []*regexp.Regexp, patternNames []string, excludePatterns []*regexp.Regexp) {
	state.mu.Lock()
	since := state.LastLogCheck
	state.mu.Unlock()

	// Convert RFC3339 timestamp to "YYYY-MM-DD HH:MM:SS" format for journalctl compatibility
	// Older systemd versions (e.g. CentOS 8 systemd 239) do not parse RFC3339 with timezone offset
	sinceFormatted := since
	if t, err := time.Parse(time.RFC3339, since); err == nil {
		sinceFormatted = t.Format("2006-01-02 15:04:05")
	}

	// Fetch kernel logs since last check, filtered for RAID-related devices
	cmd := fmt.Sprintf("journalctl -k --since '%s' --no-pager -o short-iso 2>/dev/null", sinceFormatted)
	stdout, _, err := runCommand("bash", "-c", cmd)
	if err != nil {
		logger.Printf("journalctl command failed: %v", err)
		return
	}

	// Update last check time
	state.mu.Lock()
	state.LastLogCheck = time.Now().Format(time.RFC3339)
	state.mu.Unlock()

	if strings.TrimSpace(stdout) == "" || strings.Contains(stdout, "-- No entries --") {
		return
	}

	// Scan each line for matching patterns
	var alerts []LogAlert
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		line := scanner.Text()

		// Skip lines matching any exclude pattern (non-RAID devices like nbd, loop, dm)
		excluded := false
		for _, exRe := range excludePatterns {
			if exRe.MatchString(line) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		for i, re := range patterns {
			if re.MatchString(line) {
				severity := classifySeverity(patternNames[i])
				explanation := findExplanation(patternNames[i])
				alerts = append(alerts, LogAlert{
					Timestamp:   extractTimestamp(line),
					RawLine:     line,
					Pattern:     patternNames[i],
					Explanation: explanation,
					Severity:    severity,
				})
				break // one match per line is enough
			}
		}
	}

	if len(alerts) == 0 {
		return
	}

	// Per-severity cooldown — added 2026-03-30
	// Purpose: Ensure CRITICAL pre-degradation alerts are never suppressed by
	// a WARNING/INFO cooldown. Each severity level has its own independent cooldown.
	maxSeverity := "INFO"
	for _, a := range alerts {
		if a.Severity == "CRITICAL" {
			maxSeverity = "CRITICAL"
			break
		}
		if a.Severity == "WARNING" {
			maxSeverity = "WARNING"
		}
	}
	cooldownKey := "log_alert_" + maxSeverity

	if !canSendAlert(cooldownKey) {
		logger.Printf("Log alert suppressed (cooldown %s): %d matches found", cooldownKey, len(alerts))
		return
	}

	logger.Printf("Sending log alert: %d suspicious entries found (severity: %s)", len(alerts), maxSeverity)
	sendLogAlertEmail(alerts, cmd)
	recordAlert(cooldownKey)
	saveState()
}

// classifySeverity assigns CRITICAL/WARNING/INFO based on the matched pattern.
// Created: 2026-02-11 — Categorizes log events by severity for alert prioritization
func classifySeverity(pattern string) string {
	critical := []string{"DID_BAD_TARGET", "Disk failure", "faulty", "journal abort",
		"disable device", "super_written", "I/O error",
		"reset failed", "Buffer I/O error", "Remounting filesystem read-only",
		"revalidation failed", "lost page write", "Input/output error", "SATA link down"}
	warning := []string{"degraded", "hardreset", "frozen", "NCQ", "FPDMA",
		"hard resetting link", "Uncorrectable", "Current_Pending", "Reallocated",
		"exception Emask", "link is slow to respond", "sector.*error",
		"medium error", "task abort"}

	for _, c := range critical {
		if strings.EqualFold(pattern, c) {
			return "CRITICAL"
		}
	}
	for _, w := range warning {
		if strings.EqualFold(pattern, w) {
			return "WARNING"
		}
	}
	return "INFO"
}

// findExplanation looks up the human-readable explanation for a pattern.
// Created: 2026-02-11 — Maps error patterns to plain-English descriptions
func findExplanation(pattern string) string {
	if exp, ok := errorExplanations[pattern]; ok {
		return exp
	}
	// Try partial match
	for key, exp := range errorExplanations {
		if strings.Contains(strings.ToLower(pattern), strings.ToLower(key)) ||
			strings.Contains(strings.ToLower(key), strings.ToLower(pattern)) {
			return exp
		}
	}
	return "Suspicious RAID/disk activity detected. Investigate promptly."
}

// extractTimestamp pulls the timestamp from a log line.
func extractTimestamp(line string) string {
	// short-iso format: 2026-02-11T21:28:45+0530
	if len(line) > 25 {
		return line[:25]
	}
	return line
}

// sendLogAlertEmail builds and sends an HTML email for log-based alerts.
// Created: 2026-02-11 — Formats log alerts with raw lines, commands, and explanations
func sendLogAlertEmail(alerts []LogAlert, command string) {
	// Determine highest severity
	maxSeverity := "INFO"
	for _, a := range alerts {
		if a.Severity == "CRITICAL" {
			maxSeverity = "CRITICAL"
			break
		}
		if a.Severity == "WARNING" {
			maxSeverity = "WARNING"
		}
	}

	subject := fmt.Sprintf("Gslmon Alert from %s: RAID Log Alert — %s (%d entries)",
		cfg.Email.ServerName, maxSeverity, len(alerts))

	var content strings.Builder

	// Summary section
	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Alert Summary</div>`)
	content.WriteString(fmt.Sprintf(`<p class="info-text"><span class="badge badge-crit">%s</span> `, escHTML(maxSeverity)))
	if cfg.RAID.ArrayName != "" {
		content.WriteString(fmt.Sprintf(`<strong>%d</strong> suspicious log entries detected on <strong>%s</strong> (%s)</p>`,
			len(alerts), escHTML(cfg.RAID.ArrayName), escHTML(cfg.RAID.RAIDLevel)))
	} else {
		content.WriteString(fmt.Sprintf(`<strong>%d</strong> suspicious log entries detected on <strong>%s</strong></p>`,
			len(alerts), escHTML(cfg.Email.ServerName)))
	}
	content.WriteString(`<p class="info-text">Monitoring detected kernel log entries matching known RAID/disk error patterns. Immediate investigation is recommended for CRITICAL alerts.</p>`)
	content.WriteString(`</div>`)

	// Group alerts by pattern
	grouped := make(map[string][]LogAlert)
	var order []string
	for _, a := range alerts {
		if _, exists := grouped[a.Pattern]; !exists {
			order = append(order, a.Pattern)
		}
		grouped[a.Pattern] = append(grouped[a.Pattern], a)
	}

	// Alert details
	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Alert Details</div>`)

	for _, pattern := range order {
		group := grouped[pattern]
		sev := group[0].Severity
		badgeClass := "badge-info"
		if sev == "CRITICAL" {
			badgeClass = "badge-crit"
		} else if sev == "WARNING" {
			badgeClass = "badge-warn"
		}

		content.WriteString(fmt.Sprintf(`<h3 style="color:#1a3a5c;margin:15px 0 5px 0;"><span class="badge %s">%s</span> Pattern: %s (%d matches)</h3>`,
			badgeClass, escHTML(sev), escHTML(pattern), len(group)))
		content.WriteString(fmt.Sprintf(`<p class="info-text"><strong>Explanation:</strong> %s</p>`, escHTML(group[0].Explanation)))
		content.WriteString(`<div class="log-block">`)
		for _, a := range group {
			content.WriteString(escHTML(a.RawLine) + "\n")
		}
		content.WriteString(`</div>`)
	}
	content.WriteString(`</div>`)

	// Command used
	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Command Used to Fetch Logs</div>`)
	content.WriteString(`<div class="command-block">` + escHTML(command) + `</div>`)
	content.WriteString(`<p class="info-text">Run this command on the server to see the full log output for the monitored period.</p>`)
	content.WriteString(`</div>`)

	// Recommended actions
	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Recommended Actions</div>`)
	content.WriteString(`<table>`)
	content.WriteString(`<tr><th>Action</th><th>Command</th></tr>`)
	if cfg.RAID.ArrayDevice != "" {
		content.WriteString(fmt.Sprintf(`<tr><td>Check array status</td><td style="font-family:monospace;">sudo mdadm --detail %s</td></tr>`, escHTML(cfg.RAID.ArrayDevice)))
		content.WriteString(`<tr><td>Check array sync status</td><td style="font-family:monospace;">cat /proc/mdstat</td></tr>`)
	}
	content.WriteString(`<tr><td>Check current kernel messages</td><td style="font-family:monospace;">dmesg | tail -50</td></tr>`)
	if cfg.RAID.MountPoint != "" {
		content.WriteString(fmt.Sprintf(`<tr><td>Check mount status</td><td style="font-family:monospace;">mount | grep %s</td></tr>`, escHTML(cfg.RAID.MountPoint)))
	}
	content.WriteString(`</table>`)
	content.WriteString(`</div>`)

	htmlBody := buildHTMLPage("RAID Log Alert", "&#9888;", content.String())

	if err := sendEmail(subject, htmlBody); err != nil {
		logger.Printf("Failed to send log alert email: %v", err)
	} else {
		logger.Printf("Log alert email sent: %s", subject)
	}
}

// ==================== mdstat Monitoring ====================

// monitorMdstat periodically reads /proc/mdstat and alerts on state changes.
// Created: 2026-02-11 — Detects RAID array state transitions (degraded, recovering, etc.)
func monitorMdstat(stopCh <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	logger.Printf("mdstat monitor started (interval: %ds)", cfg.Monitor.MdstatCheckIntervalSec)

	ticker := time.NewTicker(time.Duration(cfg.Monitor.MdstatCheckIntervalSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stopCh:
			logger.Printf("mdstat monitor stopping")
			return
		case <-ticker.C:
			checkMdstat()
		}
	}
}

// parseMdstat reads and parses /proc/mdstat for the configured array.
// Created: 2026-02-11 — Extracts array state, disk status, and rebuild progress
func parseMdstat() (*MdstatInfo, error) {
	data, err := os.ReadFile("/proc/mdstat")
	if err != nil {
		return nil, fmt.Errorf("failed to read /proc/mdstat: %w", err)
	}

	raw := string(data)
	info := &MdstatInfo{RawOutput: raw}

	lines := strings.Split(raw, "\n")
	inArray := false
	for _, line := range lines {
		if strings.HasPrefix(line, cfg.RAID.ArrayName+" ") {
			inArray = true
			// Parse active/inactive and raid level
			if strings.Contains(line, "inactive") {
				info.ArrayState = "inactive"
			} else if strings.Contains(line, "active") {
				info.ArrayState = "active"
			}
		} else if inArray {
			// Parse disk status [UUUU] or [U_U_] etc.
			re := regexp.MustCompile(`\[(\d+)/(\d+)\]\s*\[([U_]+)\]`)
			if matches := re.FindStringSubmatch(line); len(matches) == 4 {
				info.TotalDisks, _ = strconv.Atoi(matches[1])
				info.ActiveDisks, _ = strconv.Atoi(matches[2])
				info.DiskStatus = matches[3]
			}

			// Parse rebuild status
			reRebuild := regexp.MustCompile(`(recovery|resync)\s*=\s*(\S+).*finish=(\S+).*speed=(\S+)`)
			if matches := reRebuild.FindStringSubmatch(line); len(matches) >= 5 {
				info.IsRebuilding = true
				info.RebuildPct = matches[2]
				info.RebuildETA = matches[3]
				info.RebuildSpd = matches[4]
				info.Rebuild = fmt.Sprintf("%s %s (finish=%s, speed=%s)", matches[1], matches[2], matches[3], matches[4])
			}

			// Stop at blank line
			if strings.TrimSpace(line) == "" {
				inArray = false
			}
		}
	}

	// Also get mdadm --detail for state
	stdout, _, err := runCommand("mdadm", "--detail", cfg.RAID.ArrayDevice)
	if err == nil {
		for _, line := range strings.Split(stdout, "\n") {
			if strings.Contains(line, "State :") {
				info.ArrayState = strings.TrimSpace(strings.SplitN(line, ":", 2)[1])
			}
		}
	}

	return info, nil
}

// normalizeBaseState strips the "clean"/"active" base keyword from an mdadm state string,
// returning only the qualifiers (degraded, recovering, resyncing, etc.).
// "clean" and "active" are operationally equivalent — "clean" means idle/synced,
// "active" means pending writes. Neither indicates a health issue.
// Created: 2026-02-12 — Supports state change significance filtering
func normalizeBaseState(state string) string {
	s := strings.TrimSpace(state)
	// Remove leading "clean" or "active" and any trailing comma/space
	for _, prefix := range []string{"clean", "active"} {
		if strings.HasPrefix(s, prefix) {
			s = s[len(prefix):]
			s = strings.TrimLeft(s, ", ")
			return s
		}
	}
	return s
}

// isSignificantStateChange determines if an mdstat state transition warrants an email alert.
// Returns false for noise transitions (clean <-> active with no qualifier change).
// Returns true for meaningful transitions involving degraded, recovering, failed, inactive, etc.
// Created: 2026-02-12 — Prevents email flooding from normal clean/active toggling
func isSignificantStateChange(oldState, newState string) bool {
	// State format: "arrayState|diskStatus|active/total"
	// Example: "active|UUUU|4/4" or "active, degraded, recovering|U_UU|3/4"

	oldParts := strings.SplitN(oldState, "|", 3)
	newParts := strings.SplitN(newState, "|", 3)

	if len(oldParts) < 3 || len(newParts) < 3 {
		// Cannot parse — treat as significant to be safe
		return true
	}

	// Compare disk status and active/total counts — if these changed, always significant
	if oldParts[1] != newParts[1] || oldParts[2] != newParts[2] {
		return true
	}

	// Compare qualifiers (everything after clean/active base word)
	oldQualifiers := normalizeBaseState(oldParts[0])
	newQualifiers := normalizeBaseState(newParts[0])

	// If qualifiers are the same, the only difference is clean <-> active — not significant
	if oldQualifiers == newQualifiers {
		return false
	}

	// Qualifiers changed (e.g., "" -> "degraded", "degraded" -> "degraded, recovering") — significant
	return true
}

// checkMdstat performs a single mdstat check cycle.
// Created: 2026-02-11 — Compares current array state with last known state
func checkMdstat() {
	info, err := parseMdstat()
	if err != nil {
		logger.Printf("mdstat check failed: %v", err)
		return
	}

	currentState := fmt.Sprintf("%s|%s|%d/%d", info.ArrayState, info.DiskStatus, info.ActiveDisks, info.TotalDisks)

	state.mu.Lock()
	lastState := state.LastMdstat
	state.LastMdstat = currentState
	state.mu.Unlock()
	saveState()

	// Skip first run (no previous state to compare)
	if lastState == "" {
		logger.Printf("mdstat baseline: %s", currentState)
		return
	}

	// No raw change at all — nothing to do
	if currentState == lastState {
		return
	}

	// Check if the change is significant (not just clean <-> active noise)
	// Created: 2026-02-12 — Filter out clean/active toggling, only alert on real state changes
	if !isSignificantStateChange(lastState, currentState) {
		logger.Printf("mdstat state change suppressed (clean/active toggle): %s -> %s", lastState, currentState)
		return
	}

	logger.Printf("mdstat significant state change: %s -> %s", lastState, currentState)

	if !canSendAlert("mdstat_change") {
		logger.Printf("mdstat alert suppressed (cooldown)")
		return
	}

	sendMdstatAlertEmail(info, lastState, currentState)
	recordAlert("mdstat_change")
}

// sendMdstatAlertEmail builds and sends an HTML email for array state changes.
// Created: 2026-02-11 — Reports RAID array state transitions with full detail
func sendMdstatAlertEmail(info *MdstatInfo, oldState, newState string) {
	severity := "INFO"
	if strings.Contains(info.ArrayState, "degraded") {
		severity = "WARNING"
	}
	if strings.Contains(info.ArrayState, "inactive") || info.ActiveDisks < 2 {
		severity = "CRITICAL"
	}
	if strings.Contains(newState, "U_") || strings.Contains(newState, "_U") {
		if !strings.Contains(oldState, "_") {
			severity = "CRITICAL"
		}
	}

	subject := fmt.Sprintf("Gslmon %s from %s: RAID Array State Changed — %s",
		severity, cfg.Email.ServerName, escHTML(info.ArrayState))

	var content strings.Builder

	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Array State Change Detected</div>`)
	content.WriteString(`<table>`)
	content.WriteString(`<tr><th>Parameter</th><th>Value</th></tr>`)
	content.WriteString(fmt.Sprintf(`<tr><td>Array</td><td>%s (%s)</td></tr>`, escHTML(cfg.RAID.ArrayDevice), escHTML(cfg.RAID.RAIDLevel)))
	content.WriteString(fmt.Sprintf(`<tr><td>Previous State</td><td>%s</td></tr>`, escHTML(oldState)))
	content.WriteString(fmt.Sprintf(`<tr><td><strong>Current State</strong></td><td><span class="status-crit">%s</span></td></tr>`, escHTML(newState)))
	content.WriteString(fmt.Sprintf(`<tr><td>Array State</td><td>%s</td></tr>`, escHTML(info.ArrayState)))
	content.WriteString(fmt.Sprintf(`<tr><td>Disk Status</td><td style="font-family:monospace;font-size:22px;">[ %s ]</td></tr>`, escHTML(info.DiskStatus)))
	content.WriteString(fmt.Sprintf(`<tr><td>Active/Total</td><td>%d / %d</td></tr>`, info.ActiveDisks, info.TotalDisks))
	if info.Rebuild != "" {
		content.WriteString(fmt.Sprintf(`<tr><td>Rebuild</td><td>%s</td></tr>`, escHTML(info.Rebuild)))
	}
	content.WriteString(`</table>`)
	content.WriteString(`</div>`)

	// Raw /proc/mdstat
	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Raw /proc/mdstat</div>`)
	content.WriteString(`<div class="command-block">cat /proc/mdstat</div>`)
	content.WriteString(`<div class="log-block">` + escHTML(info.RawOutput) + `</div>`)
	content.WriteString(`</div>`)

	// mdadm detail
	stdout, _, _ := runCommand("mdadm", "--detail", cfg.RAID.ArrayDevice)
	if stdout != "" {
		content.WriteString(`<div class="section">`)
		content.WriteString(`<div class="section-title">mdadm --detail Output</div>`)
		content.WriteString(fmt.Sprintf(`<div class="command-block">mdadm --detail %s</div>`, escHTML(cfg.RAID.ArrayDevice)))
		content.WriteString(`<div class="log-block">` + escHTML(stdout) + `</div>`)
		content.WriteString(`</div>`)
	}

	htmlBody := buildHTMLPage("RAID Array State Change", "&#9881;", content.String())

	if err := sendEmail(subject, htmlBody); err != nil {
		logger.Printf("Failed to send mdstat alert email: %v", err)
	} else {
		logger.Printf("mdstat alert email sent: %s", subject)
	}
}

// ==================== SMART Monitoring ====================

// monitorSmart manages periodic SMART long tests and result checking.
// Created: 2026-02-11 — Initiates smartctl long tests every N days, checks results periodically
func monitorSmart(stopCh <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	logger.Printf("SMART monitor started (test interval: %d days, check interval: %d hrs)",
		cfg.Monitor.SmartTestIntervalDays, cfg.Monitor.SmartCheckIntervalHrs)

	ticker := time.NewTicker(time.Duration(cfg.Monitor.SmartCheckIntervalHrs) * time.Hour)
	defer ticker.Stop()

	// Do an initial SMART attribute check on startup
	time.Sleep(30 * time.Second) // allow daemon to settle
	checkSmartHealth()

	for {
		select {
		case <-stopCh:
			logger.Printf("SMART monitor stopping")
			return
		case <-ticker.C:
			checkSmartHealth()
		}
	}
}

// checkSmartHealth checks if a SMART test needs to be initiated and reads current health.
// Created: 2026-02-11 — Orchestrates SMART test lifecycle and attribute monitoring
func checkSmartHealth() {
	// Check if it's time to start a new long test
	state.mu.Lock()
	lastTestStr := state.LastSmartTest
	testActive := state.SmartTestActive
	state.mu.Unlock()

	lastTest, err := time.Parse(time.RFC3339, lastTestStr)
	if err != nil {
		lastTest = time.Now().Add(-24 * time.Duration(cfg.Monitor.SmartTestIntervalDays+1) * time.Hour)
	}

	needTest := time.Since(lastTest) >= time.Duration(cfg.Monitor.SmartTestIntervalDays)*24*time.Hour

	if needTest && !testActive {
		logger.Printf("Initiating SMART long test on all member disks")
		initiateSmartTests()
	}

	// Collect SMART data from all disks
	var reports []SmartDiskReport
	allOK := true

	for _, disk := range cfg.RAID.MemberDisks {
		report := getSmartReportDisk(disk)
		reports = append(reports, report)
		if report.HasIssues {
			allOK = false
		}
	}

	// Check RAID array health — flag degraded arrays even if individual SMART is OK
	// Added: 2026-03-01 — Prevents misleading "All Disks OK" when array is degraded
	arrayDegraded, arrayStateMsg := checkArrayHealth()
	if arrayDegraded {
		allOK = false
	}

	// Check if an active test has completed
	if testActive {
		allComplete := true
		for _, r := range reports {
			for _, t := range r.SelfTests {
				if strings.Contains(strings.ToLower(t.Status), "progress") {
					allComplete = false
					break
				}
			}
		}
		if allComplete {
			state.mu.Lock()
			state.SmartTestActive = false
			state.mu.Unlock()
			saveState()
			logger.Printf("SMART long tests completed on all disks")
		}
	}

	// Send report email
	sendSmartReportEmail(reports, allOK, needTest && !testActive, arrayDegraded, arrayStateMsg)
}

// initiateSmartTests starts a long SMART self-test on each member disk.
// Created: 2026-02-11 — Triggers smartctl --test=long on all RAID member disks
// Updated: 2026-02-12 — Support megaraid pass-through device types
func initiateSmartTests() {
	for _, disk := range cfg.RAID.MemberDisks {
		displayName := disk.Name
		if displayName == "" {
			displayName = disk.Device
		}
		_, _, err := runSmartctl([]string{"--test=long"}, disk)
		if err != nil {
			logger.Printf("Failed to start SMART test on %s: %v", displayName, err)
		} else {
			logger.Printf("SMART long test initiated on %s", displayName)
		}
	}
	state.mu.Lock()
	state.LastSmartTest = time.Now().Format(time.RFC3339)
	state.SmartTestActive = true
	state.mu.Unlock()
	saveState()
}

// getSmartReportDisk runs smartctl -a on a disk (with optional megaraid type) and parses the output.
// Created: 2026-02-12 — Supports both direct and megaraid pass-through SMART access
func getSmartReportDisk(disk DiskConfig) SmartDiskReport {
	displayName := disk.Name
	if displayName == "" {
		displayName = disk.Device
	}
	cmdStr := fmt.Sprintf("smartctl -a %s", disk.Device)
	if disk.Type != "" {
		cmdStr = fmt.Sprintf("smartctl -a -d %s %s", disk.Type, disk.Device)
	}
	report := SmartDiskReport{
		Device:  displayName,
		Command: cmdStr,
	}

	stdout, _, err := runSmartctl([]string{"-a"}, disk)
	if err != nil {
		// smartctl returns non-zero for various reasons including warnings
		if stdout == "" {
			report.Health = "UNAVAILABLE"
			report.HasIssues = true
			report.Issues = append(report.Issues, fmt.Sprintf("smartctl command failed: %v", err))
			return report
		}
	}
	report.RawOutput = stdout

	lines := strings.Split(stdout, "\n")
	inAttributes := false
	inSelfTest := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Parse model/serial
		if strings.HasPrefix(trimmed, "Device Model:") || strings.HasPrefix(trimmed, "Model Number:") {
			report.Model = strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[1])
		}
		if strings.HasPrefix(trimmed, "Serial Number:") {
			report.Serial = strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[1])
		}

		// Parse health
		if strings.Contains(trimmed, "SMART overall-health self-assessment test result:") {
			report.Health = strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[1])
			if report.Health != "PASSED" {
				report.HasIssues = true
				report.Issues = append(report.Issues, fmt.Sprintf("SMART health: %s", report.Health))
			}
		}

		// Parse attributes table
		if strings.HasPrefix(trimmed, "ID#") {
			inAttributes = true
			continue
		}
		if inAttributes {
			if trimmed == "" {
				inAttributes = false
				continue
			}
			attr := parseSmartAttribute(trimmed)
			if attr != nil {
				// Check if this attribute is critical
				for _, cid := range cfg.Monitor.SmartCriticalIDs {
					if attr.ID == cid {
						attr.Critical = true
						rawVal := strings.TrimSpace(attr.RawValue)
						if rawVal != "0" && rawVal != "" {
							// Parse numeric portion of raw value
							numStr := strings.Fields(rawVal)[0]
							if num, err := strconv.ParseInt(numStr, 10, 64); err == nil && num > 0 {
								attr.Failed = true
								report.HasIssues = true
								report.Issues = append(report.Issues,
									fmt.Sprintf("Attribute %d (%s) has non-zero raw value: %s",
										attr.ID, attr.Name, attr.RawValue))
							}
						}
						break
					}
				}
				report.Attributes = append(report.Attributes, *attr)
			}
		}

		// Parse self-test log
		if strings.HasPrefix(trimmed, "Num") && strings.Contains(trimmed, "Test_Description") {
			inSelfTest = true
			continue
		}
		if inSelfTest {
			if trimmed == "" || strings.HasPrefix(trimmed, "SMART") {
				inSelfTest = false
				continue
			}
			entry := parseSmartTestEntry(trimmed)
			if entry != nil {
				report.SelfTests = append(report.SelfTests, *entry)
			}
		}
	}

	return report
}

// parseSmartAttribute parses a single SMART attribute line.
// Created: 2026-02-11 — Extracts ID, name, values from smartctl attribute table row
func parseSmartAttribute(line string) *SmartAttribute {
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return nil
	}

	id, err := strconv.Atoi(fields[0])
	if err != nil {
		return nil
	}

	val, _ := strconv.Atoi(fields[3])
	worst, _ := strconv.Atoi(fields[4])
	thresh, _ := strconv.Atoi(fields[5])

	return &SmartAttribute{
		ID:        id,
		Name:      fields[1],
		Value:     val,
		Worst:     worst,
		Threshold: thresh,
		RawValue:  strings.Join(fields[9:], " "),
	}
}

// parseSmartTestEntry parses a single self-test log entry.
// Created: 2026-02-11 — Extracts test number, type, status, and error info
func parseSmartTestEntry(line string) *SmartTestEntry {
	// Format: # 1  Extended offline    Completed without error  00%  2285  -
	if len(line) < 5 || line[0] != '#' {
		return nil
	}

	entry := &SmartTestEntry{}
	entry.Num = strings.TrimSpace(line[1:3])

	// The status field is variable width, parse carefully
	remaining := line[3:]
	fields := strings.Fields(remaining)
	if len(fields) < 3 {
		return nil
	}

	// Find test type (first 1-2 words before status)
	if strings.HasPrefix(strings.TrimSpace(remaining), "Extended") ||
		strings.HasPrefix(strings.TrimSpace(remaining), "Short") ||
		strings.HasPrefix(strings.TrimSpace(remaining), "Conveyance") {
		entry.Type = fields[0] + " " + fields[1]
	} else {
		entry.Type = fields[0]
	}

	// Check for errors in status
	statusStr := strings.ToLower(line)
	if strings.Contains(statusStr, "completed without error") {
		entry.Status = "Completed without error"
		entry.HasError = false
	} else if strings.Contains(statusStr, "in progress") {
		entry.Status = "In progress"
		entry.HasError = false
	} else if strings.Contains(statusStr, "aborted") {
		entry.Status = "Aborted"
		entry.HasError = true
	} else {
		entry.Status = "Error/Unknown"
		entry.HasError = true
	}

	return entry
}

// sendSmartReportEmail builds and sends a formatted SMART health report email.
// Created: 2026-02-11 — Generates HTML SMART report with per-disk health summary
// Updated: 2026-03-01 — Includes RAID array state to prevent misleading "All OK" on degraded arrays
func sendSmartReportEmail(reports []SmartDiskReport, allOK bool, testJustStarted bool, arrayDegraded bool, arrayStateMsg string) {
	var subject string
	if allOK {
		subject = fmt.Sprintf("Gslmon Information from %s: SMART Health Report — All Disks OK", cfg.Email.ServerName)
	} else if arrayDegraded {
		subject = fmt.Sprintf("Gslmon Critical from %s: SMART Health Report — RAID Array Degraded", cfg.Email.ServerName)
	} else {
		subject = fmt.Sprintf("Gslmon Critical from %s: SMART Health Issues Detected on RAID Disks", cfg.Email.ServerName)
	}

	var content strings.Builder

	// Array state section — show degraded warning at top if applicable
	// Added: 2026-03-01 — Prominent array health status before individual disk details
	if arrayStateMsg != "" {
		content.WriteString(`<div class="section">`)
		content.WriteString(`<div class="section-title">RAID Array State</div>`)
		if arrayDegraded {
			content.WriteString(fmt.Sprintf(`<p class="info-text"><span class="badge badge-crit">DEGRADED</span> %s</p>`, escHTML(arrayStateMsg)))
		} else {
			content.WriteString(fmt.Sprintf(`<p class="info-text"><span class="badge badge-info">HEALTHY</span> %s</p>`, escHTML(arrayStateMsg)))
		}
		content.WriteString(`</div>`)
	}

	// Summary section
	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">SMART Health Summary</div>`)

	if allOK {
		content.WriteString(`<p class="info-text"><span class="badge badge-info">OK</span> `)
		content.WriteString(`All RAID member disks are reporting healthy SMART status. No critical attribute anomalies detected.</p>`)
	} else if arrayDegraded {
		content.WriteString(`<p class="info-text"><span class="badge badge-crit">ARRAY DEGRADED</span> `)
		content.WriteString(fmt.Sprintf(`%s</p>`, escHTML(arrayStateMsg)))
	} else {
		content.WriteString(`<p class="info-text"><span class="badge badge-crit">ISSUES DETECTED</span> `)
		content.WriteString(`One or more RAID member disks are reporting SMART anomalies. Immediate investigation recommended.</p>`)
	}

	if testJustStarted {
		content.WriteString(`<p class="info-text"><strong>Note:</strong> A SMART long self-test has been initiated on all member disks. Results will be available in the next check cycle.</p>`)
	}

	// Overview table
	content.WriteString(`<table>`)
	content.WriteString(`<tr><th>Device</th><th>Model</th><th>Serial</th><th>Health</th><th>Status</th></tr>`)
	for _, r := range reports {
		statusClass := "status-ok"
		statusText := "OK"
		if r.HasIssues {
			statusClass = "status-crit"
			statusText = "ISSUES"
		}
		content.WriteString(fmt.Sprintf(`<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td><span class="%s">%s</span></td></tr>`,
			escHTML(r.Device), escHTML(r.Model), escHTML(r.Serial), escHTML(r.Health), statusClass, statusText))
	}
	content.WriteString(`</table>`)
	content.WriteString(`</div>`)

	// Per-disk details
	for _, r := range reports {
		content.WriteString(`<div class="section">`)
		diskTitle := fmt.Sprintf("%s — %s (%s)", r.Device, r.Model, r.Serial)
		if r.HasIssues {
			content.WriteString(fmt.Sprintf(`<div class="section-title">&#9888; %s</div>`, escHTML(diskTitle)))
		} else {
			content.WriteString(fmt.Sprintf(`<div class="section-title">%s</div>`, escHTML(diskTitle)))
		}

		// Issues list (if any)
		if r.HasIssues && len(r.Issues) > 0 {
			content.WriteString(`<div style="background:#d0d0d0;padding:12px;margin:8px 0;border-left:4px solid #1a1a1a;">`)
			content.WriteString(`<strong class="critical-text">Issues Found:</strong><ul style="margin:5px 0;">`)
			for _, issue := range r.Issues {
				content.WriteString(fmt.Sprintf(`<li class="critical-text">%s</li>`, escHTML(issue)))
			}
			content.WriteString(`</ul></div>`)
		}

		// Critical attributes table
		hasCritical := false
		for _, a := range r.Attributes {
			if a.Critical {
				hasCritical = true
				break
			}
		}

		if hasCritical {
			content.WriteString(`<p style="color:#4a4a4a;font-size:16px;margin:8px 0 4px 0;"><strong>Critical Attributes:</strong></p>`)
			content.WriteString(`<table>`)
			content.WriteString(`<tr><th>ID</th><th>Attribute</th><th>Value</th><th>Worst</th><th>Thresh</th><th>Raw Value</th><th>Status</th></tr>`)
			for _, a := range r.Attributes {
				if !a.Critical {
					continue
				}
				statusText := `<span class="status-ok">OK</span>`
				if a.Failed {
					statusText = `<span class="status-crit">ALERT</span>`
				}
				content.WriteString(fmt.Sprintf(`<tr><td>%d</td><td>%s</td><td>%d</td><td>%d</td><td>%d</td><td>%s</td><td>%s</td></tr>`,
					a.ID, escHTML(a.Name), a.Value, a.Worst, a.Threshold, escHTML(a.RawValue), statusText))
			}
			content.WriteString(`</table>`)
		}

		// Self-test log
		if len(r.SelfTests) > 0 {
			content.WriteString(`<p style="color:#4a4a4a;font-size:16px;margin:12px 0 4px 0;"><strong>Recent Self-Tests (last 5):</strong></p>`)
			content.WriteString(`<table>`)
			content.WriteString(`<tr><th>#</th><th>Type</th><th>Status</th></tr>`)
			count := 0
			for _, t := range r.SelfTests {
				if count >= 5 {
					break
				}
				statusHTML := escHTML(t.Status)
				if t.HasError {
					statusHTML = fmt.Sprintf(`<span class="status-crit">%s</span>`, escHTML(t.Status))
				}
				content.WriteString(fmt.Sprintf(`<tr><td>%s</td><td>%s</td><td>%s</td></tr>`,
					escHTML(t.Num), escHTML(t.Type), statusHTML))
				count++
			}
			content.WriteString(`</table>`)
		}

		// Raw output for disks with issues
		if r.HasIssues {
			content.WriteString(`<p style="color:#4a4a4a;font-size:16px;margin:12px 0 4px 0;"><strong>Full smartctl output:</strong></p>`)
			content.WriteString(fmt.Sprintf(`<div class="command-block">%s</div>`, escHTML(r.Command)))
			content.WriteString(`<div class="log-block">` + escHTML(r.RawOutput) + `</div>`)
		}

		content.WriteString(`</div>`)
	}

	// Command reference
	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Verification Commands</div>`)
	content.WriteString(`<table>`)
	content.WriteString(`<tr><th>Purpose</th><th>Command</th></tr>`)
	for _, disk := range cfg.RAID.MemberDisks {
		displayName := disk.Name
		if displayName == "" {
			displayName = disk.Device
		}
		cmdStr := fmt.Sprintf("smartctl -a %s", disk.Device)
		if disk.Type != "" {
			cmdStr = fmt.Sprintf("smartctl -a -d %s %s", disk.Type, disk.Device)
		}
		content.WriteString(fmt.Sprintf(`<tr><td>Full SMART report (%s)</td><td style="font-family:monospace;">%s</td></tr>`,
			escHTML(displayName), escHTML(cmdStr)))
	}
	if len(cfg.RAID.MemberDisks) > 0 {
		disk := cfg.RAID.MemberDisks[0]
		cmdStr := fmt.Sprintf("smartctl --test=long %s", disk.Device)
		if disk.Type != "" {
			cmdStr = fmt.Sprintf("smartctl --test=long -d %s %s", disk.Type, disk.Device)
		}
		content.WriteString(fmt.Sprintf(`<tr><td>Start long self-test</td><td style="font-family:monospace;">%s</td></tr>`,
			escHTML(cmdStr)))
	}
	if cfg.RAID.ArrayDevice != "" {
		content.WriteString(fmt.Sprintf(`<tr><td>RAID array detail</td><td style="font-family:monospace;">mdadm --detail %s</td></tr>`,
			escHTML(cfg.RAID.ArrayDevice)))
	}
	content.WriteString(`</table>`)
	content.WriteString(`</div>`)

	icon := "&#9989;"
	if !allOK {
		icon = "&#9888;"
	}
	htmlBody := buildHTMLPage("SMART Health Report", icon, content.String())

	if err := sendEmail(subject, htmlBody); err != nil {
		logger.Printf("Failed to send SMART report email: %v", err)
	} else {
		logger.Printf("SMART report email sent: %s", subject)
	}
}

// ==================== Rebuild Progress Monitoring ====================

// monitorRebuild periodically checks RAID rebuild progress and reports via email.
// Created: 2026-02-11 — Sends rebuild progress every N hours and completion notification
// Updated: 2026-02-28 — Supports both SW RAID (mdstat) and HW RAID (perccli64) paths
func monitorRebuild(stopCh <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	if isHardwareRAID() {
		if hwRaidCLI != "" {
			logger.Printf("Rebuild monitor started — hardware RAID via %s (interval: %d min)", filepath.Base(hwRaidCLI), cfg.Monitor.RebuildCheckIntervalMin)
		} else {
			logger.Printf("Rebuild monitor started — hardware RAID (WARNING: no CLI tool found, interval: %d min)", cfg.Monitor.RebuildCheckIntervalMin)
		}
	} else {
		logger.Printf("Rebuild monitor started — software RAID via /proc/mdstat (interval: %d min)", cfg.Monitor.RebuildCheckIntervalMin)
	}

	// Immediate check on startup — don't wait for first ticker interval
	// Added: 2026-02-28 — Ensures rebuild status is reported immediately on service start
	checkRebuildProgress()

	// Rebuild monitor uses two intervals:
	// - Normal/progress reporting: 30-min ticker (configurable) — sends progress emails
	// - Completion detection: 30-sec fast poll when rebuild is active — detects completion immediately
	// Updated: 2026-03-01 — Fast completion detection so rebuild complete email is sent within ~30s
	progressInterval := time.Duration(cfg.Monitor.RebuildCheckIntervalMin) * time.Minute
	completionPollInterval := 30 * time.Second
	progressTicker := time.NewTicker(progressInterval)
	completionTicker := time.NewTicker(completionPollInterval)
	defer progressTicker.Stop()
	defer completionTicker.Stop()

	for {
		select {
		case <-stopCh:
			logger.Printf("Rebuild monitor stopping")
			return
		case <-progressTicker.C:
			// Full check — sends progress email if rebuilding, or completion if just finished
			checkRebuildProgress()
		case <-completionTicker.C:
			// Fast poll — only activates when rebuild is past 90% to detect completion within ~30s
			// Updated: 2026-03-01 — Threshold-based fast poll to avoid unnecessary checks early in rebuild
			state.mu.Lock()
			wasActive := state.RebuildWasActive
			lastPct := state.LastRebuildPct
			state.mu.Unlock()
			if wasActive {
				pctVal := 0.0
				pctStr := strings.TrimSuffix(lastPct, "%")
				if v, err := strconv.ParseFloat(pctStr, 64); err == nil {
					pctVal = v
				}
				if pctVal < 90.0 {
					continue
				}
				checkRebuildCompletion()
			}
		}
	}
}

// checkRebuildProgress reads current rebuild state and sends progress or completion email.
// Created: 2026-02-11 — Tracks rebuild percentage and detects completion transitions
// Updated: 2026-02-28 — Dispatches to HW RAID path for hardware RAID controllers
func checkRebuildProgress() {
	// Dispatch to hardware RAID path if applicable
	if isHardwareRAID() {
		checkHWRebuildProgress()
		return
	}

	info, err := parseMdstat()
	if err != nil {
		logger.Printf("Rebuild progress check failed: %v", err)
		return
	}

	state.mu.Lock()
	wasRebuilding := state.RebuildWasActive
	lastPct := state.LastRebuildPct
	state.mu.Unlock()

	if info.IsRebuilding {
		// Rebuild is in progress — send progress report
		state.mu.Lock()
		state.RebuildWasActive = true
		state.LastRebuildPct = info.RebuildPct
		state.mu.Unlock()
		saveState()

		logger.Printf("Rebuild in progress: %s (was: %s)", info.RebuildPct, lastPct)
		sendRebuildProgressEmail(info)
	} else if wasRebuilding {
		// Rebuild just completed — send completion notification
		state.mu.Lock()
		state.RebuildWasActive = false
		state.LastRebuildPct = "100%"
		state.mu.Unlock()
		saveState()

		logger.Printf("Rebuild completed! Sending completion notification")
		sendRebuildCompleteEmail(info)
	}
}

// checkRebuildCompletion is a lightweight check that only detects rebuild-to-complete transitions.
// Called every 30 seconds when a rebuild is known to be active, so completion is detected immediately
// without waiting for the full 30-min progress ticker. Does NOT send progress emails (avoids flooding).
// Created: 2026-03-01 — Fast completion detection for both SW and HW RAID
func checkRebuildCompletion() {
	if isHardwareRAID() {
		if hwRaidCLI == "" {
			return
		}
		rebuilds, err := parsePercRebuildProgress()
		if err != nil {
			return
		}
		if len(rebuilds) == 0 {
			// No drives rebuilding — rebuild just completed
			state.mu.Lock()
			state.RebuildWasActive = false
			state.LastRebuildPct = "100%"
			state.mu.Unlock()
			saveState()
			logger.Printf("HW RAID rebuild completed! Sending completion notification (fast detection)")
			sendHWRebuildCompleteEmail()
		}
	} else {
		info, err := parseMdstat()
		if err != nil {
			return
		}
		if !info.IsRebuilding {
			// Rebuild just completed
			state.mu.Lock()
			state.RebuildWasActive = false
			state.LastRebuildPct = "100%"
			state.mu.Unlock()
			saveState()
			logger.Printf("Rebuild completed! Sending completion notification (fast detection)")
			sendRebuildCompleteEmail(info)
		}
	}
}

// sendRebuildProgressEmail sends a formatted rebuild progress report with visual bar.
// Created: 2026-02-11 — HTML email with rebuild percentage, ETA, speed, and progress bar
func sendRebuildProgressEmail(info *MdstatInfo) {
	subject := fmt.Sprintf("Gslmon Information from %s: RAID Rebuild Progress — %s",
		cfg.Email.ServerName, info.RebuildPct)

	// Parse numeric percentage for progress bar
	pctNum := 0.0
	pctStr := strings.TrimSuffix(info.RebuildPct, "%")
	if v, err := strconv.ParseFloat(pctStr, 64); err == nil {
		pctNum = v
	}

	var content strings.Builder

	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">RAID Rebuild Progress</div>`)
	content.WriteString(fmt.Sprintf(`<p class="info-text">Array <strong>%s</strong> (%s) is currently rebuilding.</p>`,
		escHTML(cfg.RAID.ArrayDevice), escHTML(cfg.RAID.RAIDLevel)))

	// Visual progress bar
	content.WriteString(fmt.Sprintf(`<div class="progress-bar"><div class="progress-fill" style="width:%.1f%%"></div></div>`, pctNum))
	content.WriteString(fmt.Sprintf(`<p class="progress-text" style="text-align:center;">%s complete</p>`, escHTML(info.RebuildPct)))

	// Details table
	content.WriteString(`<table>`)
	content.WriteString(`<tr><th>Parameter</th><th>Value</th></tr>`)
	content.WriteString(fmt.Sprintf(`<tr><td>Array</td><td>%s (%s)</td></tr>`, escHTML(cfg.RAID.ArrayDevice), escHTML(cfg.RAID.RAIDLevel)))
	content.WriteString(fmt.Sprintf(`<tr><td>Array State</td><td>%s</td></tr>`, escHTML(info.ArrayState)))
	content.WriteString(fmt.Sprintf(`<tr><td>Disk Status</td><td style="font-family:monospace;font-size:22px;">[ %s ]</td></tr>`, escHTML(info.DiskStatus)))
	content.WriteString(fmt.Sprintf(`<tr><td>Active / Total</td><td>%d / %d</td></tr>`, info.ActiveDisks, info.TotalDisks))
	content.WriteString(fmt.Sprintf(`<tr><td><strong>Progress</strong></td><td><strong>%s</strong></td></tr>`, escHTML(info.RebuildPct)))
	content.WriteString(fmt.Sprintf(`<tr><td>Estimated Time Remaining</td><td>%s</td></tr>`, escHTML(info.RebuildETA)))
	content.WriteString(fmt.Sprintf(`<tr><td>Rebuild Speed</td><td>%s</td></tr>`, escHTML(info.RebuildSpd)))
	content.WriteString(`</table>`)
	content.WriteString(`</div>`)

	// Raw mdstat
	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Raw /proc/mdstat</div>`)
	content.WriteString(`<div class="command-block">cat /proc/mdstat</div>`)
	content.WriteString(`<div class="log-block">` + escHTML(info.RawOutput) + `</div>`)
	content.WriteString(`</div>`)

	htmlBody := buildHTMLPage("RAID Rebuild Progress", "&#9881;", content.String())

	if err := sendEmail(subject, htmlBody); err != nil {
		logger.Printf("Failed to send rebuild progress email: %v", err)
	} else {
		logger.Printf("Rebuild progress email sent: %s", subject)
	}
}

// sendRebuildCompleteEmail sends a notification that the RAID rebuild has finished.
// Created: 2026-02-11 — HTML email confirming rebuild completion with final array state
func sendRebuildCompleteEmail(info *MdstatInfo) {
	subject := fmt.Sprintf("Gslmon Information from %s: RAID Rebuild Complete — Array Fully Synced",
		cfg.Email.ServerName)

	var content strings.Builder

	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">RAID Rebuild Complete</div>`)
	content.WriteString(fmt.Sprintf(`<p class="info-text"><span class="badge badge-info">COMPLETE</span> `))
	content.WriteString(fmt.Sprintf(`The RAID rebuild on <strong>%s</strong> (%s) has completed successfully. The array is now fully synced.</p>`,
		escHTML(cfg.RAID.ArrayDevice), escHTML(cfg.RAID.RAIDLevel)))

	// Final state table
	content.WriteString(`<table>`)
	content.WriteString(`<tr><th>Parameter</th><th>Value</th></tr>`)
	content.WriteString(fmt.Sprintf(`<tr><td>Array</td><td>%s (%s)</td></tr>`, escHTML(cfg.RAID.ArrayDevice), escHTML(cfg.RAID.RAIDLevel)))
	content.WriteString(fmt.Sprintf(`<tr><td>Array State</td><td><span class="status-ok">%s</span></td></tr>`, escHTML(info.ArrayState)))
	content.WriteString(fmt.Sprintf(`<tr><td>Disk Status</td><td style="font-family:monospace;font-size:22px;">[ %s ]</td></tr>`, escHTML(info.DiskStatus)))
	content.WriteString(fmt.Sprintf(`<tr><td>Active / Total</td><td>%d / %d</td></tr>`, info.ActiveDisks, info.TotalDisks))
	content.WriteString(fmt.Sprintf(`<tr><td>Mount Point</td><td>%s</td></tr>`, escHTML(cfg.RAID.MountPoint)))
	content.WriteString(`</table>`)
	content.WriteString(`</div>`)

	// mdadm detail
	stdout, _, _ := runCommand("mdadm", "--detail", cfg.RAID.ArrayDevice)
	if stdout != "" {
		content.WriteString(`<div class="section">`)
		content.WriteString(`<div class="section-title">mdadm --detail Output</div>`)
		content.WriteString(fmt.Sprintf(`<div class="command-block">mdadm --detail %s</div>`, escHTML(cfg.RAID.ArrayDevice)))
		content.WriteString(`<div class="log-block">` + escHTML(stdout) + `</div>`)
		content.WriteString(`</div>`)
	}

	// Raw mdstat
	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Raw /proc/mdstat</div>`)
	content.WriteString(`<div class="command-block">cat /proc/mdstat</div>`)
	content.WriteString(`<div class="log-block">` + escHTML(info.RawOutput) + `</div>`)
	content.WriteString(`</div>`)

	htmlBody := buildHTMLPage("RAID Rebuild Complete", "&#9989;", content.String())

	if err := sendEmail(subject, htmlBody); err != nil {
		logger.Printf("Failed to send rebuild complete email: %v", err)
	} else {
		logger.Printf("Rebuild complete email sent: %s", subject)
	}
}

// ==================== Hardware RAID Rebuild Monitoring ====================

// detectHWRaidCLI scans standard paths for perccli64 or storcli64 CLI tools.
// Created: 2026-02-28 — Auto-detects hardware RAID management CLI at runtime
func detectHWRaidCLI() string {
	paths := []string{
		"/usr/local/bin/perccli64",
		"/opt/MegaRAID/perccli/perccli64",
		"/usr/local/bin/storcli64",
		"/opt/MegaRAID/storcli/storcli64",
		"/usr/local/bin/perccli",
		"/usr/local/bin/storcli",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// isHardwareRAID returns true if the server uses a hardware RAID controller (no software RAID array device).
// Created: 2026-02-28 — Checks config for empty array_device with megaraid-type member disks
func isHardwareRAID() bool {
	if cfg.RAID.ArrayDevice != "" {
		return false
	}
	for _, d := range cfg.RAID.MemberDisks {
		if strings.HasPrefix(d.Type, "megaraid,") {
			return true
		}
	}
	return false
}

// parsePercRebuildProgress runs perccli64 /c0/eall/sall show rebuild and parses the output.
// Created: 2026-02-28 — Extracts drive IDs and rebuild percentages from perccli64 output
func parsePercRebuildProgress() ([]HWRaidRebuildInfo, error) {
	if hwRaidCLI == "" {
		return nil, fmt.Errorf("no perccli64/storcli64 CLI tool available")
	}

	stdout, stderr, err := runCommand(hwRaidCLI, "/c0/eall/sall", "show", "rebuild")
	if err != nil {
		return nil, fmt.Errorf("perccli64 rebuild check failed: %w (stderr: %s)", err, stderr)
	}

	var rebuilds []HWRaidRebuildInfo
	lines := strings.Split(stdout, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "Drive-ID") {
			continue
		}
		// Skip header/footer lines
		if strings.HasPrefix(trimmed, "Controller") || strings.HasPrefix(trimmed, "Status") ||
			strings.HasPrefix(trimmed, "Description") || !strings.HasPrefix(trimmed, "/c") {
			continue
		}

		fields := strings.Fields(trimmed)
		if len(fields) < 3 {
			continue
		}

		driveID := fields[0]

		if strings.Contains(trimmed, "Not in progress") {
			continue
		}

		if strings.Contains(trimmed, "In progress") {
			pct := fields[1] + "%"
			info := HWRaidRebuildInfo{
				DriveID:      driveID,
				DriveName:    mapDriveIDToName(driveID),
				RebuildPct:   pct,
				IsRebuilding: true,
				RawOutput:    stdout,
			}
			rebuilds = append(rebuilds, info)
		}
	}
	return rebuilds, nil
}

// mapDriveIDToName maps a perccli64 drive ID (e.g. /c0/e32/s3) to a config disk name via megaraid type.
// Created: 2026-02-28 — Extracts slot number from drive ID and matches against member_disks config
func mapDriveIDToName(driveID string) string {
	// Extract slot number from /c0/e32/s3 -> "3"
	parts := strings.Split(driveID, "/")
	if len(parts) < 4 {
		return driveID
	}
	slotPart := parts[3] // "s3"
	if !strings.HasPrefix(slotPart, "s") {
		return driveID
	}
	slotNum := strings.TrimPrefix(slotPart, "s")

	// Match against config member_disks type field: "megaraid,3"
	for _, d := range cfg.RAID.MemberDisks {
		if d.Type == "megaraid,"+slotNum {
			return d.Name
		}
	}
	return "Slot " + slotNum
}

// checkHWRebuildProgress checks hardware RAID rebuild status and sends progress/completion emails.
// Created: 2026-02-28 — HW RAID equivalent of the SW RAID checkRebuildProgress logic
func checkHWRebuildProgress() {
	if hwRaidCLI == "" {
		logger.Printf("WARNING: No perccli64/storcli64 found — cannot check HW RAID rebuild status")
		return
	}

	rebuilds, err := parsePercRebuildProgress()
	if err != nil {
		logger.Printf("HW RAID rebuild check failed: %v", err)
		return
	}

	state.mu.Lock()
	wasRebuilding := state.RebuildWasActive
	lastPct := state.LastRebuildPct
	state.mu.Unlock()

	isRebuilding := len(rebuilds) > 0

	if isRebuilding {
		// Determine highest percentage across all rebuilding drives
		maxPct := ""
		for _, r := range rebuilds {
			if maxPct == "" || r.RebuildPct > maxPct {
				maxPct = r.RebuildPct
			}
		}

		state.mu.Lock()
		state.RebuildWasActive = true
		state.LastRebuildPct = maxPct
		state.mu.Unlock()
		saveState()

		driveNames := make([]string, len(rebuilds))
		for i, r := range rebuilds {
			driveNames[i] = fmt.Sprintf("%s (%s) at %s", r.DriveName, r.DriveID, r.RebuildPct)
		}
		logger.Printf("HW RAID rebuild in progress: %s (was: %s)", strings.Join(driveNames, ", "), lastPct)
		sendHWRebuildProgressEmail(rebuilds)
	} else if wasRebuilding {
		// Rebuild just completed
		state.mu.Lock()
		state.RebuildWasActive = false
		state.LastRebuildPct = "100%"
		state.mu.Unlock()
		saveState()

		logger.Printf("HW RAID rebuild completed! Sending completion notification")
		sendHWRebuildCompleteEmail()
	}
}

// sendHWRebuildProgressEmail sends an HTML email with HW RAID rebuild progress details.
// Created: 2026-02-28 — Progress bar, drive table, and raw CLI output for hardware RAID rebuilds
func sendHWRebuildProgressEmail(rebuilds []HWRaidRebuildInfo) {
	// Use first rebuilding drive's percentage for subject line
	pctDisplay := rebuilds[0].RebuildPct
	subject := fmt.Sprintf("Gslmon Information from %s: HW RAID Rebuild Progress — %s",
		cfg.Email.ServerName, pctDisplay)

	// Parse numeric percentage for progress bar
	pctNum := 0.0
	pctStr := strings.TrimSuffix(pctDisplay, "%")
	if v, err := strconv.ParseFloat(pctStr, 64); err == nil {
		pctNum = v
	}

	var content strings.Builder

	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Hardware RAID Rebuild Progress</div>`)
	content.WriteString(fmt.Sprintf(`<p class="info-text"><span class="badge badge-warn">REBUILDING</span> `))
	content.WriteString(fmt.Sprintf(`Controller <strong>%s</strong> has an active rebuild.</p>`,
		escHTML(cfg.RAID.RAIDLevel)))

	// Visual progress bar (primary drive)
	content.WriteString(fmt.Sprintf(`<div class="progress-bar"><div class="progress-fill" style="width:%.1f%%"></div></div>`, pctNum))
	content.WriteString(fmt.Sprintf(`<p class="progress-text" style="text-align:center;">%s complete</p>`, escHTML(pctDisplay)))

	// Rebuilding drives table
	content.WriteString(`<table>`)
	content.WriteString(`<tr><th>Drive ID</th><th>Name</th><th>Progress</th><th>Status</th></tr>`)
	for _, r := range rebuilds {
		content.WriteString(fmt.Sprintf(`<tr><td>%s</td><td>%s</td><td><strong>%s</strong></td><td><span class="status-warn">Rebuilding</span></td></tr>`,
			escHTML(r.DriveID), escHTML(r.DriveName), escHTML(r.RebuildPct)))
	}
	content.WriteString(`</table>`)
	content.WriteString(`</div>`)

	// Controller info table
	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Controller Details</div>`)
	content.WriteString(`<table>`)
	content.WriteString(`<tr><th>Parameter</th><th>Value</th></tr>`)
	content.WriteString(fmt.Sprintf(`<tr><td>Controller</td><td>%s</td></tr>`, escHTML(cfg.RAID.RAIDLevel)))
	content.WriteString(fmt.Sprintf(`<tr><td>Server</td><td>%s</td></tr>`, escHTML(cfg.Email.ServerName)))
	content.WriteString(fmt.Sprintf(`<tr><td>Drives Rebuilding</td><td>%d</td></tr>`, len(rebuilds)))
	content.WriteString(fmt.Sprintf(`<tr><td>CLI Tool</td><td>%s</td></tr>`, escHTML(hwRaidCLI)))
	content.WriteString(`</table>`)
	content.WriteString(`</div>`)

	// Raw CLI output
	if len(rebuilds) > 0 && rebuilds[0].RawOutput != "" {
		content.WriteString(`<div class="section">`)
		content.WriteString(`<div class="section-title">Raw CLI Output</div>`)
		content.WriteString(fmt.Sprintf(`<div class="command-block">%s /c0/eall/sall show rebuild</div>`, escHTML(filepath.Base(hwRaidCLI))))
		content.WriteString(`<div class="log-block">` + escHTML(rebuilds[0].RawOutput) + `</div>`)
		content.WriteString(`</div>`)
	}

	htmlBody := buildHTMLPage("HW RAID Rebuild Progress", "&#9881;", content.String())

	if err := sendEmail(subject, htmlBody); err != nil {
		logger.Printf("Failed to send HW rebuild progress email: %v", err)
	} else {
		logger.Printf("HW rebuild progress email sent: %s", subject)
	}
}

// sendHWRebuildCompleteEmail sends an HTML email confirming HW RAID rebuild completion.
// Created: 2026-02-28 — Fetches final drive states via perccli64 and sends completion badge email
func sendHWRebuildCompleteEmail() {
	subject := fmt.Sprintf("Gslmon Information from %s: HW RAID Rebuild Complete — All Drives Online",
		cfg.Email.ServerName)

	var content strings.Builder

	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Hardware RAID Rebuild Complete</div>`)
	content.WriteString(fmt.Sprintf(`<p class="info-text"><span class="badge badge-info">COMPLETE</span> `))
	content.WriteString(fmt.Sprintf(`The hardware RAID rebuild on <strong>%s</strong> (%s) has completed successfully.</p>`,
		escHTML(cfg.Email.ServerName), escHTML(cfg.RAID.RAIDLevel)))
	content.WriteString(`</div>`)

	// Fetch final drive states
	if hwRaidCLI != "" {
		stdout, _, err := runCommand(hwRaidCLI, "/c0/eall/sall", "show")
		if err == nil && stdout != "" {
			content.WriteString(`<div class="section">`)
			content.WriteString(`<div class="section-title">Current Drive States</div>`)
			content.WriteString(fmt.Sprintf(`<div class="command-block">%s /c0/eall/sall show</div>`, escHTML(filepath.Base(hwRaidCLI))))
			content.WriteString(`<div class="log-block">` + escHTML(stdout) + `</div>`)
			content.WriteString(`</div>`)
		}

		// Also fetch controller topology
		stdout2, _, err2 := runCommand(hwRaidCLI, "/c0", "show")
		if err2 == nil && stdout2 != "" {
			content.WriteString(`<div class="section">`)
			content.WriteString(`<div class="section-title">Controller Topology</div>`)
			content.WriteString(fmt.Sprintf(`<div class="command-block">%s /c0 show</div>`, escHTML(filepath.Base(hwRaidCLI))))
			content.WriteString(`<div class="log-block">` + escHTML(stdout2) + `</div>`)
			content.WriteString(`</div>`)
		}
	}

	htmlBody := buildHTMLPage("HW RAID Rebuild Complete", "&#9989;", content.String())

	if err := sendEmail(subject, htmlBody); err != nil {
		logger.Printf("Failed to send HW rebuild complete email: %v", err)
	} else {
		logger.Printf("HW rebuild complete email sent: %s", subject)
	}
}

// ==================== RAID Array Health for SMART Reports ====================

// checkArrayHealth checks the RAID array state and returns whether it is degraded.
// Created: 2026-03-01 — Prevents misleading "All Disks OK" in SMART reports when array is degraded
func checkArrayHealth() (bool, string) {
	if cfg.RAID.ArrayDevice != "" {
		// Software RAID — check /proc/mdstat
		info, err := parseMdstat()
		if err != nil {
			logger.Printf("Array health check failed: %v", err)
			return false, ""
		}
		if strings.Contains(info.ArrayState, "degraded") {
			msg := fmt.Sprintf("Array %s is DEGRADED — %d/%d disks active [%s]",
				cfg.RAID.ArrayDevice, info.ActiveDisks, info.TotalDisks, info.DiskStatus)
			if info.IsRebuilding {
				msg += fmt.Sprintf(" — rebuild in progress: %s", info.RebuildPct)
			}
			return true, msg
		}
		return false, fmt.Sprintf("Array %s is healthy — %d/%d disks active [%s]",
			cfg.RAID.ArrayDevice, info.ActiveDisks, info.TotalDisks, info.DiskStatus)
	} else if isHardwareRAID() && hwRaidCLI != "" {
		// Hardware RAID — check virtual drive state via perccli64 /c0/v0 show
		// Updated: 2026-03-01 — Uses VD state line only, avoids false match on legend text
		stdout, _, err := runCommand(hwRaidCLI, "/c0/v0", "show")
		if err != nil {
			logger.Printf("HW RAID array health check failed: %v", err)
			return false, ""
		}
		// Parse only the data line (e.g. "0/0   RAID10 Optl  RW ...") not the legend
		isDegraded := false
		isRebuilding := false
		vdState := ""
		for _, line := range strings.Split(stdout, "\n") {
			trimmed := strings.TrimSpace(line)
			// VD data lines start with "digit/" (e.g. "0/0")
			if len(trimmed) > 2 && trimmed[0] >= '0' && trimmed[0] <= '9' && strings.Contains(trimmed, "/") {
				fields := strings.Fields(trimmed)
				if len(fields) >= 3 {
					vdState = fields[2] // State column: Optl, Dgrd, Pdgd, Rec, etc.
					if vdState == "Dgrd" || vdState == "Pdgd" {
						isDegraded = true
					}
					if vdState == "Rec" {
						isDegraded = true
						isRebuilding = true
					}
				}
				break
			}
		}
		if isDegraded {
			msg := fmt.Sprintf("Controller %s is DEGRADED (VD state: %s)", cfg.RAID.RAIDLevel, vdState)
			if isRebuilding {
				msg += " — rebuild in progress"
			}
			return true, msg
		}
		return false, fmt.Sprintf("Controller %s — all drives online (VD state: %s)", cfg.RAID.RAIDLevel, vdState)
	}
	return false, ""
}

// ==================== Startup Health Check ====================

// sendStartupEmail sends a daemon-started notification with current array/disk state.
// Created: 2026-02-11 — Confirms monitoring is active and reports initial array health
// Updated: 2026-02-12 — Supports servers without software RAID (PERC hardware RAID)
func sendStartupEmail() {
	subject := fmt.Sprintf("Gslmon Information from %s: Disk Health Monitor Report", cfg.Email.ServerName)

	var content strings.Builder

	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Monitoring Daemon Started</div>`)
	content.WriteString(`<p class="info-text">The gslmon disk health monitoring daemon has started successfully on <strong>`)
	content.WriteString(escHTML(cfg.Email.ServerName))
	content.WriteString(`</strong>.</p>`)

	content.WriteString(`<table>`)
	content.WriteString(`<tr><th>Parameter</th><th>Value</th></tr>`)

	// Show RAID info if software RAID is configured
	if cfg.RAID.ArrayDevice != "" {
		info, err := parseMdstat()
		if err != nil {
			logger.Printf("Failed to parse mdstat for startup email: %v", err)
		} else {
			content.WriteString(fmt.Sprintf(`<tr><td>Array</td><td>%s (%s)</td></tr>`, escHTML(cfg.RAID.ArrayDevice), escHTML(cfg.RAID.RAIDLevel)))
			content.WriteString(fmt.Sprintf(`<tr><td>Mount Point</td><td>%s</td></tr>`, escHTML(cfg.RAID.MountPoint)))
			content.WriteString(fmt.Sprintf(`<tr><td>Array State</td><td>%s</td></tr>`, escHTML(info.ArrayState)))
			content.WriteString(fmt.Sprintf(`<tr><td>Disk Status</td><td style="font-family:monospace;">[ %s ]</td></tr>`, escHTML(info.DiskStatus)))
			content.WriteString(fmt.Sprintf(`<tr><td>Active/Total</td><td>%d / %d</td></tr>`, info.ActiveDisks, info.TotalDisks))
			if info.Rebuild != "" {
				content.WriteString(fmt.Sprintf(`<tr><td>Rebuild</td><td>%s</td></tr>`, escHTML(info.Rebuild)))
			}
		}
	} else if cfg.RAID.RAIDLevel != "" {
		content.WriteString(fmt.Sprintf(`<tr><td>RAID Controller</td><td>%s</td></tr>`, escHTML(cfg.RAID.RAIDLevel)))
	}

	content.WriteString(fmt.Sprintf(`<tr><td>Log Check Interval</td><td>%d seconds</td></tr>`, cfg.Monitor.LogCheckIntervalSec))
	if cfg.RAID.ArrayDevice != "" {
		content.WriteString(fmt.Sprintf(`<tr><td>mdstat Check Interval</td><td>%d seconds</td></tr>`, cfg.Monitor.MdstatCheckIntervalSec))
	}
	content.WriteString(fmt.Sprintf(`<tr><td>SMART Test Interval</td><td>%d days</td></tr>`, cfg.Monitor.SmartTestIntervalDays))
	content.WriteString(fmt.Sprintf(`<tr><td>SMART Check Interval</td><td>%d hours</td></tr>`, cfg.Monitor.SmartCheckIntervalHrs))
	content.WriteString(fmt.Sprintf(`<tr><td>Alert Cooldown</td><td>%d minutes</td></tr>`, cfg.Monitor.AlertCooldownMin))
	content.WriteString(`</table>`)
	content.WriteString(`</div>`)

	// Member disks
	content.WriteString(`<div class="section">`)
	content.WriteString(`<div class="section-title">Monitored Disks</div>`)
	content.WriteString(`<table>`)
	content.WriteString(`<tr><th>Device</th><th>Model</th><th>Serial</th><th>SMART Health</th></tr>`)
	for _, disk := range cfg.RAID.MemberDisks {
		displayName := disk.Name
		if displayName == "" {
			displayName = disk.Device
		}
		model, serial, health := getQuickSmartInfoDisk(disk)
		content.WriteString(fmt.Sprintf(`<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			escHTML(displayName), escHTML(model), escHTML(serial), escHTML(health)))
	}
	content.WriteString(`</table>`)
	content.WriteString(`</div>`)

	// Raw mdstat (only for software RAID)
	if cfg.RAID.ArrayDevice != "" {
		info, _ := parseMdstat()
		if info != nil {
			content.WriteString(`<div class="section">`)
			content.WriteString(`<div class="section-title">Current /proc/mdstat</div>`)
			content.WriteString(`<div class="command-block">cat /proc/mdstat</div>`)
			content.WriteString(`<div class="log-block">` + escHTML(info.RawOutput) + `</div>`)
			content.WriteString(`</div>`)
		}
	}

	htmlBody := buildHTMLPage("Disk Health Monitor Started", "&#9881;", content.String())

	if err := sendEmail(subject, htmlBody); err != nil {
		logger.Printf("Failed to send startup email: %v", err)
	} else {
		logger.Printf("Startup notification email sent")
	}
}

// getQuickSmartInfoDisk fetches basic SMART info (model, serial, health) for a disk.
// Created: 2026-02-11 — Quick SMART identity check for startup report
// Updated: 2026-02-12 — Support megaraid pass-through device types
func getQuickSmartInfoDisk(disk DiskConfig) (model, serial, health string) {
	stdout, _, err := runSmartctl([]string{"-i", "-H"}, disk)
	if err != nil && stdout == "" {
		return "N/A", "N/A", "UNAVAILABLE"
	}

	model = "N/A"
	serial = "N/A"
	health = "N/A"

	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Device Model:") || strings.HasPrefix(trimmed, "Model Number:") {
			model = strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[1])
		}
		if strings.HasPrefix(trimmed, "Serial Number:") {
			serial = strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[1])
		}
		if strings.Contains(trimmed, "SMART overall-health") {
			health = strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[1])
		}
	}
	return
}

// ==================== Logging Setup ====================

// setupLogging initializes the log file and logger.
// Created: 2026-02-11 — Configures file-based logging for the daemon
func setupLogging() (*os.File, error) {
	dir := filepath.Dir(cfg.LogFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %w", err)
	}

	f, err := os.OpenFile(cfg.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	logger = log.New(f, "[gslmon] ", log.LstdFlags|log.Lshortfile)
	return f, nil
}

// ==================== PID File Locking ====================

// checkAndCreatePidFile ensures only one instance of gslmon runs at a time.
// If another instance is already running, it prints the existing PID and exits.
// Created: 2026-02-13 — Prevents duplicate processes from running simultaneously
func checkAndCreatePidFile(pidPath string) error {
	// Check if PID file already exists
	data, err := os.ReadFile(pidPath)
	if err == nil {
		// PID file exists — check if the process is still alive
		existingPid := strings.TrimSpace(string(data))
		if existingPid != "" {
			pid, err := strconv.Atoi(existingPid)
			if err == nil {
				// Check if process with this PID is running
				process, err := os.FindProcess(pid)
				if err == nil {
					// On Unix, FindProcess always succeeds; send signal 0 to check if alive
					err = process.Signal(syscall.Signal(0))
					if err == nil {
						return fmt.Errorf("gslmon is already running with PID %d. Refusing to start a duplicate instance", pid)
					}
				}
			}
		}
		// PID file exists but process is dead — remove stale PID file
		os.Remove(pidPath)
	}

	// Write our PID
	currentPid := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(currentPid)), 0644); err != nil {
		return fmt.Errorf("failed to write PID file %s: %w", pidPath, err)
	}
	return nil
}

// removePidFile cleans up the PID file on shutdown.
// Created: 2026-02-13 — Ensures PID file is removed when daemon stops gracefully
func removePidFile(pidPath string) {
	os.Remove(pidPath)
}

// ==================== Main ====================

func main() {
	// Determine config path from command-line argument (required)
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: gslmon <config.json>\n")
		fmt.Fprintf(os.Stderr, "Example: gslmon /etc/gslmon/config.json\n")
		os.Exit(1)
	}
	configPath := os.Args[1]

	// Load configuration
	if err := loadConfig(configPath); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Create directories
	os.MkdirAll(cfg.TmpDir, 0755)
	os.MkdirAll(filepath.Dir(cfg.StateFile), 0755)

	// PID file locking — prevent duplicate instances
	// Created: 2026-02-13 — Ensures only one gslmon process runs at a time
	pidPath := cfg.PidFile
	if pidPath == "" {
		pidPath = filepath.Join(filepath.Dir(cfg.LogFile), "gslmon.pid")
	}
	if err := checkAndCreatePidFile(pidPath); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}
	defer removePidFile(pidPath)

	// Setup logging
	logFile, err := setupLogging()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to setup logging: %v\n", err)
		os.Exit(1)
	}
	defer logFile.Close()

	logger.Printf("========================================")
	logger.Printf("gslmon Disk Health Monitor starting")
	logger.Printf("PID: %d (lock: %s)", os.Getpid(), pidPath)
	logger.Printf("Config: %s", configPath)
	if cfg.RAID.ArrayDevice != "" {
		logger.Printf("Array: %s (%s) on %s", cfg.RAID.ArrayDevice, cfg.RAID.RAIDLevel, cfg.RAID.MountPoint)
	} else {
		logger.Printf("RAID: %s (no software RAID — hardware controller)", cfg.RAID.RAIDLevel)
		hwRaidCLI = detectHWRaidCLI()
		if hwRaidCLI != "" {
			logger.Printf("HW RAID CLI tool: %s", hwRaidCLI)
		} else {
			logger.Printf("WARNING: No perccli64/storcli64 found — HW RAID rebuild monitoring disabled")
		}
	}
	diskNames := make([]string, len(cfg.RAID.MemberDisks))
	for i, d := range cfg.RAID.MemberDisks {
		if d.Name != "" {
			diskNames[i] = d.Name
		} else {
			diskNames[i] = d.Device
		}
	}
	logger.Printf("Disks: %s", strings.Join(diskNames, ", "))
	logger.Printf("Log check: %ds, SMART test: %dd, SMART check: %dh",
		cfg.Monitor.LogCheckIntervalSec,
		cfg.Monitor.SmartTestIntervalDays, cfg.Monitor.SmartCheckIntervalHrs)
	if cfg.RAID.ArrayDevice != "" {
		logger.Printf("mdstat check: %ds, Rebuild check: %dm",
			cfg.Monitor.MdstatCheckIntervalSec, cfg.Monitor.RebuildCheckIntervalMin)
	} else if isHardwareRAID() {
		logger.Printf("Rebuild check: %dm (hardware RAID)", cfg.Monitor.RebuildCheckIntervalMin)
	}
	logger.Printf("Alert cooldown: %d minutes", cfg.Monitor.AlertCooldownMin)
	logger.Printf("Email: %s -> %s via %s:%d",
		cfg.Email.From, cfg.Email.To, cfg.Email.SMTPServer, cfg.Email.SMTPPort)
	logger.Printf("========================================")

	// Load state
	loadState()

	// Signal handling for graceful shutdown
	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var wg sync.WaitGroup

	// Send startup notification
	sendStartupEmail()

	// Start monitoring goroutines
	// Updated: 2026-02-12 — Conditional goroutines based on RAID type
	// Updated: 2026-02-28 — Rebuild monitor always runs (dispatches to SW or HW path)
	goroutineCount := 3 // logs + smart + rebuild always run
	if cfg.RAID.ArrayDevice != "" {
		goroutineCount += 1 // mdstat only for software RAID
	}
	wg.Add(goroutineCount)
	go monitorLogs(stopCh, &wg)
	go monitorSmart(stopCh, &wg)
	go monitorRebuild(stopCh, &wg)
	if cfg.RAID.ArrayDevice != "" {
		go monitorMdstat(stopCh, &wg)
	}

	// Wait for shutdown signal
	sig := <-sigCh
	logger.Printf("Received signal %v, shutting down...", sig)
	close(stopCh)

	// Wait for goroutines with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Printf("All monitors stopped gracefully")
	case <-time.After(10 * time.Second):
		logger.Printf("Shutdown timed out, exiting")
	}

	saveState()
	logger.Printf("gslmon stopped")
}
