package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	sql *sql.DB
}

type Account struct {
	ID            string
	Provider      string
	Name          string
	Priority      int
	Enabled       bool
	NeedsReauth   bool
	AccessToken   string
	RefreshToken  string
	ExpiresAt     time.Time
	CooldownUntil sql.NullTime
	MetadataJSON  string
}

type APIKey struct {
	ID        string
	Name      string
	Prefix    string
	Secret    string
	CreatedAt time.Time
}

type Settings struct {
	Host                   string
	Port                   int
	LogRequests            bool
	LogUpstream            bool
	LogBodyLimit           int
	DefaultModel           string
	UpstreamTimeoutSeconds int
}

func DefaultSettings() Settings {
	return Settings{
		Host:                   "127.0.0.1",
		Port:                   19090,
		LogRequests:            true,
		LogUpstream:            true,
		LogBodyLimit:           64 * 1024,
		DefaultModel:           "gpt-5.3-codex",
		UpstreamTimeoutSeconds: 300,
	}
}

func Open(ctx context.Context, dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, "lm-router.db")
	file, err := os.OpenFile(dbPath, os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	_ = file.Close()

	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db := &DB{sql: sqlDB}
	if err := db.init(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) Close() error {
	return db.sql.Close()
}

func (db *DB) init(ctx context.Context) error {
	pragmas := []string{
		`pragma busy_timeout = 5000;`,
		`pragma journal_mode = wal;`,
	}
	for _, stmt := range pragmas {
		if _, err := db.sql.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	stmts := []string{
		`create table if not exists accounts (
			id text primary key,
			provider text not null,
			name text not null,
			priority integer not null default 0,
			enabled integer not null default 1,
			needs_reauth integer not null default 0,
			access_token text not null default '',
			refresh_token text not null default '',
			expires_at text not null default '',
			cooldown_until text,
			metadata_json text not null default '{}'
		);`,
		`create table if not exists api_keys (
			id text primary key,
			name text not null,
			prefix text not null,
			secret_hash text not null,
			created_at text not null
		);`,
		`create table if not exists settings (
			key text primary key,
			value text not null
		);`,
	}
	for _, stmt := range stmts {
		if _, err := db.sql.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) GetSettings(ctx context.Context) (Settings, error) {
	settings := DefaultSettings()
	rows, err := db.sql.QueryContext(ctx, `select key, value from settings`)
	if err != nil {
		return Settings{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return Settings{}, err
		}
		switch key {
		case "host":
			if value != "" {
				settings.Host = value
			}
		case "port":
			var port int
			if _, err := fmt.Sscanf(value, "%d", &port); err == nil && port > 0 {
				settings.Port = port
			}
		case "log_requests":
			settings.LogRequests = value != "false"
		case "log_upstream":
			settings.LogUpstream = value != "false"
		case "log_body_limit":
			var limit int
			if _, err := fmt.Sscanf(value, "%d", &limit); err == nil && limit > 0 {
				settings.LogBodyLimit = limit
			}
		case "default_model":
			if value != "" {
				settings.DefaultModel = value
			}
		case "upstream_timeout_seconds":
			var t int
			if _, err := fmt.Sscanf(value, "%d", &t); err == nil && t > 0 {
				settings.UpstreamTimeoutSeconds = t
			}
		}
	}
	return settings, rows.Err()
}

func (db *DB) SaveSettings(ctx context.Context, settings Settings) error {
	if settings.Host == "" {
		settings.Host = "127.0.0.1"
	}
	if settings.Port <= 0 {
		settings.Port = 19090
	}
	if settings.LogBodyLimit <= 0 {
		settings.LogBodyLimit = 64 * 1024
	}
	if settings.DefaultModel == "" {
		settings.DefaultModel = "gpt-5.3-codex"
	}
	if settings.UpstreamTimeoutSeconds <= 0 {
		settings.UpstreamTimeoutSeconds = 300
	}
	values := map[string]string{
		"host":                     settings.Host,
		"port":                     fmt.Sprintf("%d", settings.Port),
		"log_requests":             fmt.Sprintf("%t", settings.LogRequests),
		"log_upstream":             fmt.Sprintf("%t", settings.LogUpstream),
		"log_body_limit":           fmt.Sprintf("%d", settings.LogBodyLimit),
		"default_model":            settings.DefaultModel,
		"upstream_timeout_seconds": fmt.Sprintf("%d", settings.UpstreamTimeoutSeconds),
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	for key, value := range values {
		if _, err = tx.ExecContext(ctx, `
			insert into settings (key, value) values (?, ?)
			on conflict(key) do update set value=excluded.value
		`, key, value); err != nil {
			return err
		}
	}
	err = tx.Commit()
	return err
}

func (db *DB) UpsertAccount(ctx context.Context, account Account) error {
	_, err := db.sql.ExecContext(ctx, `
		insert into accounts (
			id, provider, name, priority, enabled, needs_reauth, access_token,
			refresh_token, expires_at, cooldown_until, metadata_json
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(id) do update set
			provider=excluded.provider,
			name=excluded.name,
			priority=excluded.priority,
			enabled=excluded.enabled,
			needs_reauth=excluded.needs_reauth,
			access_token=excluded.access_token,
			refresh_token=excluded.refresh_token,
			expires_at=excluded.expires_at,
			cooldown_until=excluded.cooldown_until,
			metadata_json=excluded.metadata_json
	`,
		account.ID,
		account.Provider,
		account.Name,
		account.Priority,
		boolToInt(account.Enabled),
		boolToInt(account.NeedsReauth),
		account.AccessToken,
		account.RefreshToken,
		account.ExpiresAt.UTC().Format(time.RFC3339Nano),
		nullTimeValue(account.CooldownUntil),
		defaultString(account.MetadataJSON, "{}"),
	)
	return err
}

func (db *DB) GetAccount(ctx context.Context, id string) (Account, error) {
	row := db.sql.QueryRowContext(ctx, `
		select id, provider, name, priority, enabled, needs_reauth, access_token,
			refresh_token, expires_at, cooldown_until, metadata_json
		from accounts where id = ?
	`, id)
	return scanAccount(row)
}

func (db *DB) GetAccountByName(ctx context.Context, name string) (Account, error) {
	row := db.sql.QueryRowContext(ctx, `
		select id, provider, name, priority, enabled, needs_reauth, access_token,
			refresh_token, expires_at, cooldown_until, metadata_json
		from accounts where name = ?
		order by priority asc
		limit 1
	`, name)
	return scanAccount(row)
}

func (db *DB) ListRoutableAccounts(ctx context.Context, provider string) ([]Account, error) {
	return db.listAccounts(ctx, `where provider = ? and enabled = 1 and needs_reauth = 0`, provider)
}

func (db *DB) ListAccounts(ctx context.Context) ([]Account, error) {
	return db.listAccounts(ctx, ``, nil)
}

func (db *DB) listAccounts(ctx context.Context, where string, arg any) ([]Account, error) {
	query := `
		select id, provider, name, priority, enabled, needs_reauth, access_token,
			refresh_token, expires_at, cooldown_until, metadata_json
		from accounts
	`
	if where != "" {
		query += " " + where
	}
	var (
		rows *sql.Rows
		err  error
	)
	if arg != nil {
		rows, err = db.sql.QueryContext(ctx, query, arg)
	} else {
		rows, err = db.sql.QueryContext(ctx, query)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	accounts := make([]Account, 0)
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	sort.Slice(accounts, func(i, j int) bool {
		return accounts[i].Priority < accounts[j].Priority
	})
	return accounts, rows.Err()
}

func (db *DB) DeleteAccount(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `delete from accounts where id = ?`, id)
	return err
}

func (db *DB) RenameAccount(ctx context.Context, id, name string) error {
	_, err := db.sql.ExecContext(ctx, `update accounts set name = ? where id = ?`, name, id)
	return err
}

func (db *DB) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := db.sql.QueryContext(ctx, `select id, name, prefix, created_at from api_keys order by created_at asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]APIKey, 0)
	for rows.Next() {
		var key APIKey
		var createdAt string
		if err := rows.Scan(&key.ID, &key.Name, &key.Prefix, &createdAt); err != nil {
			return nil, err
		}
		key.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

func (db *DB) DeleteAPIKey(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `delete from api_keys where id = ?`, id)
	return err
}

func (db *DB) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := db.sql.ExecContext(ctx, `update accounts set enabled = ? where id = ?`, boolToInt(enabled), id)
	return err
}

func (db *DB) SetAccountPriority(ctx context.Context, id string, priority int) error {
	_, err := db.sql.ExecContext(ctx, `update accounts set priority = ? where id = ?`, priority, id)
	return err
}

func (db *DB) NextPriority(ctx context.Context) (int, error) {
	var next sql.NullInt64
	if err := db.sql.QueryRowContext(ctx, `select coalesce(max(priority), 0) + 1 from accounts`).Scan(&next); err != nil {
		return 0, err
	}
	return int(next.Int64), nil
}

func (db *DB) SetCooldown(ctx context.Context, id string, until time.Time) error {
	_, err := db.sql.ExecContext(ctx, `update accounts set cooldown_until = ? where id = ?`, until.UTC().Format(time.RFC3339Nano), id)
	return err
}

func (db *DB) MarkNeedsReauth(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `update accounts set needs_reauth = 1 where id = ?`, id)
	return err
}

func (db *DB) UpdateTokens(ctx context.Context, id, accessToken, refreshToken string, expiresAt time.Time) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `
		update accounts
		set access_token = ?, refresh_token = ?, expires_at = ?, needs_reauth = 0
		where id = ?
	`, accessToken, refreshToken, expiresAt.UTC().Format(time.RFC3339Nano), id); err != nil {
		return err
	}
	err = tx.Commit()
	return err
}

func (db *DB) CreateAPIKey(ctx context.Context, name string) (APIKey, error) {
	secret, err := randomToken("sk-lm-router-", 24)
	if err != nil {
		return APIKey{}, err
	}
	key := APIKey{
		ID:        mustRandomID("key"),
		Name:      name,
		Prefix:    secret[:min(len(secret), 18)],
		Secret:    secret,
		CreatedAt: time.Now().UTC(),
	}
	_, err = db.sql.ExecContext(ctx, `
		insert into api_keys (id, name, prefix, secret_hash, created_at)
		values (?, ?, ?, ?, ?)
	`, key.ID, key.Name, key.Prefix, hashSecret(secret), key.CreatedAt.Format(time.RFC3339Nano))
	return key, err
}

func (db *DB) ValidateAPIKey(ctx context.Context, secret string) bool {
	if secret == "" {
		return false
	}
	var count int
	err := db.sql.QueryRowContext(ctx, `select count(1) from api_keys where secret_hash = ?`, hashSecret(secret)).Scan(&count)
	return err == nil && count > 0
}

type accountScanner interface {
	Scan(dest ...any) error
}

func scanAccount(scanner accountScanner) (Account, error) {
	var account Account
	var enabled int
	var needsReauth int
	var expiresAt string
	var cooldown sql.NullString
	err := scanner.Scan(
		&account.ID,
		&account.Provider,
		&account.Name,
		&account.Priority,
		&enabled,
		&needsReauth,
		&account.AccessToken,
		&account.RefreshToken,
		&expiresAt,
		&cooldown,
		&account.MetadataJSON,
	)
	if err != nil {
		return Account{}, err
	}
	account.Enabled = enabled == 1
	account.NeedsReauth = needsReauth == 1
	if expiresAt != "" {
		account.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
		if err != nil {
			return Account{}, err
		}
	}
	if cooldown.Valid && cooldown.String != "" {
		tm, err := time.Parse(time.RFC3339Nano, cooldown.String)
		if err != nil {
			return Account{}, err
		}
		account.CooldownUntil = sql.NullTime{Time: tm, Valid: true}
	}
	return account, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func nullTimeValue(v sql.NullTime) any {
	if !v.Valid {
		return nil
	}
	return v.Time.UTC().Format(time.RFC3339Nano)
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomToken(prefix string, bytesLen int) (string, error) {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buf), nil
}

func mustRandomID(prefix string) string {
	token, err := randomToken(prefix+"_", 8)
	if err != nil {
		panic(err)
	}
	return token
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var ErrAccountNotFound = errors.New("account not found")

func (db *DB) MustGetAccount(ctx context.Context, id string) (Account, error) {
	account, err := db.GetAccount(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrAccountNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("get account %s: %w", id, err)
	}
	return account, nil
}
