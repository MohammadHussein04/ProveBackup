# ProveBackup

> A backup you never restored is not a backup.

ProveBackup restores your latest PostgreSQL backup inside a throwaway Docker container, runs sanity checks, and tells you whether the backup is actually usable.

## Why
Most teams make backups but never test them, and find out they are broken on the worst possible day. ProveBackup tests them automatically, before that day.

## How it works
```
dump file -> fresh Postgres container -> pg_restore -> checks -> report -> container deleted
```

- Measures restore time
- Fails on any restore error (`--exit-on-error`)
- Runs your own SQL checks (e.g. "users table has at least 1 row")
- Sends the result to Slack and/or Discord
- Exit code `0` = backup OK, `1` = backup bad, `2` = tool/config error

## Requirements
- Go (version in `go.mod`)
- Docker

## Quick start
```bash
git clone https://github.com/MohammadHussein04/ProveBackup.git
cd ProveBackup
cp provebackup.example.yaml provebackup.yaml
go mod tidy
go run .
```

Create a test dump (works on Windows too):
```bash
docker run -d --name testdb -e POSTGRES_PASSWORD=pw -e POSTGRES_DB=mydb postgres:16
docker exec testdb psql -U postgres -d mydb -c "CREATE TABLE users(id serial, name text); INSERT INTO users(name) VALUES ('a'),('b'),('c');"
docker exec testdb pg_dump -U postgres -Fc -f /tmp/latest.dump mydb
mkdir backups
docker cp testdb:/tmp/latest.dump ./backups/latest.dump
docker rm -f testdb
```

## Take a fresh backup automatically (optional)
Add `backup_source` and ProveBackup will run `pg_dump` for you before verifying:

```yaml
backup_path: backups/auto.dump
backup_source: "postgres://user:password@host:5432/dbname"
```
If `backup_source` is omitted, ProveBackup just verifies the existing file at `backup_path`.

## Example output
```
Restoring backup...
[OK]   restore: completed in 206ms
[OK]   tables: 2 found (min 2)
[OK]   users exist: got 3 (min 3)

BACKUP OK
```
With a corrupted backup:
```
[FAIL] restore: pg_restore: error: ...

BACKUP FAILED
```

## Configuration
See [`provebackup.example.yaml`](provebackup.example.yaml). Never commit your real `provebackup.yaml`: it contains webhook URLs and database credentials.

## Roadmap
- [x] PostgreSQL restore + custom SQL checks
- [x] Slack / Discord alerts
- [x] Optional automatic backup (`pg_dump`)
- [x] Scheduling (Windows Task Scheduler / cron)
- [ ] Fetch backups from S3
- [ ] MySQL support
- [ ] Hosted version
