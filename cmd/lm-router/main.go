package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/andrisasuke/lm-router/internal/anthropic"
	"github.com/andrisasuke/lm-router/internal/app"
	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/proxy"
	"github.com/andrisasuke/lm-router/internal/store"
	"github.com/andrisasuke/lm-router/internal/tui"
	iversion "github.com/andrisasuke/lm-router/internal/version"
	"golang.org/x/term"
)

const defaultCodexBaseURL = "https://chatgpt.com/backend-api/codex/responses"

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "tui":
		runTUI(os.Args[2:])
	case "auth":
		runAuth(os.Args[2:])
	case "keys":
		runKeys(os.Args[2:])
	case "codex":
		runCodex(os.Args[2:])
	case "claude":
		runClaude(os.Args[2:])
	case "version":
		runVersion()
	default:
		usage()
	}
}

func runVersion() {
	fmt.Print(versionText(iversion.Version, iversion.Commit, iversion.BuildDate))
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	host := fs.String("host", "127.0.0.1", "")
	port := fs.Int("port", 19090, "")
	dataDir := fs.String("data-dir", defaultDataDir(), "")
	fs.Parse(args)

	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		exitError(err)
	}
	defer db.Close()

	tokens := app.NewProviderTokenManager(db)
	claudeClient := anthropic.NewClient(anthropic.DefaultMessagesURL, anthropic.DefaultUsageURL, tokens, nil)
	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", *host, *port),
		Handler:           proxy.New(proxy.ServerConfig{Store: db, Codex: codex.NewClient(defaultCodexBaseURL, tokens), Anthropic: claudeClient, RequireKey: true, LogRequests: true}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("lm-router listening on %s", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}

func runTUI(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	host := fs.String("host", "", "")
	port := fs.Int("port", 0, "")
	dataDir := fs.String("data-dir", defaultDataDir(), "")
	fs.Parse(args)
	if err := tui.Run(context.Background(), *dataDir, *host, *port); err != nil {
		exitError(err)
	}
}

func runAuth(args []string) {
	if len(args) < 1 {
		usage()
		return
	}
	switch args[0] {
	case "add":
		runAuthAdd(args[1:])
	case "list":
		runAuthList(args[1:])
	case "remove":
		runAuthRemove(args[1:])
	case "enable":
		runAuthEnableDisable(args[1:], true)
	case "disable":
		runAuthEnableDisable(args[1:], false)
	case "move":
		runAuthMove(args[1:])
	case "refresh":
		runAuthRefresh(args[1:])
	case "test":
		runAuthTest(args[1:])
	default:
		usage()
	}
}

func runAuthAdd(args []string) {
	provider := store.ProviderOpenAICodex
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		canonical, err := store.CanonicalProvider(args[0])
		if err != nil {
			exitError(err)
		}
		provider = canonical
		args = args[1:]
	}
	fs := flag.NewFlagSet("auth add", flag.ExitOnError)
	name := fs.String("name", "", "")
	dataDir := fs.String("data-dir", defaultDataDir(), "")
	defaultRedirect := "http://localhost:1455/auth/callback"
	if provider == store.ProviderAnthropicClaude {
		defaultRedirect = "https://console.anthropic.com/oauth/code/callback"
	}
	redirectURI := fs.String("redirect-uri", defaultRedirect, "")
	acceptRisk := fs.Bool("accept-risk", false, "")
	testAfterAdd := fs.Bool("test", false, "")
	testModel := fs.String("test-model", "gpt-5.3-codex", "")
	testBaseURL := fs.String("test-base-url", defaultCodexBaseURL, "")
	fs.Parse(args)
	if *name == "" {
		*name = provider
	}
	if provider == store.ProviderAnthropicClaude && !*acceptRisk {
		fmt.Println("WARNING: Using Claude subscription OAuth through a router is not an officially licensed Anthropic API flow and may put the account at risk.")
		fmt.Println("Automatic fallback across subscriptions may be treated as combining or bypassing capacity limits and does not prevent provider restrictions.")
		fmt.Print("Type 'I understand' to continue: ")
		confirmation, err := readPromptLine(os.Stdin)
		if err != nil {
			exitError(err)
		}
		if !strings.EqualFold(strings.TrimSpace(confirmation), "I understand") {
			exitErrorf("risk confirmation was not accepted (use --accept-risk for non-interactive use)")
		}
	}

	service := app.ProviderService{}
	session := service.NewAuthSessionForProvider(provider, *redirectURI)
	fmt.Println(session.AuthURL)
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not create OAuth URL directory: %v\n", err)
	} else if err := os.WriteFile(filepath.Join(*dataDir, provider+"-auth-url.txt"), []byte(session.AuthURL+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save OAuth URL: %v\n", err)
	}
	prompt := "Paste callback URL: "
	if provider == store.ProviderAnthropicClaude {
		prompt = "Paste callback URL or code#state: "
	}
	fmt.Print(prompt)
	callbackURL, err := readPromptLine(os.Stdin)
	if err != nil {
		exitError(err)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		exitError(err)
	}
	defer db.Close()
	service.DB = db
	account, err := service.AddFromCallback(ctx, session, *name, callbackURL)
	if err != nil {
		exitError(err)
	}
	fmt.Printf("Success: provider %q saved (%s)\n", account.Name, account.ID)
	if *testAfterAdd {
		result, err := testStoredAccount(ctx, db, account, *testModel, *testBaseURL)
		if err != nil {
			exitError(err)
		}
		printTestResult(result)
	}
}

func runAuthList(args []string) {
	fs := flag.NewFlagSet("auth list", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "")
	provider := fs.String("provider", "", "")
	fs.Parse(args)

	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		exitError(err)
	}
	defer db.Close()
	var accounts []store.Account
	if strings.TrimSpace(*provider) == "" {
		accounts, err = db.ListAccounts(ctx)
	} else {
		accounts, err = db.ListAccountsByProvider(ctx, *provider)
	}
	if err != nil {
		exitError(err)
	}
	printAccounts(accounts)
}

func runAuthRemove(args []string) {
	fs := flag.NewFlagSet("auth remove", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "")
	fs.Parse(args)
	if fs.NArg() != 1 {
		exitErrorf("usage: lm-router auth remove <account-id>")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		exitError(err)
	}
	defer db.Close()
	if err := db.DeleteAccount(ctx, fs.Arg(0)); err != nil {
		exitError(err)
	}
	fmt.Printf("Success: provider removed (%s)\n", fs.Arg(0))
}

func runAuthEnableDisable(args []string, enabled bool) {
	name := "disable"
	if enabled {
		name = "enable"
	}
	fs := flag.NewFlagSet("auth "+name, flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "")
	fs.Parse(args)
	if fs.NArg() != 1 {
		exitErrorf("usage: lm-router auth %s <account-id>", name)
	}
	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		exitError(err)
	}
	defer db.Close()
	if err := db.SetAccountEnabled(ctx, fs.Arg(0), enabled); err != nil {
		exitError(err)
	}
	fmt.Printf("Success: provider %s (%s)\n", name+"d", fs.Arg(0))
}

func runAuthMove(args []string) {
	fs := flag.NewFlagSet("auth move", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "")
	priority := fs.Int("priority", 0, "")
	fs.Parse(args)
	if fs.NArg() != 1 {
		exitErrorf("usage: lm-router auth move <account-id> --priority <n>")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		exitError(err)
	}
	defer db.Close()
	if err := db.SetAccountPriority(ctx, fs.Arg(0), *priority); err != nil {
		exitError(err)
	}
	fmt.Printf("Success: provider moved to priority %d (%s)\n", *priority, fs.Arg(0))
}

func runAuthRefresh(args []string) {
	fs := flag.NewFlagSet("auth refresh", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "")
	name := fs.String("name", "", "")
	provider := fs.String("provider", "", "")
	fs.Parse(args)
	if fs.NArg() != 1 && *name == "" {
		exitErrorf("usage: lm-router auth refresh <account-id> OR --provider <provider> --name <alias>")
	}
	if *name != "" && *provider == "" {
		exitErrorf("--provider is required when using --name")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		exitError(err)
	}
	defer db.Close()
	account, err := resolveAccount(ctx, db, fs, *provider, *name)
	if err != nil {
		exitError(err)
	}
	account, err = (app.ProviderService{DB: db}).Refresh(ctx, account.ID)
	if err != nil {
		exitError(err)
	}
	fmt.Printf("Success: provider %q refreshed (%s)\n", account.Name, account.ID)
}

func runAuthTest(args []string) {
	fs := flag.NewFlagSet("auth test", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "")
	name := fs.String("name", "", "")
	provider := fs.String("provider", "", "")
	model := fs.String("model", "gpt-5.3-codex", "")
	baseURL := fs.String("base-url", defaultCodexBaseURL, "")
	usageURL := fs.String("usage-url", anthropic.DefaultUsageURL, "")
	fs.Parse(args)
	if fs.NArg() != 1 && *name == "" {
		exitErrorf("usage: lm-router auth test <account-id> OR --provider <provider> --name <alias>")
	}
	if *name != "" && *provider == "" {
		exitErrorf("--provider is required when using --name")
	}
	ctx := context.Background()
	db, err := store.Open(ctx, *dataDir)
	if err != nil {
		exitError(err)
	}
	defer db.Close()
	account, err := resolveAccount(ctx, db, fs, *provider, *name)
	if err != nil {
		exitError(err)
	}
	result, err := testStoredAccountWithUsage(ctx, db, account, *model, *baseURL, *usageURL)
	if err != nil {
		exitError(err)
	}
	printTestResult(result)
}

func resolveAccount(ctx context.Context, db *store.DB, fs *flag.FlagSet, provider, name string) (store.Account, error) {
	if name != "" {
		return db.GetAccountByProviderAndName(ctx, provider, name)
	}
	return db.GetAccount(ctx, fs.Arg(0))
}

func resolveAccountForTest(ctx context.Context, db *store.DB, fs *flag.FlagSet, name string) (store.Account, error) {
	if name != "" {
		provider := ""
		if found := fs.Lookup("provider"); found != nil {
			provider = found.Value.String()
		}
		if provider != "" {
			return db.GetAccountByProviderAndName(ctx, provider, name)
		}
		return db.GetAccountByName(ctx, name)
	}
	return db.GetAccount(ctx, fs.Arg(0))
}

type providerTestResult struct {
	AccountID string
	Name      string
	Status    int
	OK        bool
	Output    string
}

func testStoredAccount(ctx context.Context, db *store.DB, account store.Account, model, baseURL string) (providerTestResult, error) {
	return testStoredAccountWithUsage(ctx, db, account, model, baseURL, anthropic.DefaultUsageURL)
}

func testStoredAccountWithUsage(ctx context.Context, db *store.DB, account store.Account, model, baseURL, usageURL string) (providerTestResult, error) {
	result, err := (app.ProviderService{DB: db, BaseURL: baseURL, AnthropicUsageURL: usageURL}).Test(ctx, account, model)
	if err != nil {
		return providerTestResult{}, err
	}
	return providerTestResult{
		AccountID: result.AccountID,
		Name:      result.Name,
		Status:    result.Status,
		OK:        result.OK,
		Output:    result.Output,
	}, nil
}

func printTestResult(result providerTestResult) {
	if result.OK {
		fmt.Printf("Success: provider %q connected", result.Name)
		if result.Output != "" {
			fmt.Printf(" (%s)", result.Output)
		}
		fmt.Println()
		return
	}
	reason := strings.TrimSpace(result.Output)
	if reason == "" {
		reason = fmt.Sprintf("HTTP %d", result.Status)
	}
	fmt.Printf("Error: provider %q failed: %s\n", result.Name, humanError(reason))
}

func printAccounts(accounts []store.Account) {
	if len(accounts) == 0 {
		fmt.Println("No providers saved.")
		return
	}
	fmt.Println("Providers:")
	for _, account := range accounts {
		status := "active"
		if !account.Enabled {
			status = "disabled"
		}
		if account.NeedsReauth {
			status = "needs re-auth"
		}
		if account.CooldownUntil.Valid && account.CooldownUntil.Time.After(time.Now()) {
			status = "cooldown until " + account.CooldownUntil.Time.Local().Format(time.RFC3339)
		}
		fmt.Printf("- %s (%s): provider=%s priority=%d status=%s expires=%s\n",
			account.Name,
			account.ID,
			account.Provider,
			account.Priority,
			status,
			account.ExpiresAt.Local().Format(time.RFC3339),
		)
	}
}

func printCreatedKey(key store.APIKey) {
	fmt.Println("Success: API key created")
	fmt.Printf("Name: %s\n", key.Name)
	fmt.Printf("ID: %s\n", key.ID)
	fmt.Printf("Secret: %s\n", key.Secret)
}

func printKeys(keys []store.APIKey) {
	if len(keys) == 0 {
		fmt.Println("No API keys saved.")
		return
	}
	fmt.Println("API keys:")
	for _, key := range keys {
		fmt.Printf("- %s (%s): prefix=%s created=%s\n",
			key.Name,
			key.ID,
			key.Prefix,
			key.CreatedAt.Local().Format(time.RFC3339),
		)
	}
}

func exitError(err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "Error: %s\n", humanError(err.Error()))
	os.Exit(1)
}

func exitErrorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "Error: "+format+"\n", args...)
	os.Exit(1)
}

func humanError(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "unknown error"
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err == nil {
		if msg := stringField(payload, "message"); msg != "" {
			return msg
		}
		if detail := stringField(payload, "detail"); detail != "" {
			return detail
		}
		if errObj, ok := payload["error"].(map[string]any); ok {
			if msg := stringField(errObj, "message"); msg != "" {
				return msg
			}
			if code := stringField(errObj, "code"); code != "" {
				return code
			}
			if typ := stringField(errObj, "type"); typ != "" {
				return typ
			}
		}
	}
	return raw
}

func stringField(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func runKeys(args []string) {
	if len(args) < 1 {
		usage()
		return
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("keys create", flag.ExitOnError)
		name := fs.String("name", "default", "")
		dataDir := fs.String("data-dir", defaultDataDir(), "")
		fs.Parse(args[1:])

		ctx := context.Background()
		db, err := store.Open(ctx, *dataDir)
		if err != nil {
			exitError(err)
		}
		defer db.Close()
		key, err := db.CreateAPIKey(ctx, *name)
		if err != nil {
			exitError(err)
		}
		printCreatedKey(key)
	case "list":
		fs := flag.NewFlagSet("keys list", flag.ExitOnError)
		dataDir := fs.String("data-dir", defaultDataDir(), "")
		fs.Parse(args[1:])

		ctx := context.Background()
		db, err := store.Open(ctx, *dataDir)
		if err != nil {
			exitError(err)
		}
		defer db.Close()
		keys, err := db.ListAPIKeys(ctx)
		if err != nil {
			exitError(err)
		}
		printKeys(keys)
	case "revoke":
		fs := flag.NewFlagSet("keys revoke", flag.ExitOnError)
		dataDir := fs.String("data-dir", defaultDataDir(), "")
		fs.Parse(args[1:])
		if fs.NArg() != 1 {
			exitErrorf("usage: lm-router keys revoke <key-id>")
		}
		ctx := context.Background()
		db, err := store.Open(ctx, *dataDir)
		if err != nil {
			exitError(err)
		}
		defer db.Close()
		if err := db.DeleteAPIKey(ctx, fs.Arg(0)); err != nil {
			exitError(err)
		}
		fmt.Printf("Success: API key revoked (%s)\n", fs.Arg(0))
	default:
		usage()
	}
}

func runCodex(args []string) {
	if len(args) >= 1 && args[0] == "print-config" {
		fs := flag.NewFlagSet("codex print-config", flag.ExitOnError)
		model := fs.String("model", "gpt-5.3-codex", "")
		subagentModel := fs.String("subagent-model", "", "")
		port := fs.Int("port", 19090, "")
		apiKey := fs.String("api-key", "sk-lm-router-REPLACE_ME", "")
		fs.Parse(args[1:])
		effectiveSubagent := *subagentModel
		if effectiveSubagent == "" {
			effectiveSubagent = *model
		}
		fmt.Print(app.CodexConfigText(*port, *apiKey, *model))
		if effectiveSubagent != *model {
			fmt.Printf("\n# subagent override:\n[agents.subagent]\nmodel = %q\n", effectiveSubagent)
		}
		return
	}
	usage()
}

func runClaude(args []string) {
	if len(args) >= 1 && args[0] == "print-config" {
		fs := flag.NewFlagSet("claude print-config", flag.ExitOnError)
		model := fs.String("model", "", "")
		port := fs.Int("port", 19090, "")
		apiKey := fs.String("api-key", "sk-lm-router-REPLACE_ME", "")
		fs.Parse(args[1:])
		fmt.Print(app.ClaudeConfigText(*port, *apiKey, *model))
		return
	}
	usage()
}

func usage() {
	fmt.Println("usage: lm-router <serve|tui|auth|keys|codex|claude|version>")
}

func versionText(version, commit, buildDate string) string {
	return fmt.Sprintf("Version: %s\nCommit: %s\nBuildDate: %s\n", version, commit, buildDate)
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".lm-router"
	}
	return filepath.Join(home, ".lm-router")
}

func mustRandomString(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buf)[:n]
}

func mustRandomBase64URLString(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return stringsTrimRight(base64.RawURLEncoding.EncodeToString(sum[:]), "=")
}

func stringsTrimRight(v, cutset string) string {
	return strings.TrimRight(v, cutset)
}

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func mustJSONBytes(v any) []byte {
	return []byte(mustJSON(v))
}

var terminalEscapePattern = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

func readPromptLine(r *os.File) (string, error) {
	if term.IsTerminal(int(r.Fd())) {
		return readRawPromptLine(r)
	}
	reader := bufio.NewReader(r)
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	return sanitizeTerminalInput(line), nil
}

func readRawPromptLine(r *os.File) (string, error) {
	fd := int(r.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = term.Restore(fd, oldState)
	}()

	reader := bufio.NewReader(r)
	var editor lineEditor
	for {
		ch, _, err := reader.ReadRune()
		if err != nil {
			return "", err
		}
		switch ch {
		case '\r', '\n':
			fmt.Fprintln(os.Stdout)
			return sanitizeTerminalInput(editor.String()), nil
		case 0x03:
			return "", fmt.Errorf("interrupted")
		case 0x08, 0x7f:
			if editor.backspace() {
				renderBackspace(editor)
			}
		case 0x1b:
			action, err := consumeEscapeSequence(reader)
			if err != nil {
				return "", err
			}
			renderEscapeAction(&editor, action)
		default:
			if ch >= 0x20 {
				editor.insert(ch)
				renderInsert(editor)
			}
		}
	}
}

type escapeAction int

const (
	escapeIgnored escapeAction = iota
	escapeLeft
	escapeRight
	escapeDelete
	escapeHome
	escapeEnd
)

type lineEditor struct {
	input  []rune
	cursor int
}

func (e *lineEditor) String() string {
	return string(e.input)
}

func (e *lineEditor) insert(ch rune) {
	if e.cursor == len(e.input) {
		e.input = append(e.input, ch)
		e.cursor++
		return
	}
	e.input = append(e.input, 0)
	copy(e.input[e.cursor+1:], e.input[e.cursor:])
	e.input[e.cursor] = ch
	e.cursor++
}

func (e *lineEditor) backspace() bool {
	if e.cursor == 0 {
		return false
	}
	e.cursor--
	copy(e.input[e.cursor:], e.input[e.cursor+1:])
	e.input = e.input[:len(e.input)-1]
	return true
}

func (e *lineEditor) delete() bool {
	if e.cursor >= len(e.input) {
		return false
	}
	copy(e.input[e.cursor:], e.input[e.cursor+1:])
	e.input = e.input[:len(e.input)-1]
	return true
}

func (e *lineEditor) moveLeft() bool {
	if e.cursor == 0 {
		return false
	}
	e.cursor--
	return true
}

func (e *lineEditor) moveRight() bool {
	if e.cursor >= len(e.input) {
		return false
	}
	e.cursor++
	return true
}

func (e *lineEditor) moveHome() int {
	old := e.cursor
	e.cursor = 0
	return old
}

func (e *lineEditor) moveEnd() int {
	old := e.cursor
	e.cursor = len(e.input)
	return e.cursor - old
}

func renderInsert(editor lineEditor) {
	tail := string(editor.input[editor.cursor-1:])
	fmt.Fprint(os.Stdout, tail)
	moveCursorLeft(len([]rune(tail)) - 1)
}

func renderBackspace(editor lineEditor) {
	tail := string(editor.input[editor.cursor:])
	fmt.Fprint(os.Stdout, "\b"+tail+" ")
	moveCursorLeft(len([]rune(tail)) + 1)
}

func renderDelete(editor lineEditor) {
	tail := string(editor.input[editor.cursor:])
	fmt.Fprint(os.Stdout, tail+" ")
	moveCursorLeft(len([]rune(tail)) + 1)
}

func renderEscapeAction(editor *lineEditor, action escapeAction) {
	switch action {
	case escapeLeft:
		if editor.moveLeft() {
			moveCursorLeft(1)
		}
	case escapeRight:
		if editor.moveRight() {
			moveCursorRight(1)
		}
	case escapeDelete:
		if editor.delete() {
			renderDelete(*editor)
		}
	case escapeHome:
		moveCursorLeft(editor.moveHome())
	case escapeEnd:
		moveCursorRight(editor.moveEnd())
	}
}

func moveCursorLeft(n int) {
	if n > 0 {
		fmt.Fprintf(os.Stdout, "\x1b[%dD", n)
	}
}

func moveCursorRight(n int) {
	if n > 0 {
		fmt.Fprintf(os.Stdout, "\x1b[%dC", n)
	}
}

func consumeEscapeSequence(reader *bufio.Reader) (escapeAction, error) {
	ch, _, err := reader.ReadRune()
	if err != nil {
		return escapeIgnored, err
	}
	if ch != '[' {
		return escapeIgnored, nil
	}
	var seq []rune
	for {
		ch, _, err = reader.ReadRune()
		if err != nil {
			return escapeIgnored, err
		}
		seq = append(seq, ch)
		if ch >= '@' && ch <= '~' {
			switch string(seq) {
			case "D":
				return escapeLeft, nil
			case "C":
				return escapeRight, nil
			case "3~":
				return escapeDelete, nil
			case "H", "1~", "OH":
				return escapeHome, nil
			case "F", "4~", "OF":
				return escapeEnd, nil
			default:
				return escapeIgnored, nil
			}
		}
	}
}

func sanitizeTerminalInput(v string) string {
	v = terminalEscapePattern.ReplaceAllString(v, "")
	v = strings.ReplaceAll(v, "\r", "")
	v = strings.ReplaceAll(v, "\n", "")
	return strings.TrimSpace(v)
}

func nextPriority(db *store.DB, ctx context.Context) int {
	priority, err := db.NextPriority(ctx)
	if err != nil {
		return int(time.Now().Unix())
	}
	return priority
}
