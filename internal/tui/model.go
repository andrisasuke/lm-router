package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrisasuke/lm-router/internal/app"
	"github.com/andrisasuke/lm-router/internal/codex"
	"github.com/andrisasuke/lm-router/internal/store"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type screen int

const (
	screenHome screen = iota
	screenProviders
	screenAddProvider
	screenReauthProvider
	screenProviderDetail
	screenKeys
	screenServer
	screenSettings
	screenLogs
	screenCodexConfig
)

type addProviderField int

const (
	addProviderNameField addProviderField = iota
	addProviderCallbackField
)

const statusTimeout = 3 * time.Second

type Model struct {
	ctx        context.Context
	db         *store.DB
	logger     *app.RingLogger
	server     *app.ServerController
	settings   store.Settings
	screen     screen
	stack      []screen
	selected   int
	statusLine string
	statusSeq  int

	accounts          []store.Account
	keys              []store.APIKey
	selectedAccount   int
	authSession       app.AuthSession
	dataDir           string
	authURLPath       string
	authURLWriteErr   error
	providerNameInput textinput.Model
	callbackInput     textinput.Model
	addProviderField  addProviderField
	aliasInput        textinput.Model
	aliasEditing      bool
	settingInput      textinput.Model
	settingEditing    bool
	settingSelected   int
	logFilter         string

	providerQuota        *codex.QuotaInfo
	providerQuotaErr     error
	providerQuotaLoading bool
}

func New(ctx context.Context, db *store.DB, logger *app.RingLogger, server *app.ServerController, settings store.Settings) Model {
	return NewWithDataDir(ctx, db, logger, server, settings, "")
}

func NewWithDataDir(ctx context.Context, db *store.DB, logger *app.RingLogger, server *app.ServerController, settings store.Settings, dataDir string) Model {
	cb := textinput.New()
	cb.Placeholder = "http://localhost:1455/auth/callback?code=..."
	cb.CharLimit = 4096
	cb.Width = 90
	nameInput := textinput.New()
	nameInput.Placeholder = "main"
	nameInput.CharLimit = 80
	nameInput.Width = 32
	aliasInput := textinput.New()
	aliasInput.CharLimit = 80
	aliasInput.Width = 32
	settingInput := textinput.New()
	settingInput.Width = 40
	return Model{
		ctx:               ctx,
		db:                db,
		logger:            logger,
		server:            server,
		settings:          settings,
		screen:            screenHome,
		dataDir:           dataDir,
		providerNameInput: nameInput,
		callbackInput:     cb,
		aliasInput:        aliasInput,
		settingInput:      settingInput,
		logFilter:         "all",
	}
}

func NewTestModel() Model {
	return New(context.Background(), nil, app.NewRingLogger(10, nil), app.NewServerController(app.ServerControllerConfig{}), store.DefaultSettings())
}

func (m Model) Init() tea.Cmd {
	if m.db == nil {
		return nil
	}
	return tea.Batch(m.loadProvidersCmd(), m.loadKeysCmd())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.screen == screenAddProvider {
			return m.updateAddProvider(msg)
		}
		if m.screen == screenReauthProvider {
			return m.updateReauthProvider(msg)
		}
		if m.screen == screenProviderDetail && m.aliasEditing {
			return m.updateAliasInput(msg)
		}
		if m.screen == screenSettings && m.settingEditing {
			return m.updateSettingInput(msg)
		}
		switch msg.Type {
		case tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEsc, tea.KeyBackspace:
			return m.back(), nil
		case tea.KeyShiftUp:
			if m.screen == screenProviders {
				return m.reorderSelectedProvider(-1)
			}
			m.move(-1)
		case tea.KeyShiftDown:
			if m.screen == screenProviders {
				return m.reorderSelectedProvider(1)
			}
			m.move(1)
		case tea.KeyUp:
			m.move(-1)
		case tea.KeyDown:
			m.move(1)
		case tea.KeyEnter:
			return m.activate()
		default:
			if msg.String() == "q" && m.screen == screenHome {
				return m, tea.Quit
			}
		}
	case addProviderDoneMsg:
		if msg.err != nil {
			m.statusLine = "Error: " + app.HumanError(msg.err.Error())
			return m, nil
		}
		m.accounts = append(m.accounts, msg.account)
		m.statusLine = fmt.Sprintf("Success: provider %q saved", msg.account.Name)
		m.screen = screenProviders
		m.selected = 0
	case reauthDoneMsg:
		if msg.err != nil {
			m.statusLine = "Error: " + app.HumanError(msg.err.Error())
			return m, nil
		}
		if m.selectedAccount >= 0 && m.selectedAccount < len(m.accounts) {
			m.accounts[m.selectedAccount] = msg.account
		}
		m.statusLine = fmt.Sprintf("Success: provider %q re-authenticated", msg.account.Name)
		return m.back(), nil
	case providerTestDoneMsg:
		if msg.err != nil {
			m.statusLine = "Error: " + app.HumanError(msg.err.Error())
			return m, nil
		}
		if msg.result.OK {
			m.statusLine = fmt.Sprintf("Success: provider %q connected", msg.result.Name)
		} else {
			m.statusLine = fmt.Sprintf("Error: provider %q failed: %s", msg.result.Name, app.HumanError(msg.result.Output))
		}
	case providerQuotaDoneMsg:
		m.providerQuotaLoading = false
		if msg.err != nil {
			m.providerQuotaErr = msg.err
		} else {
			q := msg.info
			m.providerQuota = &q
			m.providerQuotaErr = nil
		}
	case loadProvidersMsg:
		if msg.err != nil {
			m.statusLine = "Error: " + msg.err.Error()
		} else {
			m.accounts = msg.accounts
		}
	case loadKeysMsg:
		if msg.err != nil {
			m.statusLine = "Error: " + msg.err.Error()
		} else {
			m.keys = msg.keys
		}
	case createdKeyMsg:
		if msg.err != nil {
			m.statusLine = "Error: " + msg.err.Error()
		} else {
			m.keys = append(m.keys, msg.key)
			m.statusLine = "Success: API key created. Secret: " + msg.key.Secret
		}
	case errMsg:
		m.statusLine = "Error: " + msg.err.Error()
	case clearStatusMsg:
		if msg.seq == m.statusSeq {
			m.statusLine = ""
		}
	}
	return m, nil
}

func (m Model) View() string {
	var b strings.Builder
	b.WriteString(title(m.title()))
	b.WriteString("\n")
	b.WriteString(breadcrumb(m.breadcrumb()))
	b.WriteString("\n\n")
	switch m.screen {
	case screenHome:
		b.WriteString(m.viewHome())
	case screenProviders:
		b.WriteString(m.viewProviders())
	case screenAddProvider:
		b.WriteString(m.viewAddProvider())
	case screenReauthProvider:
		b.WriteString(m.viewReauthProvider())
	case screenProviderDetail:
		b.WriteString(m.viewProviderDetail())
	case screenKeys:
		b.WriteString(m.viewKeys())
	case screenServer:
		b.WriteString(m.viewServer())
	case screenSettings:
		b.WriteString(m.viewSettings())
	case screenLogs:
		b.WriteString(m.viewLogs())
	case screenCodexConfig:
		b.WriteString(m.viewCodexConfig())
	}
	if m.statusLine != "" {
		b.WriteString("\n\n")
		b.WriteString(statusStyle.Render(m.statusLine))
	}
	b.WriteString("\n")
	return b.String()
}

func (m Model) updateAddProvider(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		return m.back(), nil
	case tea.KeyTab, tea.KeyDown:
		m.focusAddProviderField(addProviderCallbackField)
		return m, nil
	case tea.KeyShiftTab, tea.KeyUp:
		m.focusAddProviderField(addProviderNameField)
		return m, nil
	case tea.KeyEnter:
		if m.addProviderField == addProviderNameField {
			if err := m.validateProviderName(); err != nil {
				m.statusLine = "Error: " + err.Error()
				return m, nil
			}
			m.focusAddProviderField(addProviderCallbackField)
			return m, nil
		}
		providerName := strings.TrimSpace(m.providerNameInput.Value())
		if err := m.validateProviderName(); err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
		callbackURL := strings.TrimSpace(m.callbackInput.Value())
		if callbackURL == "" {
			m.statusLine = "Error: paste callback URL first"
			return m, nil
		}
		session := m.authSession
		return m, func() tea.Msg {
			account, err := (app.ProviderService{DB: m.db, Logger: m.logger}).AddFromCallback(m.ctx, session, providerName, callbackURL)
			return addProviderDoneMsg{account: account, err: err}
		}
	}
	var cmd tea.Cmd
	if m.addProviderField == addProviderNameField {
		m.providerNameInput, cmd = m.providerNameInput.Update(msg)
	} else {
		m.callbackInput, cmd = m.callbackInput.Update(msg)
	}
	return m, cmd
}

func (m *Model) focusAddProviderField(field addProviderField) {
	m.addProviderField = field
	if field == addProviderNameField {
		m.providerNameInput.Focus()
		m.callbackInput.Blur()
		return
	}
	m.providerNameInput.Blur()
	m.callbackInput.Focus()
}

func (m Model) validateProviderName() error {
	name := strings.TrimSpace(m.providerNameInput.Value())
	return m.validateAliasName(name, "")
}

func (m Model) validateAliasName(name, currentID string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("connection alias is required")
	}
	for _, account := range m.accounts {
		if currentID != "" && account.ID == currentID {
			continue
		}
		if strings.EqualFold(account.Name, name) {
			return fmt.Errorf("connection alias %q already exists", name)
		}
	}
	return nil
}

func (m Model) updateAliasInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.aliasEditing = false
		return m, nil
	case tea.KeyEnter:
		if m.selectedAccount < 0 || m.selectedAccount >= len(m.accounts) {
			m.aliasEditing = false
			return m, nil
		}
		account := m.accounts[m.selectedAccount]
		name := strings.TrimSpace(m.aliasInput.Value())
		if err := m.validateAliasName(name, account.ID); err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
		if err := (app.ProviderService{DB: m.db}).Rename(m.ctx, account.ID, name); err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
		account.Name = name
		m.accounts[m.selectedAccount] = account
		m.aliasEditing = false
		m.statusLine = "Success: alias updated"
		return m, nil
	}
	var cmd tea.Cmd
	m.aliasInput, cmd = m.aliasInput.Update(msg)
	return m, cmd
}

func (m Model) updateSettingInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.settingEditing = false
		return m, nil
	case tea.KeyEnter:
		m.applySettingInput()
		m.settingEditing = false
		return m, nil
	}
	var cmd tea.Cmd
	m.settingInput, cmd = m.settingInput.Update(msg)
	return m, cmd
}

func (m Model) activate() (tea.Model, tea.Cmd) {
	switch m.screen {
	case screenHome:
		return m.activateHome()
	case screenProviders:
		return m.activateProviders()
	case screenProviderDetail:
		return m.activateProviderDetail()
	case screenKeys:
		return m.activateKeys()
	case screenServer:
		return m.activateServer()
	case screenSettings:
		return m.activateSettings()
	case screenLogs:
		if m.selected == 0 {
			return m.back(), nil
		}
		if m.selected == 1 {
			m.logFilter = nextLogFilter(m.logFilter)
			return m, nil
		}
		m.logger.Clear()
	case screenCodexConfig:
		return m.back(), nil
	}
	return m, nil
}

func (m Model) activateHome() (tea.Model, tea.Cmd) {
	switch m.selected {
	case 0:
		m.push(screenProviders)
		return m, m.loadProvidersCmd()
	case 1:
		m.push(screenKeys)
		return m, m.loadKeysCmd()
	case 2:
		m.push(screenServer)
	case 3:
		m.push(screenSettings)
	case 4:
		m.push(screenLogs)
	case 5:
		m.push(screenCodexConfig)
	case 6:
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) activateProviders() (tea.Model, tea.Cmd) {
	if m.selected == 0 {
		return m.back(), nil
	}
	if m.selected == 1 {
		service := app.ProviderService{DB: m.db}
		m.authSession = service.NewAuthSession("http://localhost:1455/auth/callback")
		m.authURLPath, m.authURLWriteErr = m.writeAuthURL()
		m.providerNameInput.SetValue("")
		m.callbackInput.SetValue("")
		m.focusAddProviderField(addProviderNameField)
		m.push(screenAddProvider)
		return m, textinput.Blink
	}
	idx := m.selected - 2
	if idx >= 0 && idx < len(m.accounts) {
		m.selectedAccount = idx
		m.providerQuota = nil
		m.providerQuotaErr = nil
		m.providerQuotaLoading = false
		m.push(screenProviderDetail)
	}
	return m, nil
}

func (m Model) activateProviderDetail() (tea.Model, tea.Cmd) {
	if m.selectedAccount < 0 || m.selectedAccount >= len(m.accounts) {
		return m.back(), nil
	}
	account := m.accounts[m.selectedAccount]
	switch m.selected {
	case 0:
		return m.back(), nil
	case 1:
		m.aliasInput.SetValue(account.Name)
		m.aliasInput.Focus()
		m.aliasEditing = true
		return m, textinput.Blink
	case 2:
		return m, func() tea.Msg {
			result, err := (app.ProviderService{DB: m.db, Logger: m.logger}).Test(m.ctx, account, m.settings.DefaultModel)
			return providerTestDoneMsg{result: result, err: err}
		}
	case 3:
		if m.providerQuotaLoading {
			return m, nil
		}
		m.providerQuota = nil
		m.providerQuotaErr = nil
		m.providerQuotaLoading = true
		return m, func() tea.Msg {
			info, err := (app.ProviderService{DB: m.db, Logger: m.logger}).Quota(m.ctx, account)
			return providerQuotaDoneMsg{info: info, err: err}
		}
	case 4:
		refreshed, err := (app.ProviderService{DB: m.db}).Refresh(m.ctx, account.ID)
		if err != nil {
			m.statusLine = "Error: " + app.HumanError(err.Error())
			return m, nil
		}
		m.accounts[m.selectedAccount] = refreshed
		m.statusLine = "Success: provider refreshed"
	case 5:
		service := app.ProviderService{DB: m.db}
		m.authSession = service.NewAuthSession("http://localhost:1455/auth/callback")
		m.authURLPath, m.authURLWriteErr = m.writeAuthURL()
		m.callbackInput.SetValue("")
		m.callbackInput.Focus()
		m.push(screenReauthProvider)
		return m, textinput.Blink
	case 6:
		err := (app.ProviderService{DB: m.db}).SetEnabled(m.ctx, account.ID, !account.Enabled)
		if err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
		account.Enabled = !account.Enabled
		m.accounts[m.selectedAccount] = account
		m.statusLine = "Success: provider updated"
	case 7:
		if err := (app.ProviderService{DB: m.db}).Delete(m.ctx, account.ID); err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
		m.accounts = append(m.accounts[:m.selectedAccount], m.accounts[m.selectedAccount+1:]...)
		m.statusLine = "Success: provider deleted"
		return m.back(), nil
	}
	return m, nil
}

func (m Model) activateKeys() (tea.Model, tea.Cmd) {
	if m.selected == 0 {
		return m.back(), nil
	}
	if m.selected == 1 {
		return m, func() tea.Msg {
			key, err := (app.KeyService{DB: m.db}).Create(m.ctx, "default")
			return createdKeyMsg{key: key, err: err}
		}
	}
	idx := m.selected - 2
	if idx >= 0 && idx < len(m.keys) {
		key := m.keys[idx]
		if err := (app.KeyService{DB: m.db}).Delete(m.ctx, key.ID); err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
		m.keys = append(m.keys[:idx], m.keys[idx+1:]...)
		m.statusLine = "Success: API key deleted"
	}
	return m, nil
}

func (m Model) activateServer() (tea.Model, tea.Cmd) {
	status := m.server.Status()
	switch m.selected {
	case 0:
		return m.back(), nil
	case 1:
		if status.State == app.ServerOn {
			if err := m.server.Stop(m.ctx); err != nil {
				m.statusLine = "Error: " + err.Error()
			} else {
				m.statusLine = "Success: server stopped"
			}
			return m, nil
		}
		if err := m.server.Start(m.ctx, m.settings.Host, m.settings.Port); err != nil {
			m.statusLine = "Error: " + err.Error()
		} else {
			m.statusLine = "Success: server started"
		}
	case 2:
		if err := m.server.Restart(m.ctx, m.settings.Host, m.settings.Port); err != nil {
			m.statusLine = "Error: " + err.Error()
		} else {
			m.statusLine = "Success: server restarted"
		}
	}
	return m, nil
}

func (m Model) activateSettings() (tea.Model, tea.Cmd) {
	if m.selected == 0 {
		return m.back(), nil
	}
	switch m.selected {
	case 1, 2, 5, 6, 7:
		m.settingSelected = m.selected
		m.settingInput.SetValue(m.settingValue(m.selected))
		m.settingInput.Focus()
		m.settingEditing = true
		return m, textinput.Blink
	case 3:
		m.settings.LogRequests = !m.settings.LogRequests
	case 4:
		m.settings.LogUpstream = !m.settings.LogUpstream
	case 8:
		if err := m.db.SaveSettings(m.ctx, m.settings); err != nil {
			m.statusLine = "Error: " + err.Error()
		} else {
			m.statusLine = "Success: settings saved"
		}
	}
	return m, nil
}

func (m *Model) push(next screen) {
	m.stack = append(m.stack, m.screen)
	m.screen = next
	m.selected = 0
	m.statusLine = ""
}

func (m Model) back() Model {
	if len(m.stack) == 0 {
		return m
	}
	m.screen = m.stack[len(m.stack)-1]
	m.stack = m.stack[:len(m.stack)-1]
	m.selected = 0
	m.statusLine = ""
	return m
}

func (m *Model) move(delta int) {
	count := m.itemCount()
	if count == 0 {
		m.selected = 0
		return
	}
	m.selected = (m.selected + delta + count) % count
}

func (m Model) reorderSelectedProvider(delta int) (Model, tea.Cmd) {
	idx := m.selected - 2
	if idx < 0 || idx >= len(m.accounts) {
		return m, nil
	}
	target := idx + delta
	if target < 0 || target >= len(m.accounts) {
		return m, nil
	}
	current := m.accounts[idx]
	other := m.accounts[target]
	currentPriority := current.Priority
	otherPriority := other.Priority
	current.Priority = otherPriority
	other.Priority = currentPriority
	if m.db != nil {
		if err := m.db.SetAccountPriority(m.ctx, current.ID, current.Priority); err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
		if err := m.db.SetAccountPriority(m.ctx, other.ID, other.Priority); err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
	}
	m.accounts[idx] = other
	m.accounts[target] = current
	m.selected = target + 2
	return m.setTimedStatus("Success: provider priority updated")
}

func (m Model) setTimedStatus(message string) (Model, tea.Cmd) {
	m.statusLine = message
	m.statusSeq++
	seq := m.statusSeq
	return m, tea.Tick(statusTimeout, func(time.Time) tea.Msg {
		return clearStatusMsg{seq: seq}
	})
}

func (m Model) itemCount() int {
	switch m.screen {
	case screenHome:
		return 7
	case screenProviders:
		return 2 + len(m.accounts)
	case screenProviderDetail:
		return 8
	case screenReauthProvider:
		return 0
	case screenKeys:
		return 2 + len(m.keys)
	case screenServer:
		return 3
	case screenSettings:
		return 9
	case screenLogs:
		return 3
	case screenCodexConfig:
		return 1
	default:
		return 0
	}
}

func (m Model) loadProvidersCmd() tea.Cmd {
	return func() tea.Msg {
		if m.db == nil {
			return loadProvidersMsg{}
		}
		accounts, err := (app.ProviderService{DB: m.db}).List(m.ctx)
		return loadProvidersMsg{accounts: accounts, err: err}
	}
}

func (m Model) loadKeysCmd() tea.Cmd {
	return func() tea.Msg {
		if m.db == nil {
			return loadKeysMsg{}
		}
		keys, err := (app.KeyService{DB: m.db}).List(m.ctx)
		return loadKeysMsg{keys: keys, err: err}
	}
}

func (m *Model) applySettingInput() {
	value := strings.TrimSpace(m.settingInput.Value())
	switch m.settingSelected {
	case 1:
		if value != "" {
			m.settings.Host = value
		}
	case 2:
		var port int
		if _, err := fmt.Sscanf(value, "%d", &port); err == nil && port > 0 {
			m.settings.Port = port
		}
	case 5:
		var limit int
		if _, err := fmt.Sscanf(value, "%d", &limit); err == nil && limit > 0 {
			m.settings.LogBodyLimit = limit
		}
	case 6:
		if value != "" {
			m.settings.DefaultModel = value
		}
	case 7:
		var t int
		if _, err := fmt.Sscanf(value, "%d", &t); err == nil && t > 0 {
			m.settings.UpstreamTimeoutSeconds = t
		}
	}
}

func (m Model) settingValue(selected int) string {
	switch selected {
	case 1:
		return m.settings.Host
	case 2:
		return fmt.Sprintf("%d", m.settings.Port)
	case 5:
		return fmt.Sprintf("%d", m.settings.LogBodyLimit)
	case 6:
		return m.settings.DefaultModel
	case 7:
		return fmt.Sprintf("%d", m.settings.UpstreamTimeoutSeconds)
	default:
		return ""
	}
}

func (m Model) currentAPIKeyPrefix() string {
	if len(m.keys) == 0 && m.db != nil {
		keys, err := (app.KeyService{DB: m.db}).List(m.ctx)
		if err == nil {
			m.keys = keys
		}
	}
	if len(m.keys) == 0 {
		return "(none)"
	}
	return m.keys[0].Prefix
}

func (m Model) activeProviderCount() int {
	count := 0
	for _, account := range m.accounts {
		if account.Enabled && !account.NeedsReauth {
			count++
		}
	}
	return count
}

func (m Model) title() string {
	switch m.screen {
	case screenProviders:
		return "OpenAI Codex (OAUTH)"
	case screenAddProvider:
		return "Add OpenAI Codex"
	case screenReauthProvider:
		if m.selectedAccount >= 0 && m.selectedAccount < len(m.accounts) {
			return "Re-authenticate " + providerDisplayName(m.accounts[m.selectedAccount])
		}
		return "Re-authenticate"
	case screenProviderDetail:
		if m.selectedAccount >= 0 && m.selectedAccount < len(m.accounts) {
			return providerDisplayName(m.accounts[m.selectedAccount])
		}
		return "Provider"
	case screenKeys:
		return "API Keys"
	case screenServer:
		return "Server"
	case screenSettings:
		return "Settings"
	case screenLogs:
		return "Logs"
	case screenCodexConfig:
		return "Codex Config"
	default:
		return "LM Router Terminal UI"
	}
}

func (m Model) breadcrumb() string {
	switch m.screen {
	case screenHome:
		return "LM Router"
	case screenProviders:
		return "LM Router > Providers"
	case screenAddProvider:
		return "LM Router > Providers > Add"
	case screenReauthProvider:
		return "LM Router > Providers > Connection > Re-authenticate"
	case screenProviderDetail:
		return "LM Router > Providers > Connection"
	case screenKeys:
		return "LM Router > API Keys"
	case screenServer:
		return "LM Router > Server"
	case screenSettings:
		return "LM Router > Settings"
	case screenLogs:
		return "LM Router > Logs"
	case screenCodexConfig:
		return "LM Router > Codex Config"
	default:
		return "LM Router"
	}
}

func (m Model) viewHome() string {
	status := m.server.Status()
	endpoint := fmt.Sprintf("http://%s:%d/v1", m.settings.Host, m.settings.Port)
	if status.State == app.ServerOn {
		endpoint = status.Endpoint + "/v1"
	}
	lines := []string{
		"Endpoint: " + endpoint,
		"Server:   " + string(status.State),
		fmt.Sprintf("Providers: %d active", m.activeProviderCount()),
		"Key:      " + m.currentAPIKeyPrefix(),
		"",
	}
	lines = append(lines, renderMenu(m.selected, []string{"Providers", "API Keys", "Server", "Settings", "Logs", "Codex Config", "Quit"})...)
	return strings.Join(lines, "\n")
}

func (m Model) viewProviders() string {
	lines := []string{
		"Provider: OpenAI Codex",
		"Default model: " + m.settings.DefaultModel,
		"Model routing: pass-through",
		"Reorder: Shift+Up / Shift+Down",
		"",
	}
	items := []string{"<- Back", "Add New Connection"}
	for _, account := range m.accounts {
		items = append(items, providerListLabel(account))
	}
	lines = append(lines, renderMenu(m.selected, items)...)
	return strings.Join(lines, "\n")
}

func (m Model) viewAddProvider() string {
	lines := []string{
		"Open this URL in your browser:",
		wrapURLForDisplay(m.authSession.AuthURL, 96),
	}
	if m.authURLPath != "" {
		lines = append(lines, "", "Full URL saved to: "+m.authURLPath)
	}
	if m.authURLWriteErr != nil {
		lines = append(lines, "Could not save URL file: "+m.authURLWriteErr.Error())
	}
	lines = append(lines,
		"",
		"Complete authorization in browser.",
		"Connection name:",
		m.providerNameInput.View(),
		"Paste callback URL:",
		m.callbackInput.View(),
	)
	return strings.Join(lines, "\n")
}

func (m Model) viewReauthProvider() string {
	lines := []string{
		"Open this URL in your browser:",
		wrapURLForDisplay(m.authSession.AuthURL, 96),
	}
	if m.authURLPath != "" {
		lines = append(lines, "", "Full URL saved to: "+m.authURLPath)
	}
	if m.authURLWriteErr != nil {
		lines = append(lines, "Could not save URL file: "+m.authURLWriteErr.Error())
	}
	if m.selectedAccount >= 0 && m.selectedAccount < len(m.accounts) {
		lines = append(lines, "",
			"Re-authenticating: "+providerDisplayName(m.accounts[m.selectedAccount]),
			"Existing alias and priority will be preserved.",
		)
	}
	lines = append(lines,
		"",
		"Complete authorization in browser.",
		"Paste callback URL:",
		m.callbackInput.View(),
		"",
		"Press Esc to cancel.",
	)
	return strings.Join(lines, "\n")
}

func (m Model) updateReauthProvider(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		return m.back(), nil
	case tea.KeyEnter:
		callbackURL := strings.TrimSpace(m.callbackInput.Value())
		if callbackURL == "" {
			m.statusLine = "Error: paste callback URL first"
			return m, nil
		}
		if m.selectedAccount < 0 || m.selectedAccount >= len(m.accounts) {
			m.statusLine = "Error: account not found"
			return m, nil
		}
		accountID := m.accounts[m.selectedAccount].ID
		session := m.authSession
		return m, func() tea.Msg {
			account, err := (app.ProviderService{DB: m.db, Logger: m.logger}).ReAuthFromCallback(m.ctx, session, accountID, callbackURL)
			return reauthDoneMsg{account: account, err: err}
		}
	}
	var cmd tea.Cmd
	m.callbackInput, cmd = m.callbackInput.Update(msg)
	return m, cmd
}

func (m Model) writeAuthURL() (string, error) {
	if strings.TrimSpace(m.dataDir) == "" || strings.TrimSpace(m.authSession.AuthURL) == "" {
		return "", nil
	}
	if err := os.MkdirAll(m.dataDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(m.dataDir, "openai-codex-auth-url.txt")
	return path, os.WriteFile(path, []byte(m.authSession.AuthURL+"\n"), 0o600)
}

func wrapURLForDisplay(url string, width int) string {
	if width <= 0 || len(url) <= width {
		return url
	}
	var b strings.Builder
	for len(url) > width {
		b.WriteString(url[:width])
		b.WriteByte('\n')
		url = url[width:]
	}
	b.WriteString(url)
	return b.String()
}

func (m Model) viewProviderDetail() string {
	if m.selectedAccount < 0 || m.selectedAccount >= len(m.accounts) {
		return "Connection not found"
	}
	account := m.accounts[m.selectedAccount]
	enableText := "Disable Connection"
	if !account.Enabled {
		enableText = "Enable Connection"
	}
	lines := []string{
		"Connection: " + providerDisplayName(account),
		"Alias:      " + account.Name,
		"Status:     " + app.FormatProviderStatus(account),
		"Priority:   " + fmt.Sprintf("%d", account.Priority),
		"Expires:    " + account.ExpiresAt.Local().Format(time.RFC3339),
	}
	switch {
	case m.providerQuotaLoading:
		lines = append(lines, "Quota:      loading...")
	case m.providerQuotaErr != nil:
		lines = append(lines, "Quota:      error: "+app.HumanError(m.providerQuotaErr.Error()))
	case m.providerQuota != nil:
		if m.providerQuota.Primary == nil && m.providerQuota.Secondary == nil {
			lines = append(lines, "Quota:      no data (no x-codex-*-used-percent header)")
			if keys := m.providerQuota.HeaderKeys; len(keys) > 0 {
				lines = append(lines, "            x-* headers seen: "+strings.Join(keys, ", "))
			}
		} else {
			if s := codex.FormatQuotaWindow(m.providerQuota.Primary); s != "" {
				lines = append(lines, "Quota:      "+s)
			}
			if s := codex.FormatQuotaWindow(m.providerQuota.Secondary); s != "" {
				lines = append(lines, "            "+s)
			}
		}
	}
	lines = append(lines, "")
	if m.aliasEditing {
		lines = append(lines, "Edit alias:", m.aliasInput.View())
		return strings.Join(lines, "\n")
	}
	lines = append(lines, renderMenu(m.selected, []string{"<- Back", "Edit Alias", "Test Connection", "Show Quota Limit", "Refresh Token", "Re-authenticate", enableText, "Delete Connection"})...)
	return strings.Join(lines, "\n")
}

func providerListLabel(account store.Account) string {
	display := providerDisplayName(account)
	status := app.FormatProviderStatus(account)
	if display == account.Name {
		return fmt.Sprintf("%s (%s)", display, status)
	}
	return fmt.Sprintf("%s (%s, %s)", display, account.Name, status)
}

func providerDisplayName(account store.Account) string {
	var meta struct {
		Email            string `json:"email"`
		ChatGPTAccountID string `json:"chatgpt_account_id"`
	}
	_ = json.Unmarshal([]byte(account.MetadataJSON), &meta)
	if strings.TrimSpace(meta.Email) != "" {
		return strings.TrimSpace(meta.Email)
	}
	if strings.TrimSpace(meta.ChatGPTAccountID) != "" {
		return strings.TrimSpace(meta.ChatGPTAccountID)
	}
	return account.Name
}

func (m Model) viewKeys() string {
	items := []string{"<- Back", "Create Key"}
	for _, key := range m.keys {
		items = append(items, fmt.Sprintf("Delete %s (%s, prefix=%s)", key.Name, key.ID, key.Prefix))
	}
	return strings.Join(renderMenu(m.selected, items), "\n")
}

func (m Model) viewServer() string {
	status := m.server.Status()
	action := "Start Server"
	if status.State == app.ServerOn {
		action = "Stop Server"
	}
	lines := []string{
		"Status:   " + string(status.State),
		"Endpoint: " + fmt.Sprintf("http://%s:%d/v1", m.settings.Host, m.settings.Port),
	}
	if status.Endpoint != "" {
		lines = append(lines, "Actual:   "+status.Endpoint+"/v1")
	}
	if status.Error != "" {
		lines = append(lines, "Error:    "+status.Error)
	}
	lines = append(lines, "")
	lines = append(lines, renderMenu(m.selected, []string{"<- Back", action, "Restart Server"})...)
	return strings.Join(lines, "\n")
}

func (m Model) viewSettings() string {
	values := []string{
		"<- Back",
		"Host: " + m.settings.Host,
		fmt.Sprintf("Port: %d", m.settings.Port),
		fmt.Sprintf("Log requests: %t", m.settings.LogRequests),
		fmt.Sprintf("Log upstream: %t", m.settings.LogUpstream),
		fmt.Sprintf("Body log limit: %d", m.settings.LogBodyLimit),
		"Default model: " + m.settings.DefaultModel,
		fmt.Sprintf("Upstream timeout (s): %d", m.settings.UpstreamTimeoutSeconds),
		"Save",
	}
	if m.settingEditing {
		values[m.settingSelected] = strings.Split(values[m.settingSelected], ":")[0] + ": " + m.settingInput.View()
	}
	return strings.Join(renderMenu(m.selected, values), "\n")
}

func (m Model) viewLogs() string {
	lines := renderMenu(m.selected, []string{"<- Back", "Filter: " + strings.ToUpper(m.logFilter), "Clear Logs"})
	lines = append(lines, "")
	entries := m.logger.Entries()
	if len(entries) == 0 {
		lines = append(lines, "No logs yet.")
		return strings.Join(lines, "\n")
	}
	start := len(entries) - 16
	if start < 0 {
		start = 0
	}
	for _, entry := range entries[start:] {
		if m.logFilter != "all" && entry.Source != m.logFilter {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s [%s] %s", entry.Time.Format("15:04:05"), entry.Source, entry.Message))
	}
	return strings.Join(lines, "\n")
}

func nextLogFilter(current string) string {
	switch current {
	case "all":
		return "proxy"
	case "proxy":
		return "openai"
	default:
		return "all"
	}
}

func (m Model) viewCodexConfig() string {
	key := "sk-lm-router-REPLACE_ME"
	if len(m.keys) > 0 {
		key = m.keys[0].Prefix + "-REPLACE_WITH_FULL_SECRET"
	}
	return app.CodexConfigText(m.settings.Port, key, m.settings.DefaultModel) + "\n" + strings.Join(renderMenu(m.selected, []string{"<- Back"}), "\n")
}

func renderMenu(selected int, items []string) []string {
	out := make([]string, 0, len(items))
	for i, item := range items {
		prefix := "☆"
		if i == selected {
			prefix = "★"
			out = append(out, selectedStyle.Render(prefix+" "+item))
			continue
		}
		out = append(out, "  "+prefix+" "+item)
	}
	return out
}

func title(v string) string {
	line := strings.Repeat("=", 58)
	return accentStyle.Render(line) + "\n" + titleStyle.Render("  "+v) + "\n" + accentStyle.Render(line)
}

func breadcrumb(v string) string {
	return crumbStyle.Render(v)
}

var (
	accentStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("209"))
	titleStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("209")).Bold(true)
	crumbStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("248"))
	selectedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("232")).Background(lipgloss.Color("230")).Bold(true)
	statusStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("114"))
)

type addProviderDoneMsg struct {
	account store.Account
	err     error
}

type providerTestDoneMsg struct {
	result app.ProviderTestResult
	err    error
}

type loadProvidersMsg struct {
	accounts []store.Account
	err      error
}

type loadKeysMsg struct {
	keys []store.APIKey
	err  error
}

type createdKeyMsg struct {
	key store.APIKey
	err error
}

type errMsg struct {
	err error
}

type clearStatusMsg struct {
	seq int
}

type providerQuotaDoneMsg struct {
	info codex.QuotaInfo
	err  error
}

type reauthDoneMsg struct {
	account store.Account
	err     error
}
