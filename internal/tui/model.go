package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrisasuke/lm-router/internal/anthropic"
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
	screenProviderTypes
	screenClaudeRisk
	screenClaudeConfig
	screenAddCustomProvider
)

type addProviderField int

const (
	addProviderNameField addProviderField = iota
	addProviderCallbackField
)

// customProviderStep drives the custom-provider add/edit wizard, a separate
// state machine from addProviderField's 2-field OAuth flow — this one has up
// to 6 sequential steps and no OAuth browser-URL/callback concept at all.
type customProviderStep int

const (
	customStepCompatType customProviderStep = iota
	customStepName
	customStepPrefix
	customStepBaseURL
	customStepAPIKey
	customStepAPIType
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
	selectedProvider  string
	providerLoadSeq   uint64
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
	riskForReauth     bool

	customStep         customProviderStep
	customCompatType   string
	customAPIType      string
	customListSelected int
	customEditingID    string
	customNameInput    textinput.Model
	customPrefixInput  textinput.Model
	customBaseURLInput textinput.Model
	customAPIKeyInput  textinput.Model

	providerQuota        *codex.QuotaInfo
	claudeQuota          *anthropic.UsageInfo
	claudeQuotaRetryAt   map[string]time.Time
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
	customName := textinput.New()
	customName.Placeholder = "my-server"
	customName.CharLimit = 80
	customName.Width = 32
	customPrefix := textinput.New()
	customPrefix.Placeholder = "myapi"
	customPrefix.CharLimit = 40
	customPrefix.Width = 32
	customBaseURL := textinput.New()
	customBaseURL.Placeholder = "https://api.example.com/v1"
	customBaseURL.CharLimit = 200
	customBaseURL.Width = 60
	customAPIKey := textinput.New()
	customAPIKey.Placeholder = "sk-..."
	customAPIKey.CharLimit = 200
	customAPIKey.Width = 60
	customAPIKey.EchoMode = textinput.EchoPassword
	return Model{
		ctx:                ctx,
		db:                 db,
		logger:             logger,
		server:             server,
		settings:           settings,
		screen:             screenHome,
		dataDir:            dataDir,
		providerNameInput:  nameInput,
		callbackInput:      cb,
		aliasInput:         aliasInput,
		settingInput:       settingInput,
		logFilter:          "all",
		claudeQuotaRetryAt: make(map[string]time.Time),
		customNameInput:    customName,
		customPrefixInput:  customPrefix,
		customBaseURLInput: customBaseURL,
		customAPIKeyInput:  customAPIKey,
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
		if m.screen == screenAddCustomProvider {
			return m.updateAddCustomProvider(msg)
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
			return m.navigateBack()
		case tea.KeyShiftUp:
			// Priority ordering is meaningless for custom providers — a prefix
			// selects one connection directly, there is no pool to reorder.
			if m.screen == screenProviders && m.selectedProvider != store.ProviderCustom {
				return m.reorderSelectedProvider(-1)
			}
			m.move(-1)
		case tea.KeyShiftDown:
			if m.screen == screenProviders && m.selectedProvider != store.ProviderCustom {
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
		if len(m.stack) > 0 && m.stack[len(m.stack)-1] == screenProviders {
			m.stack = m.stack[:len(m.stack)-1]
		}
		m.selected = 0
	case customProviderDoneMsg:
		if msg.err != nil {
			m.statusLine = "Error: " + app.HumanError(msg.err.Error())
			return m, nil
		}
		if m.customEditingID == "" {
			m.accounts = append(m.accounts, msg.account)
		} else {
			for i, existing := range m.accounts {
				if existing.ID == msg.account.ID {
					m.accounts[i] = msg.account
					break
				}
			}
		}
		m.statusLine = fmt.Sprintf("Success: custom provider %q saved", msg.account.Name)
		m.screen = screenProviders
		if len(m.stack) > 0 && m.stack[len(m.stack)-1] == screenProviders {
			m.stack = m.stack[:len(m.stack)-1]
		}
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
		} else if msg.claude != nil {
			q := *msg.claude
			m.claudeQuota = &q
			if !q.RetryAt.IsZero() && msg.accountID != "" {
				if m.claudeQuotaRetryAt == nil {
					m.claudeQuotaRetryAt = make(map[string]time.Time)
				}
				m.claudeQuotaRetryAt[msg.accountID] = q.RetryAt
			} else if msg.accountID != "" {
				delete(m.claudeQuotaRetryAt, msg.accountID)
			}
			m.providerQuota = nil
			m.providerQuotaErr = nil
		} else {
			q := msg.info
			m.providerQuota = &q
			m.claudeQuota = nil
			m.providerQuotaErr = nil
		}
	case loadProvidersMsg:
		if msg.seq != m.providerLoadSeq || msg.provider != m.selectedProvider {
			break
		}
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
	case screenProviderTypes:
		b.WriteString(m.viewProviderTypes())
	case screenClaudeRisk:
		b.WriteString(m.viewClaudeRisk())
	case screenAddProvider:
		b.WriteString(m.viewAddProvider())
	case screenAddCustomProvider:
		b.WriteString(m.viewAddCustomProvider())
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
	case screenClaudeConfig:
		b.WriteString(m.viewClaudeConfig())
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
		if account.Provider == m.selectedProvider && strings.EqualFold(account.Name, name) {
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
	case screenProviderTypes:
		return m.activateProviderTypes()
	case screenClaudeRisk:
		return m.activateClaudeRisk()
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
	case screenClaudeConfig:
		return m.back(), nil
	}
	return m, nil
}

func (m Model) activateHome() (tea.Model, tea.Cmd) {
	switch m.selected {
	case 0:
		m.push(screenProviderTypes)
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
		m.push(screenClaudeConfig)
	case 7:
		return m, tea.Quit
	}
	return m, nil
}

func (m Model) activateProviderTypes() (tea.Model, tea.Cmd) {
	if m.selected == 0 {
		return m.navigateBack()
	}
	switch m.selected {
	case 1:
		m.selectedProvider = store.ProviderOpenAICodex
	case 2:
		m.selectedProvider = store.ProviderAnthropicClaude
	case 3:
		m.selectedProvider = store.ProviderCustom
	}
	m.accounts = nil
	m.selectedAccount = -1
	m.providerLoadSeq++
	m.push(screenProviders)
	return m, m.loadProvidersCmd()
}

func (m Model) activateClaudeRisk() (tea.Model, tea.Cmd) {
	if m.selected == 0 {
		return m.back(), nil
	}
	reauth := m.riskForReauth
	m = m.back()
	return m.beginProviderAuth(reauth)
}

func (m Model) activateProviders() (tea.Model, tea.Cmd) {
	if m.selected == 0 {
		return m.navigateBack()
	}
	if m.selected == 1 {
		if m.selectedProvider == store.ProviderAnthropicClaude {
			m.riskForReauth = false
			m.push(screenClaudeRisk)
			return m, nil
		}
		return m.beginProviderAuth(false)
	}
	idx := m.selected - 2
	if idx >= 0 && idx < len(m.accounts) {
		m.selectedAccount = idx
		m.providerQuota = nil
		m.claudeQuota = nil
		m.providerQuotaErr = nil
		m.providerQuotaLoading = false
		m.push(screenProviderDetail)
	}
	return m, nil
}

func (m Model) beginProviderAuth(reauth bool) (tea.Model, tea.Cmd) {
	if m.selectedProvider == store.ProviderCustom {
		// Custom providers have no OAuth flow to begin; reauth is never true
		// here since the detail screen omits Re-authenticate for them.
		return m.beginAddCustomProvider("")
	}
	service := app.ProviderService{DB: m.db}
	redirectURI := "http://localhost:1455/auth/callback"
	if m.selectedProvider == store.ProviderAnthropicClaude {
		redirectURI = "https://console.anthropic.com/oauth/code/callback"
	}
	m.authSession = service.NewAuthSessionForProvider(m.selectedProvider, redirectURI)
	m.authURLPath, m.authURLWriteErr = m.writeAuthURL()
	m.callbackInput.SetValue("")
	if reauth {
		m.callbackInput.Focus()
		m.push(screenReauthProvider)
		return m, textinput.Blink
	}
	m.providerNameInput.SetValue("")
	m.focusAddProviderField(addProviderNameField)
	m.push(screenAddProvider)
	return m, textinput.Blink
}

// detailAction identifies a provider-detail menu row independent of its
// position, so a shorter custom-provider menu can't have its rows
// misinterpreted as the longer OAuth menu's rows at the same index.
type detailAction int

const (
	detailBack detailAction = iota
	detailEdit
	detailTest
	detailQuota
	detailRefresh
	detailReauth
	detailToggleEnabled
	detailDelete
)

type detailMenuItem struct {
	Label  string
	Action detailAction
}

func providerDetailMenu(account store.Account) []detailMenuItem {
	enableText := "Disable Connection"
	if !account.Enabled {
		enableText = "Enable Connection"
	}
	if account.Provider == store.ProviderCustom {
		return []detailMenuItem{
			{"<- Back", detailBack},
			{"Edit Connection", detailEdit},
			{"Test Connection", detailTest},
			{enableText, detailToggleEnabled},
			{"Delete Connection", detailDelete},
		}
	}
	return []detailMenuItem{
		{"<- Back", detailBack},
		{"Edit Alias", detailEdit},
		{"Test Connection", detailTest},
		{"Show Quota Limit", detailQuota},
		{"Refresh Token", detailRefresh},
		{"Re-authenticate", detailReauth},
		{enableText, detailToggleEnabled},
		{"Delete Connection", detailDelete},
	}
}

// beginAddCustomProvider starts the custom-provider wizard for either Add
// (editID == "") or Edit (editID identifies the account to pre-seed from and
// update in place). It is the single screen used for both.
func (m Model) beginAddCustomProvider(editID string) (tea.Model, tea.Cmd) {
	m.customEditingID = editID
	m.customCompatType = ""
	m.customAPIType = ""
	m.customListSelected = 0
	m.customNameInput.SetValue("")
	m.customPrefixInput.SetValue("")
	m.customBaseURLInput.SetValue("")
	m.customAPIKeyInput.SetValue("")
	m.customStep = customStepCompatType
	if editID != "" {
		for _, account := range m.accounts {
			if account.ID != editID {
				continue
			}
			m.customCompatType = account.CompatType
			m.customAPIType = account.APIType
			m.customNameInput.SetValue(account.Name)
			m.customPrefixInput.SetValue(account.Prefix)
			m.customBaseURLInput.SetValue(account.BaseURL)
			break
		}
		// Compat type can't change on edit (UpdateCustomProviderParams has no
		// field for it), so the wizard starts one step in.
		m.customStep = customStepName
	}
	m.push(screenAddCustomProvider)
	m.focusCustomStep()
	return m, textinput.Blink
}

func (m *Model) focusCustomStep() {
	m.customNameInput.Blur()
	m.customPrefixInput.Blur()
	m.customBaseURLInput.Blur()
	m.customAPIKeyInput.Blur()
	switch m.customStep {
	case customStepName:
		m.customNameInput.Focus()
	case customStepPrefix:
		m.customPrefixInput.Focus()
	case customStepBaseURL:
		m.customBaseURLInput.Focus()
	case customStepAPIKey:
		m.customAPIKeyInput.Focus()
	}
}

func (m Model) updateAddCustomProvider(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	isListStep := m.customStep == customStepCompatType || m.customStep == customStepAPIType
	switch msg.Type {
	case tea.KeyEsc:
		return m.back(), nil
	case tea.KeyUp, tea.KeyDown:
		if isListStep {
			m.customListSelected = (m.customListSelected + 1) % 2
		}
		return m, nil
	case tea.KeyEnter:
		return m.advanceCustomProviderStep()
	}
	if isListStep {
		return m, nil
	}
	var cmd tea.Cmd
	switch m.customStep {
	case customStepName:
		m.customNameInput, cmd = m.customNameInput.Update(msg)
	case customStepPrefix:
		m.customPrefixInput, cmd = m.customPrefixInput.Update(msg)
	case customStepBaseURL:
		m.customBaseURLInput, cmd = m.customBaseURLInput.Update(msg)
	case customStepAPIKey:
		m.customAPIKeyInput, cmd = m.customAPIKeyInput.Update(msg)
	}
	return m, cmd
}

func (m Model) advanceCustomProviderStep() (tea.Model, tea.Cmd) {
	switch m.customStep {
	case customStepCompatType:
		if m.customListSelected == 0 {
			m.customCompatType = store.CompatOpenAIStyle
		} else {
			m.customCompatType = store.CompatAnthropicStyle
		}
		m.customStep = customStepName
	case customStepName:
		if strings.TrimSpace(m.customNameInput.Value()) == "" {
			m.statusLine = "Error: name is required"
			return m, nil
		}
		m.customStep = customStepPrefix
	case customStepPrefix:
		if err := m.validatePrefix(m.customPrefixInput.Value(), m.customEditingID); err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
		m.customStep = customStepBaseURL
	case customStepBaseURL:
		if err := validateBaseURL(strings.TrimSpace(m.customBaseURLInput.Value())); err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
		m.customStep = customStepAPIKey
	case customStepAPIKey:
		if m.customEditingID == "" && strings.TrimSpace(m.customAPIKeyInput.Value()) == "" {
			m.statusLine = "Error: API key is required"
			return m, nil
		}
		if m.customCompatType != store.CompatOpenAIStyle {
			return m.submitCustomProvider()
		}
		m.customStep = customStepAPIType
		// Derive the cursor from the current value (pre-seeded from the account
		// on edit, "" on add) instead of always resetting to 0 — otherwise
		// editing a "responses" connection silently downgrades it to "chat" on
		// the next Enter, since the cursor is what customStepAPIType reads from.
		if m.customAPIType == store.CustomAPITypeResponses {
			m.customListSelected = 1
		} else {
			m.customListSelected = 0
		}
	case customStepAPIType:
		if m.customListSelected == 0 {
			m.customAPIType = store.CustomAPITypeChat
		} else {
			m.customAPIType = store.CustomAPITypeResponses
		}
		return m.submitCustomProvider()
	}
	m.focusCustomStep()
	return m, nil
}

func (m Model) submitCustomProvider() (tea.Model, tea.Cmd) {
	name := strings.TrimSpace(m.customNameInput.Value())
	prefix := strings.ToLower(strings.TrimSpace(m.customPrefixInput.Value()))
	baseURL := strings.TrimSpace(m.customBaseURLInput.Value())
	apiKey := m.customAPIKeyInput.Value()
	compatType := m.customCompatType
	apiType := m.customAPIType
	editID := m.customEditingID
	db := m.db
	ctx := m.ctx
	return m, func() tea.Msg {
		if editID == "" {
			account, err := (app.ProviderService{DB: db}).AddCustomProvider(ctx, app.AddCustomProviderParams{
				Name: name, Prefix: prefix, BaseURL: baseURL, APIKey: apiKey,
				CompatType: compatType, APIType: apiType,
			})
			return customProviderDoneMsg{account: account, err: err}
		}
		params := app.UpdateCustomProviderParams{Name: &name, Prefix: &prefix, BaseURL: &baseURL, APIType: &apiType}
		if apiKey != "" {
			params.APIKey = &apiKey
		}
		account, err := (app.ProviderService{DB: db}).UpdateCustomProvider(ctx, editID, params)
		return customProviderDoneMsg{account: account, err: err}
	}
}

func (m Model) viewAddCustomProvider() string {
	switch m.customStep {
	case customStepCompatType:
		lines := []string{"Select type:"}
		lines = append(lines, renderMenu(m.customListSelected, []string{"openai-compatible", "anthropic-compatible"})...)
		return strings.Join(lines, "\n")
	case customStepAPIType:
		lines := []string{"API Type:"}
		lines = append(lines, renderMenu(m.customListSelected, []string{"chat (chat/completions)", "responses"})...)
		return strings.Join(lines, "\n")
	case customStepName:
		return "Name:\n" + m.customNameInput.View()
	case customStepPrefix:
		return "Prefix (used in model IDs, e.g. myapi):\n" + m.customPrefixInput.View()
	case customStepBaseURL:
		return "Base URL (e.g. https://api.example.com/v1):\n" + m.customBaseURLInput.View()
	case customStepAPIKey:
		lines := []string{"API Key:", m.customAPIKeyInput.View()}
		if m.customEditingID != "" {
			lines = append(lines, "", "Leave blank to keep the current key.")
		}
		return strings.Join(lines, "\n")
	default:
		return ""
	}
}

// validatePrefix checks syntax locally and gives an early uniqueness hint;
// the authoritative check runs server-side in store.UpsertAccount on submit.
func (m Model) validatePrefix(raw, excludeID string) error {
	prefix := strings.ToLower(strings.TrimSpace(raw))
	if prefix == "" {
		return fmt.Errorf("prefix is required")
	}
	if strings.ContainsAny(prefix, "/ \t\n") {
		return fmt.Errorf("prefix must not contain slashes or whitespace")
	}
	if m.db == nil {
		return nil
	}
	existing, err := m.db.GetAccountByPrefix(m.ctx, prefix)
	if err != nil {
		return nil
	}
	if existing.ID != excludeID {
		return fmt.Errorf("prefix %q is already in use", prefix)
	}
	return nil
}

func validateBaseURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("base URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("base URL must be a valid http(s) URL")
	}
	return nil
}

func (m Model) activateProviderDetail() (tea.Model, tea.Cmd) {
	if m.selectedAccount < 0 || m.selectedAccount >= len(m.accounts) {
		return m.back(), nil
	}
	account := m.accounts[m.selectedAccount]
	menu := providerDetailMenu(account)
	if m.selected < 0 || m.selected >= len(menu) {
		return m, nil
	}
	switch menu[m.selected].Action {
	case detailBack:
		return m.back(), nil
	case detailEdit:
		if account.Provider == store.ProviderCustom {
			return m.beginAddCustomProvider(account.ID)
		}
		m.aliasInput.SetValue(account.Name)
		m.aliasInput.Focus()
		m.aliasEditing = true
		return m, textinput.Blink
	case detailTest:
		return m, func() tea.Msg {
			result, err := (app.ProviderService{DB: m.db, Logger: m.logger}).Test(m.ctx, account, m.settings.DefaultModel)
			return providerTestDoneMsg{result: result, err: err}
		}
	case detailQuota:
		if m.providerQuotaLoading {
			return m, nil
		}
		if account.Provider == store.ProviderAnthropicClaude {
			retryAt := m.claudeQuotaRetryAt[account.ID]
			if retryAt.After(time.Now()) {
				return m.setTimedStatus("Claude quota check is cooling down until " + retryAt.Local().Format("15:04:05"))
			}
		}
		m.providerQuota = nil
		m.providerQuotaErr = nil
		m.providerQuotaLoading = true
		if account.Provider == store.ProviderAnthropicClaude {
			return m, func() tea.Msg {
				info, err := (app.ProviderService{DB: m.db, Logger: m.logger}).ClaudeQuota(m.ctx, account)
				return providerQuotaDoneMsg{accountID: account.ID, claude: &info, err: err}
			}
		}
		return m, func() tea.Msg {
			info, err := (app.ProviderService{DB: m.db, Logger: m.logger}).Quota(m.ctx, account)
			return providerQuotaDoneMsg{info: info, err: err}
		}
	case detailRefresh:
		refreshed, err := (app.ProviderService{DB: m.db}).Refresh(m.ctx, account.ID)
		if err != nil {
			m.statusLine = "Error: " + app.HumanError(err.Error())
			return m, nil
		}
		m.accounts[m.selectedAccount] = refreshed
		m.statusLine = "Success: provider refreshed"
	case detailReauth:
		m.selectedProvider = account.Provider
		if account.Provider == store.ProviderAnthropicClaude {
			m.riskForReauth = true
			m.push(screenClaudeRisk)
			return m, nil
		}
		return m.beginProviderAuth(true)
	case detailToggleEnabled:
		err := (app.ProviderService{DB: m.db}).SetEnabled(m.ctx, account.ID, !account.Enabled)
		if err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
		account.Enabled = !account.Enabled
		m.accounts[m.selectedAccount] = account
		m.statusLine = "Success: provider updated"
	case detailDelete:
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

// navigateBack applies provider-pool cleanup consistently for both the
// explicit Back rows and keyboard navigation. It also invalidates in-flight
// loads so a delayed result cannot repopulate the next screen with stale rows.
func (m Model) navigateBack() (tea.Model, tea.Cmd) {
	leaving := m.screen
	m = m.back()
	switch leaving {
	case screenProviders:
		m.selectedProvider = ""
		m.accounts = nil
		m.selectedAccount = -1
		m.providerLoadSeq++
	case screenProviderTypes:
		m.selectedProvider = ""
		m.providerLoadSeq++
		return m, m.loadProvidersCmd()
	}
	return m, nil
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
		provider := m.selectedProvider
		if provider == "" {
			provider = current.Provider
		}
		swapped, err := m.db.SwapAccountPrioritiesCAS(m.ctx, provider,
			current.ID, currentPriority, other.ID, otherPriority)
		if err != nil {
			m.statusLine = "Error: " + err.Error()
			return m, nil
		}
		if !swapped {
			m.statusLine = "Error: provider priorities changed; reload and retry"
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
		return 8
	case screenProviderTypes:
		return 4
	case screenProviders:
		return 2 + len(m.accounts)
	case screenClaudeRisk:
		return 2
	case screenProviderDetail:
		if m.selectedAccount >= 0 && m.selectedAccount < len(m.accounts) {
			return len(providerDetailMenu(m.accounts[m.selectedAccount]))
		}
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
	case screenClaudeConfig:
		return 1
	default:
		return 0
	}
}

func (m Model) loadProvidersCmd() tea.Cmd {
	seq := m.providerLoadSeq
	provider := m.selectedProvider
	return func() tea.Msg {
		if m.db == nil {
			return loadProvidersMsg{provider: provider, seq: seq}
		}
		var accounts []store.Account
		var err error
		if provider == "" {
			accounts, err = (app.ProviderService{DB: m.db}).List(m.ctx)
		} else {
			accounts, err = (app.ProviderService{DB: m.db}).ListProvider(m.ctx, provider)
		}
		return loadProvidersMsg{provider: provider, seq: seq, accounts: accounts, err: err}
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
	case screenProviderTypes:
		return "Providers"
	case screenProviders:
		if m.selectedProvider == store.ProviderCustom {
			return providerTitle(m.selectedProvider)
		}
		return providerTitle(m.selectedProvider) + " (OAUTH)"
	case screenAddProvider:
		return "Add " + providerTitle(m.selectedProvider)
	case screenAddCustomProvider:
		if m.customEditingID == "" {
			return "Add Custom Provider"
		}
		return "Edit Custom Provider"
	case screenClaudeRisk:
		return "Anthropic Claude Risk Warning"
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
	case screenClaudeConfig:
		return "Claude Config"
	default:
		return "LM Router Terminal UI"
	}
}

func (m Model) breadcrumb() string {
	switch m.screen {
	case screenHome:
		return "LM Router"
	case screenProviderTypes:
		return "LM Router > Providers"
	case screenProviders:
		return "LM Router > Providers > " + providerTitle(m.selectedProvider)
	case screenAddProvider:
		return "LM Router > Providers > " + providerTitle(m.selectedProvider) + " > Add"
	case screenAddCustomProvider:
		if m.customEditingID == "" {
			return "LM Router > Providers > Custom Provider > Add"
		}
		return "LM Router > Providers > Custom Provider > Edit"
	case screenClaudeRisk:
		return "LM Router > Providers > Anthropic Claude > Risk"
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
	case screenClaudeConfig:
		return "LM Router > Claude Config"
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
	lines = append(lines, renderMenu(m.selected, []string{"Providers", "API Keys", "Server", "Settings", "Logs", "Codex Config", "Claude Config", "Quit"})...)
	return strings.Join(lines, "\n")
}

func (m Model) viewProviderTypes() string {
	return strings.Join(renderMenu(m.selected, []string{"<- Back", "OpenAI Codex", "Anthropic Claude", "Custom Provider"}), "\n")
}

func (m Model) viewProviders() string {
	lines := []string{
		"Provider: " + providerTitle(m.selectedProvider),
		"Default model: " + providerDefaultModel(m.selectedProvider, m.settings.DefaultModel),
		"Model routing: " + providerRouting(m.selectedProvider),
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

func (m Model) viewClaudeRisk() string {
	lines := []string{
		"Claude subscriptions and the Anthropic API are separate products.",
		"Using Claude subscription OAuth through a router is not an officially",
		"licensed Anthropic API flow and may put the connected account at risk.",
		"Automatic fallback combines capacity across subscriptions and may be",
		"treated as bypassing limits; it cannot prevent provider restrictions.",
		"",
	}
	lines = append(lines, renderMenu(m.selected, []string{"Cancel", "I understand, continue"})...)
	return strings.Join(lines, "\n")
}

func providerTitle(provider string) string {
	switch provider {
	case store.ProviderAnthropicClaude:
		return "Anthropic Claude"
	case store.ProviderCustom:
		return "Custom Provider"
	default:
		return "OpenAI Codex"
	}
}

func providerRouting(provider string) string {
	switch provider {
	case store.ProviderAnthropicClaude:
		return "claude* via native Messages API"
	case store.ProviderCustom:
		return "<prefix>/<model> via configured connection (passthrough)"
	default:
		return "gpt* via Codex Responses"
	}
}

func providerDefaultModel(provider, configured string) string {
	switch provider {
	case store.ProviderAnthropicClaude:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(configured)), "claude") {
			return configured
		}
		return app.DefaultClaudeModel
	case store.ProviderCustom:
		return "n/a — set per connection"
	default:
		return configured
	}
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
	callbackPrompt := "Paste callback URL:"
	if m.authSession.Provider == store.ProviderAnthropicClaude {
		callbackPrompt = "Paste callback URL or code#state:"
	}
	lines = append(lines,
		"",
		"Complete authorization in browser.",
		"Connection name:",
		m.providerNameInput.View(),
		callbackPrompt,
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
	callbackPrompt := "Paste callback URL:"
	if m.authSession.Provider == store.ProviderAnthropicClaude {
		callbackPrompt = "Paste callback URL or code#state:"
	}
	lines = append(lines,
		"",
		"Complete authorization in browser.",
		callbackPrompt,
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
	filename := "openai-codex-auth-url.txt"
	if m.authSession.Provider == store.ProviderAnthropicClaude {
		filename = "anthropic-claude-auth-url.txt"
	}
	path := filepath.Join(m.dataDir, filename)
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
	lines := []string{
		"Connection: " + providerDisplayName(account),
		"Alias:      " + account.Name,
		"Status:     " + app.FormatProviderStatus(account),
		"Priority:   " + fmt.Sprintf("%d", account.Priority),
	}
	if account.Provider == store.ProviderCustom {
		lines = append(lines,
			"Prefix:     "+account.Prefix,
			"Base URL:   "+account.BaseURL,
			"Compat:     "+account.CompatType,
		)
		if account.APIType != "" {
			lines = append(lines, "API Type:   "+account.APIType)
		}
	} else {
		lines = append(lines, "Expires:    "+account.ExpiresAt.Local().Format(time.RFC3339))
	}
	switch {
	case m.providerQuotaLoading:
		lines = append(lines, "Quota:      loading...")
	case m.providerQuotaErr != nil:
		lines = append(lines, "Quota:      error: "+app.HumanError(m.providerQuotaErr.Error()))
	case m.claudeQuota != nil:
		if !m.claudeQuota.Available {
			lines = append(lines, "Quota:      connected; temporarily unavailable")
		} else if len(m.claudeQuota.Windows) == 0 {
			lines = append(lines, "Quota:      no usage windows returned")
		} else {
			for i, window := range m.claudeQuota.Windows {
				prefix := "            "
				if i == 0 {
					prefix = "Quota:      "
				}
				reset := ""
				if !window.ResetsAt.IsZero() {
					reset = " — resets " + window.ResetsAt.Local().Format("2 Jan 15:04")
				}
				lines = append(lines, fmt.Sprintf("%s%s (%.0f%%)%s", prefix, window.Name, window.Utilization, reset))
			}
		}
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
	menu := providerDetailMenu(account)
	labels := make([]string, len(menu))
	for i, item := range menu {
		labels[i] = item.Label
	}
	lines = append(lines, renderMenu(m.selected, labels)...)
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

func (m Model) viewClaudeConfig() string {
	key := "sk-lm-router-REPLACE_ME"
	if len(m.keys) > 0 {
		key = m.keys[0].Prefix + "-REPLACE_WITH_FULL_SECRET"
	}
	model := app.DefaultClaudeModel
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(m.settings.DefaultModel)), "claude") {
		model = m.settings.DefaultModel
	}
	return app.ClaudeConfigText(m.settings.Port, key, model) + "\n" + strings.Join(renderMenu(m.selected, []string{"<- Back"}), "\n")
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
	provider string
	seq      uint64
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
	accountID string
	info      codex.QuotaInfo
	claude    *anthropic.UsageInfo
	err       error
}

type reauthDoneMsg struct {
	account store.Account
	err     error
}

type customProviderDoneMsg struct {
	account store.Account
	err     error
}
