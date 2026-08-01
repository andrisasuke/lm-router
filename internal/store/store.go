package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	sql *sql.DB
}

const (
	ProviderOpenAICodex     = "openai-codex"
	ProviderAnthropicClaude = "anthropic-claude"
)

func CanonicalProvider(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ProviderOpenAICodex, "codex":
		return ProviderOpenAICodex, nil
	case ProviderAnthropicClaude, "claude":
		return ProviderAnthropicClaude, nil
	default:
		return "", fmt.Errorf("unsupported provider %q", raw)
	}
}

type Account struct {
	ID                  string
	Provider            string
	Name                string
	Priority            int
	Enabled             bool
	NeedsReauth         bool
	AccessToken         string
	RefreshToken        string
	ExpiresAt           time.Time
	ConsecutiveFailures int
	LastFailureAt       sql.NullTime
	CooldownUntil       sql.NullTime
	MetadataJSON        string
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
	// SQLite PRAGMAs such as busy_timeout are connection-local. A single shared
	// connection keeps them effective and serializes the small state updates
	// used by concurrent routing decisions.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
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
			consecutive_failures integer not null default 0,
			last_failure_at text,
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
	for _, migration := range []struct {
		name       string
		definition string
	}{
		{name: "consecutive_failures", definition: "integer not null default 0"},
		{name: "last_failure_at", definition: "text"},
		{name: "cooldown_until", definition: "text"},
	} {
		if err := db.ensureColumn(ctx, "accounts", migration.name, migration.definition); err != nil {
			return err
		}
	}
	return db.migrateLegacyAccountAliases(ctx)
}

func (db *DB) ensureColumn(ctx context.Context, table, name, definition string) error {
	rows, err := db.sql.QueryContext(ctx, "pragma table_info("+table+")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if columnName == name {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.sql.ExecContext(ctx, fmt.Sprintf("alter table %s add column %s %s", table, name, definition))
	return err
}

// migrateLegacyAccountAliases makes aliases unique within each provider before
// the provider-scoped uniqueness rule is enforced. Older databases allowed
// duplicates (the CLI commonly created several accounts named "openai-codex"),
// which otherwise prevents a later re-authentication from saving fresh tokens.
func (db *DB) migrateLegacyAccountAliases(ctx context.Context) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	type aliasRow struct {
		id       string
		provider string
		name     string
	}
	rows, err := tx.QueryContext(ctx, `
		select id, provider, name
		from accounts
		order by provider asc, lower(trim(name)) asc, priority asc, id asc
	`)
	if err != nil {
		return err
	}
	var accounts []aliasRow
	for rows.Next() {
		var account aliasRow
		if err := rows.Scan(&account.id, &account.provider, &account.name); err != nil {
			_ = rows.Close()
			return err
		}
		accounts = append(accounts, account)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	used := make(map[string]map[string]bool)
	for _, account := range accounts {
		base := strings.TrimSpace(account.name)
		if base == "" {
			base = account.provider
		}
		if used[account.provider] == nil {
			used[account.provider] = make(map[string]bool)
		}
		used[account.provider][strings.ToLower(base)] = true
	}

	kept := make(map[string]map[string]bool)
	for _, account := range accounts {
		base := strings.TrimSpace(account.name)
		if base == "" {
			base = account.provider
		}
		if kept[account.provider] == nil {
			kept[account.provider] = make(map[string]bool)
		}
		key := strings.ToLower(base)
		name := base
		if kept[account.provider][key] {
			for suffix := 2; ; suffix++ {
				candidate := fmt.Sprintf("%s-%d", base, suffix)
				candidateKey := strings.ToLower(candidate)
				if !used[account.provider][candidateKey] {
					name = candidate
					used[account.provider][candidateKey] = true
					break
				}
			}
		} else {
			kept[account.provider][key] = true
		}
		if name != account.name {
			if _, err := tx.ExecContext(ctx, `update accounts set name = ? where id = ?`, name, account.id); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
		create unique index if not exists accounts_provider_name_nocase
		on accounts(provider, name collate nocase)
	`); err != nil {
		return err
	}
	return tx.Commit()
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
	provider, err := CanonicalProvider(account.Provider)
	if err != nil {
		return err
	}
	account.Provider = provider
	account.Name = strings.TrimSpace(account.Name)
	if account.Name == "" {
		return errors.New("connection alias is required")
	}
	if err := db.ensureAccountNameAvailable(ctx, account.Provider, account.Name, account.ID); err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx, `
		insert into accounts (
			id, provider, name, priority, enabled, needs_reauth, access_token,
			refresh_token, expires_at, consecutive_failures, last_failure_at,
			cooldown_until, metadata_json
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		on conflict(id) do update set
			provider=excluded.provider,
			name=excluded.name,
			priority=excluded.priority,
			enabled=excluded.enabled,
			needs_reauth=excluded.needs_reauth,
			access_token=excluded.access_token,
			refresh_token=excluded.refresh_token,
			expires_at=excluded.expires_at,
			consecutive_failures=excluded.consecutive_failures,
			last_failure_at=excluded.last_failure_at,
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
		account.ConsecutiveFailures,
		nullTimeValue(account.LastFailureAt),
		nullTimeValue(account.CooldownUntil),
		defaultString(account.MetadataJSON, "{}"),
	)
	return err
}

func (db *DB) GetAccount(ctx context.Context, id string) (Account, error) {
	row := db.sql.QueryRowContext(ctx, `
		select id, provider, name, priority, enabled, needs_reauth, access_token,
			refresh_token, expires_at, consecutive_failures, last_failure_at,
			cooldown_until, metadata_json
		from accounts where id = ?
	`, id)
	return scanAccount(row)
}

func (db *DB) GetAccountByName(ctx context.Context, name string) (Account, error) {
	row := db.sql.QueryRowContext(ctx, `
		select id, provider, name, priority, enabled, needs_reauth, access_token,
			refresh_token, expires_at, consecutive_failures, last_failure_at,
			cooldown_until, metadata_json
		from accounts where name = ?
		order by priority asc
		limit 1
	`, name)
	return scanAccount(row)
}

func (db *DB) GetAccountByProviderAndName(ctx context.Context, provider, name string) (Account, error) {
	provider, err := CanonicalProvider(provider)
	if err != nil {
		return Account{}, err
	}
	row := db.sql.QueryRowContext(ctx, `
		select id, provider, name, priority, enabled, needs_reauth, access_token,
			refresh_token, expires_at, consecutive_failures, last_failure_at,
			cooldown_until, metadata_json
		from accounts where provider = ? and lower(name) = lower(?)
		order by priority asc
		limit 1
	`, provider, strings.TrimSpace(name))
	return scanAccount(row)
}

func (db *DB) ListRoutableAccounts(ctx context.Context, provider string) ([]Account, error) {
	provider, err := CanonicalProvider(provider)
	if err != nil {
		return nil, err
	}
	return db.listAccounts(ctx, `where provider = ? and enabled = 1 and needs_reauth = 0`, provider)
}

func (db *DB) ListAccounts(ctx context.Context) ([]Account, error) {
	return db.listAccounts(ctx, ``, nil)
}

func (db *DB) ListAccountsByProvider(ctx context.Context, provider string) ([]Account, error) {
	provider, err := CanonicalProvider(provider)
	if err != nil {
		return nil, err
	}
	return db.listAccounts(ctx, `where provider = ?`, provider)
}

func (db *DB) listAccounts(ctx context.Context, where string, arg any) ([]Account, error) {
	query := `
		select id, provider, name, priority, enabled, needs_reauth, access_token,
			refresh_token, expires_at, consecutive_failures, last_failure_at,
			cooldown_until, metadata_json
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
		if accounts[i].Priority == accounts[j].Priority {
			return accounts[i].ID < accounts[j].ID
		}
		return accounts[i].Priority < accounts[j].Priority
	})
	return accounts, rows.Err()
}

func (db *DB) DeleteAccount(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `delete from accounts where id = ?`, id)
	return err
}

func (db *DB) RenameAccount(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("connection alias is required")
	}
	account, err := db.MustGetAccount(ctx, id)
	if err != nil {
		return err
	}
	if err := db.ensureAccountNameAvailable(ctx, account.Provider, name, id); err != nil {
		return err
	}
	_, err = db.sql.ExecContext(ctx, `update accounts set name = ? where id = ?`, name, id)
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

func (db *DB) NextPriorityForProvider(ctx context.Context, provider string) (int, error) {
	provider, err := CanonicalProvider(provider)
	if err != nil {
		return 0, err
	}
	var next sql.NullInt64
	if err := db.sql.QueryRowContext(ctx, `select coalesce(max(priority), 0) + 1 from accounts where provider = ?`, provider).Scan(&next); err != nil {
		return 0, err
	}
	return int(next.Int64), nil
}

func (db *DB) ensureAccountNameAvailable(ctx context.Context, provider, name, excludeID string) error {
	var count int
	err := db.sql.QueryRowContext(ctx, `
		select count(1) from accounts
		where provider = ? and lower(name) = lower(?) and id <> ?
	`, provider, strings.TrimSpace(name), excludeID).Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("connection alias %q already exists for provider %s", name, provider)
	}
	return nil
}

func (db *DB) SetCooldown(ctx context.Context, id string, until time.Time) error {
	_, err := db.sql.ExecContext(ctx, `update accounts set cooldown_until = ? where id = ?`, until.UTC().Format(time.RFC3339Nano), id)
	return err
}

// RecordRetryableFailure persists the account's retry state. A future
// cooldownHint (for example Retry-After) wins; otherwise an exponential
// backoff with jitter is calculated from the new consecutive failure count.
func (db *DB) RecordRetryableFailure(ctx context.Context, id string, now, cooldownHint time.Time) (time.Time, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var failures int
	if err := tx.QueryRowContext(ctx, `select consecutive_failures from accounts where id = ?`, id).Scan(&failures); err != nil {
		return time.Time{}, err
	}
	failures++
	until := cooldownHint.UTC()
	if !until.After(now) {
		until = now.Add(retryBackoff(failures))
	}
	if _, err := tx.ExecContext(ctx, `
		update accounts
		set consecutive_failures = ?, last_failure_at = ?, cooldown_until = ?
		where id = ?
	`, failures, now.Format(time.RFC3339Nano), until.Format(time.RFC3339Nano), id); err != nil {
		return time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, err
	}
	return until, nil
}

func retryBackoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	base := 2 * time.Second
	for i := 1; i < failures && base < 5*time.Minute; i++ {
		base *= 2
		if base > 5*time.Minute {
			base = 5 * time.Minute
		}
	}
	// Add up to 25% positive jitter while preserving the documented two-second
	// minimum and five-minute maximum.
	jitter, err := rand.Int(rand.Reader, big.NewInt(251))
	if err == nil {
		base = time.Duration(float64(base) * (1 + float64(jitter.Int64())/1000))
	}
	if base > 5*time.Minute {
		return 5 * time.Minute
	}
	return base
}

func (db *DB) ResetFailureState(ctx context.Context, id string) error {
	_, err := db.sql.ExecContext(ctx, `
		update accounts
		set consecutive_failures = 0, last_failure_at = null, cooldown_until = null
		where id = ?
	`, id)
	return err
}

// SwapAccountPrioritiesCAS swaps two priorities only when both rows still
// have the values observed by the caller. The single statement prevents two
// concurrent successful fallbacks from swapping the accounts back.
func (db *DB) SwapAccountPrioritiesCAS(ctx context.Context, provider, firstID string, firstPriority int, successID string, successPriority int) (bool, error) {
	provider, err := CanonicalProvider(provider)
	if err != nil {
		return false, err
	}
	if firstID == successID {
		return false, nil
	}
	result, err := db.sql.ExecContext(ctx, `
		update accounts
		set priority = case id when ? then ? when ? then ? end
		where provider = ? and id in (?, ?)
		  and exists (
			select 1
			from accounts first_account, accounts success_account
			where first_account.id = ? and first_account.provider = ? and first_account.priority = ?
			  and success_account.id = ? and success_account.provider = ? and success_account.priority = ?
		  )
	`, firstID, successPriority, successID, firstPriority,
		provider, firstID, successID,
		firstID, provider, firstPriority, successID, provider, successPriority)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 2, err
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
	var lastFailure sql.NullString
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
		&account.ConsecutiveFailures,
		&lastFailure,
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
	if lastFailure.Valid && lastFailure.String != "" {
		tm, err := time.Parse(time.RFC3339Nano, lastFailure.String)
		if err != nil {
			return Account{}, err
		}
		account.LastFailureAt = sql.NullTime{Time: tm, Valid: true}
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
