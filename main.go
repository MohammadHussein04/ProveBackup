package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

type S3Config struct {
	Bucket    string `yaml:"bucket"`
	Key       string `yaml:"key"`
	Region    string `yaml:"region"`
	Endpoint  string `yaml:"endpoint"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
}

type Config struct {
	Engine            string   `yaml:"engine"`
	BackupPath        string   `yaml:"backup_path"`
	BackupSource      string   `yaml:"backup_source"`
	S3                S3Config `yaml:"s3"`
	Image             string   `yaml:"image"`
	PostgresImage     string   `yaml:"postgres_image"`
	MinTables         int64    `yaml:"min_tables"`
	MaxRestoreSeconds int      `yaml:"max_restore_seconds"`
	Queries           []Query  `yaml:"queries"`
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

func dockerStdout(args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v: %s", err, short(stderr.String()+stdout.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		s = s[:500] + "..."
	}
	return s
}

type engine struct {
	runEnv    []string
	execEnv   []string
	ready     []string
	restore   []string
	query     func(sql string) []string
	tablesSQL string
}

func newEngine(kind string) *engine {
	if kind == "mysql" {
		return &engine{
			runEnv:  []string{"MYSQL_ROOT_PASSWORD=pw", "MYSQL_DATABASE=restore_test"},
			execEnv: []string{"MYSQL_PWD=pw"},
			ready:   []string{"mysqladmin", "ping", "-h", "127.0.0.1", "-uroot", "--silent"},
			restore: []string{"sh", "-c", "mysql -h 127.0.0.1 -uroot restore_test < /tmp/backup.dump"},
			query: func(sql string) []string {
				return []string{"mysql", "-h", "127.0.0.1", "-uroot", "-N", "-B", "restore_test", "-e", sql}
			},
			tablesSQL: "SELECT count(*) FROM information_schema.tables WHERE table_schema='restore_test'",
		}
	}
	return &engine{
		runEnv: []string{"POSTGRES_PASSWORD=pw", "POSTGRES_DB=restore_test"},
		ready:  []string{"pg_isready", "-h", "localhost", "-U", "postgres", "-d", "restore_test"},
		restore: []string{"pg_restore", "-h", "localhost", "-U", "postgres", "-d", "restore_test",
			"--no-owner", "--exit-on-error", "/tmp/backup.dump"},
		query: func(sql string) []string {
			return []string{"psql", "-h", "localhost", "-U", "postgres", "-d", "restore_test", "-tAc", sql}
		},
		tablesSQL: "SELECT count(*) FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema')",
	}
}

func (e *engine) execArgs(container string, cmd []string) []string {
	args := []string{"exec"}
	for _, v := range e.execEnv {
		args = append(args, "-e", v)
	}
	args = append(args, container)
	return append(args, cmd...)
}

func (e *engine) queryInt(container, sql string) (int64, error) {
	out, err := dockerStdout(e.execArgs(container, e.query(sql))...)
	if err != nil {
		return 0, fmt.Errorf("query failed: %v", err)
	}
	return strconv.ParseInt(out, 10, 64)
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

	if c.Engine == "" {
		c.Engine = "postgres"
	}
	if c.Engine != "postgres" && c.Engine != "mysql" {
		return nil, fmt.Errorf("engine must be postgres or mysql, got %q", c.Engine)
	}
	if c.BackupPath == "" {
		return nil, fmt.Errorf("backup_path is required")
	}
	if c.BackupSource != "" && c.S3.Bucket != "" {
		return nil, fmt.Errorf("use either backup_source or s3, not both")
	}
	if c.S3.Bucket != "" {
		if c.S3.Key == "" {
			return nil, fmt.Errorf("s3.key is required")
		}
		if c.S3.Region == "" {
			c.S3.Region = "us-east-1"
		}
	}

	if c.BackupSource == "" && c.S3.Bucket == "" {
		if _, err := os.Stat(c.BackupPath); err != nil {
			return nil, fmt.Errorf("backup file not found: %s", c.BackupPath)
		}
	}

	if c.Image == "" {
		c.Image = c.PostgresImage
	}
	if c.Image == "" {
		if c.Engine == "mysql" {
			c.Image = "mysql:8"
		} else {
			c.Image = "postgres:16"
		}
	}
	if c.MinTables == 0 {
		c.MinTables = 1
	}
	return &c, nil
}

func takeBackup(cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(cfg.BackupPath), 0o755); err != nil {
		return err
	}

	args := []string{"run", "--rm", "--add-host=host.docker.internal:host-gateway"}
	if cfg.Engine == "mysql" {
		u, err := url.Parse(cfg.BackupSource)
		if err != nil || u.Hostname() == "" {
			return fmt.Errorf("backup_source must look like mysql://user:pass@host:3306/dbname")
		}
		pass, _ := u.User.Password()
		port := u.Port()
		if port == "" {
			port = "3306"
		}
		args = append(args, "-e", "MYSQL_PWD="+pass, cfg.Image,
			"mysqldump", "-h", u.Hostname(), "-P", port, "-u", u.User.Username(),
			"--single-transaction", "--no-tablespaces", strings.TrimPrefix(u.Path, "/"))
	} else {
		args = append(args, cfg.Image, "pg_dump", "-Fc", cfg.BackupSource)
	}

	f, err := os.Create(cfg.BackupPath)
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout = f
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	f.Close()
	if runErr != nil {
		os.Remove(cfg.BackupPath)
		return fmt.Errorf("%v: %s", runErr, short(stderr.String()))
	}
	return nil
}

func downloadS3(cfg *Config) error {
	s := cfg.S3
	name := fmt.Sprintf("provebackup-s3-%d", time.Now().UnixNano())

	args := []string{"run", "--name", name,
		"--add-host=host.docker.internal:host-gateway",
		"-e", "AWS_DEFAULT_REGION=" + s.Region}
	if s.AccessKey != "" {
		args = append(args, "-e", "AWS_ACCESS_KEY_ID="+s.AccessKey)
	} else {
		args = append(args, "-e", "AWS_ACCESS_KEY_ID")
	}
	if s.SecretKey != "" {
		args = append(args, "-e", "AWS_SECRET_ACCESS_KEY="+s.SecretKey)
	} else {
		args = append(args, "-e", "AWS_SECRET_ACCESS_KEY")
	}
	args = append(args, "amazon/aws-cli")
	if s.Endpoint != "" {
		args = append(args, "--endpoint-url", s.Endpoint)
	}
	args = append(args, "s3", "cp", "s3://"+s.Bucket+"/"+s.Key, "/tmp/backup.dump")

	defer docker("rm", "-f", name)
	if out, err := docker(args...); err != nil {
		return fmt.Errorf("s3 download failed: %s", short(out))
	}
	if err := os.MkdirAll(filepath.Dir(cfg.BackupPath), 0o755); err != nil {
		return err
	}
	if out, err := docker("cp", name+":/tmp/backup.dump", cfg.BackupPath); err != nil {
		return fmt.Errorf("cannot copy downloaded backup: %s", short(out))
	}
	return nil
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
	if len(text) > 1900 {
		text = text[:1900] + "\n```"
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

	toolError := func(msg string) int {
		fmt.Println(msg)
		lines = append(lines, "[ERROR] "+msg)
		notify(cfg, false)
		return 2
	}

	switch {
	case cfg.BackupSource != "":
		fmt.Println("Taking backup...")
		if err := takeBackup(cfg); err != nil {
			return toolError("backup failed: " + err.Error())
		}
	case cfg.S3.Bucket != "":
		fmt.Println("Downloading backup from S3...")
		if err := downloadS3(cfg); err != nil {
			return toolError(err.Error())
		}
	}

	eng := newEngine(cfg.Engine)
	name := fmt.Sprintf("provebackup-%d", time.Now().UnixNano())

	fmt.Println("Starting", cfg.Image, "...")
	runArgs := []string{"run", "-d", "--name", name}
	for _, v := range eng.runEnv {
		runArgs = append(runArgs, "-e", v)
	}
	runArgs = append(runArgs, cfg.Image)
	out, err := docker(runArgs...)
	if err != nil {
		return toolError("failed to start container: " + short(out))
	}
	defer docker("rm", "-f", "-v", name)

	fmt.Println("Waiting for database...")
	ready := false
	for i := 0; i < 120; i++ {
		if _, err := docker(eng.execArgs(name, eng.ready)...); err == nil {
			ready = true
			break
		}
		time.Sleep(time.Second)
	}
	if !ready {
		return toolError("database did not become ready in time")
	}

	out, err = docker("cp", cfg.BackupPath, name+":/tmp/backup.dump")
	if err != nil {
		return toolError("cannot copy backup: " + short(out))
	}

	fmt.Println("Restoring backup...")
	failed := false

	start := time.Now()
	out, err = docker(eng.execArgs(name, eng.restore)...)
	elapsed := time.Since(start)

	if err != nil {
		report(&failed, "restore", false, short(out))
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

	n, err := eng.queryInt(name, eng.tablesSQL)
	if err != nil {
		report(&failed, "tables", false, err.Error())
	} else {
		report(&failed, "tables", n >= cfg.MinTables, fmt.Sprintf("%d found (min %d)", n, cfg.MinTables))
	}

	for _, q := range cfg.Queries {
		v, err := eng.queryInt(name, q.SQL)
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
