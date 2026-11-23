package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Query struct {
	Name string `yaml:"name"`
	SQL  string `yaml:"sql"`
	Min  int64  `yaml:"min"`
}

type Config struct {
	BackupPath        string  `yaml:"backup_path"`
	PostgresImage     string  `yaml:"postgres_image"`
	MinTables         int64   `yaml:"min_tables"`
	MaxRestoreSeconds int     `yaml:"max_restore_seconds"`
	Queries           []Query `yaml:"queries"`
	Notify            struct {
		SlackWebhook   string `yaml:"slack_webhook"`
		DiscordWebhook string `yaml:"discord_webhook"`
	} `yaml:"notify"`
}

var lines []string

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.BackupPath == "" {
		return nil, fmt.Errorf("backup_path is required")
	}
	if _, err := os.Stat(c.BackupPath); err != nil {
		return nil, fmt.Errorf("backup file not found: %s", c.BackupPath)
	}
	if c.PostgresImage == "" {
		c.PostgresImage = "postgres:16"
	}
	if c.MinTables == 0 {
		c.MinTables = 1
	}
	return &c, nil
}

func queryInt(container, sql string) (int64, error) {
	out, err := docker("exec", container, "psql", "-h", "localhost",
		"-U", "postgres", "-d", "restore_test", "-tAc", sql)
	if err != nil {
		return 0, fmt.Errorf("query failed: %s", out)
	}
	return strconv.ParseInt(out, 10, 64)
}

func report(failed *bool, name string, ok bool, detail string) {
	mark := "[OK]  "
	if !ok {
		mark = "[FAIL]"
		*failed = true
	}
	line := fmt.Sprintf("%s %s: %s", mark, name, detail)
	fmt.Println(line)
	lines = append(lines, line)
}

// يرسل JSON لرابط webhook
func postJSON(url string, payload map[string]string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	return nil
}

func notify(cfg *Config, ok bool) {
	title := "✅ provebackup: backup OK"
	if !ok {
		title = "❌ provebackup: BACKUP FAILED"
	}
	text := title + "\n```\n" + strings.Join(lines, "\n") + "\n```"
	if len(text) > 1900 { // حد Discord 2000 حرف
		text = text[:1900]
	}

	if u := cfg.Notify.SlackWebhook; u != "" {
		if err := postJSON(u, map[string]string{"text": text}); err != nil {
			fmt.Println("slack error:", err)
		}
	}
	if u := cfg.Notify.DiscordWebhook; u != "" {
		if err := postJSON(u, map[string]string{"content": text}); err != nil {
			fmt.Println("discord error:", err)
		}
	}
}

func run() int {
	cfg, err := loadConfig("provebackup.yaml")
	if err != nil {
		fmt.Println("config error:", err)
		return 2
	}

	name := fmt.Sprintf("provebackup-%d", time.Now().UnixNano())

	fmt.Println("Starting", cfg.PostgresImage, "...")
	out, err := docker("run", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD=pw",
		"-e", "POSTGRES_DB=restore_test",
		cfg.PostgresImage)
	if err != nil {
		fmt.Println("failed to start:", out)
		return 2
	}
	defer docker("rm", "-f", "-v", name)

	fmt.Println("Waiting for Postgres...")
	ready := false
	for i := 0; i < 60; i++ {
		_, err := docker("exec", name, "pg_isready", "-h", "localhost", "-U", "postgres", "-d", "restore_test")
		if err == nil {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		fmt.Println("Postgres did not become ready in time")
		return 2
	}

	out, err = docker("cp", cfg.BackupPath, name+":/tmp/backup.dump")
	if err != nil {
		fmt.Println("cannot copy backup:", out)
		return 2
	}

	fmt.Println("Restoring backup...")
	failed := false

	start := time.Now()
	out, err = docker("exec", name, "pg_restore", "-h", "localhost",
		"-U", "postgres", "-d", "restore_test",
		"--no-owner", "--exit-on-error", "/tmp/backup.dump")
	elapsed := time.Since(start)

	if err != nil {
		report(&failed, "restore", false, out)
		fmt.Println("\nBACKUP FAILED")
		notify(cfg, false)
		return 1
	}
	report(&failed, "restore", true, "completed in "+elapsed.Round(time.Millisecond).String())

	if cfg.MaxRestoreSeconds > 0 {
		limit := time.Duration(cfg.MaxRestoreSeconds) * time.Second
		report(&failed, "restore time", elapsed <= limit,
			fmt.Sprintf("%s (limit %ds)", elapsed.Round(time.Millisecond), cfg.MaxRestoreSeconds))
	}

	n, err := queryInt(name, "SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema')")
	if err != nil {
		report(&failed, "tables", false, err.Error())
	} else {
		report(&failed, "tables", n >= cfg.MinTables, fmt.Sprintf("%d found (min %d)", n, cfg.MinTables))
	}

	for _, q := range cfg.Queries {
		v, err := queryInt(name, q.SQL)
		if err != nil {
			report(&failed, q.Name, false, err.Error())
			continue
		}
		report(&failed, q.Name, v >= q.Min, fmt.Sprintf("got %d (min %d)", v, q.Min))
	}

	notify(cfg, !failed)

	if failed {
		fmt.Println("\nBACKUP FAILED")
		return 1
	}
	fmt.Println("\nBACKUP OK")
	return 0
}

func main() {
	os.Exit(run())
}
