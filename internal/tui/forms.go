package tui

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

type serverDraft struct {
	Name, Host, User                                         string
	Port                                                     int
	Password, KeyPath, KeyPass, SudoPassword                 string
	Description, Location, Hardware, Services, Role, Caveats string
}

// newServerForm builds the add/edit form bound to d by pointer. Secret fields
// are masked; in add mode BOTH may stay empty (credential-less server, Plan 20
// C0), in edit mode empty = keep existing credential.
func newServerForm(d *serverDraft, editing bool) *huh.Form {
	credTitle := "密码（留空=保持不变）"
	if !editing {
		credTitle = "密码（可选，与密钥二选一）"
	}
	return huh.NewForm(
		huh.NewGroup(
			huh.NewInput().Title("名称（唯一）").Value(&d.Name).Validate(nonEmpty),
			huh.NewInput().Title("Host / IP").Value(&d.Host).Validate(nonEmpty),
			huh.NewInput().Title("SSH 用户").Value(&d.User).Validate(nonEmpty),
			portField(&d.Port),
		),
		huh.NewGroup(
			huh.NewInput().Title(credTitle).Value(&d.Password).EchoMode(huh.EchoModePassword),
			huh.NewInput().Title("私钥路径（可选，与密码互斥；编辑时留空=不变）").Value(&d.KeyPath),
			huh.NewInput().Title("密钥口令（可选）").Value(&d.KeyPass).EchoMode(huh.EchoModePassword),
			huh.NewInput().Title("sudo 密码（可选）").Value(&d.SudoPassword).EchoMode(huh.EchoModePassword),
		),
		huh.NewGroup(
			huh.NewInput().Title("硬件").Value(&d.Hardware),
			huh.NewInput().Title("位置").Value(&d.Location),
			huh.NewInput().Title("角色").Value(&d.Role),
			huh.NewInput().Title("服务").Value(&d.Services),
			huh.NewInput().Title("Caveats（agent 行动前必读）").Value(&d.Caveats),
			huh.NewInput().Title("备注").Value(&d.Description),
		),
	)
}

func nonEmpty(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("必填")
	}
	return nil
}

// portField binds an int draft field via a string mirror (huh Input is
// string-bound). Validation Atoi's the trimmed value — a field error on
// non-numeric input — and writes back into p on success.
func portField(p *int) *huh.Input {
	s := strconv.Itoa(*p)
	if *p == 0 { // add mode: default port 22
		s = "22"
	}
	return huh.NewInput().Title("端口").Value(&s).Validate(func(v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return errors.New("必须是整数")
		}
		if n < 1 || n > 65535 {
			return errors.New("端口范围 1-65535")
		}
		*p = n
		return nil
	})
}

// newProfileForm: single-field add-profile form (name, non-empty).
func newProfileForm(name *string) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("Profile 名称").Value(name).Validate(nonEmpty),
	))
}

// grantOptions builds MultiSelect options for the grant form: label = server
// name (display), value = server id (GrantServers wants ids; names are not
// unique so they must not be used as values).
func grantOptions(servers []*models.Server) []huh.Option[string] {
	opts := make([]huh.Option[string], len(servers))
	for i, s := range servers {
		opts[i] = huh.NewOption(s.Name, s.ID)
	}
	return opts
}

// newGrantForm builds the grant multi-select; chosen receives the selected
// server ids on submit. v1: selection starts EMPTY — grant is additive and
// INSERT OR IGNORE makes re-grants harmless, so pre-selection adds no safety.
func newGrantForm(servers []*models.Server, chosen *[]string) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[string]().Title("授权服务器（空格勾选，回车提交）").
			Options(grantOptions(servers)...).Value(chosen),
	))
}

// projectDraft is the create-project form state: a name plus the bound
// profile id (the Select's VALUE — AddProject wants the id, not the name).
type projectDraft struct {
	Name      string
	ProfileID string
}

// projectProfileOptions builds Select options: label = profile name (display),
// value = profile id (same label/value discipline as grantOptions).
func projectProfileOptions(profiles []*models.Profile) []huh.Option[string] {
	opts := make([]huh.Option[string], len(profiles))
	for i, p := range profiles {
		opts[i] = huh.NewOption(p.Name, p.ID)
	}
	return opts
}

// newProjectForm: name + profile select for creating an agent project.
func newProjectForm(d *projectDraft, profiles []*models.Profile) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("项目名称").Value(&d.Name).Validate(nonEmpty),
		huh.NewSelect[string]().Title("绑定 Profile").
			Options(projectProfileOptions(profiles)...).Value(&d.ProfileID),
	))
}

// deviceDraft is the issue-device-code form state. Name is persisted as the
// token's name; ServeURL is used ONLY to compose the usage hint in the
// one-time view — the broker cannot learn its own reachable address from
// cacheMeta/cred, so the operator supplies it. Never stored anywhere.
type deviceDraft struct {
	Name     string
	ServeURL string
}

// newCacheTokenForm: device name + serve address (usage-hint-only field).
func newCacheTokenForm(d *deviceDraft) *huh.Form {
	return huh.NewForm(huh.NewGroup(
		huh.NewInput().Title("设备名称（如 laptop，吊销后可重发）").Value(&d.Name).Validate(nonEmpty),
		huh.NewInput().Title("serve 地址（仅用于生成使用提示，不保存）").
			Placeholder("https://192.0.2.5:7878").Value(&d.ServeURL).Validate(nonEmpty),
	))
}

// submitGrant grants the chosen server ids to profileID.
func submitGrant(st *store.Store, profileID, profileName string, ids []string) tea.Cmd {
	return doAction(st, func() (string, error) {
		if len(ids) == 0 {
			return "未选择任何服务器", nil
		}
		desc := fmt.Sprintf("已授权 %d 台服务器到 %s", len(ids), profileName)
		return desc, st.GrantServers(profileID, ids)
	})
}

// prefill copies the editable non-secret fields of cur into a draft for the
// edit form (secret fields stay empty = keep existing credential).
func prefill(cur *models.Server) *serverDraft {
	return &serverDraft{
		Name: cur.Name, Host: cur.Host, User: cur.User, Port: cur.Port,
		Description: cur.Description, Location: cur.Location, Hardware: cur.Hardware,
		Services: cur.Services, Role: cur.Role, Caveats: cur.Caveats,
	}
}

// toServer assembles a models.Server from the draft, minting credentials via
// st when secret fields are filled. Password/key are mutually exclusive (CLI
// parity with serversAddCmd).
func (d *serverDraft) toServer(st *store.Store) (*models.Server, error) {
	if d.Password != "" && d.KeyPath != "" {
		return nil, errors.New("密码与私钥互斥：二选一")
	}
	srv := &models.Server{
		Name: strings.TrimSpace(d.Name), Host: strings.TrimSpace(d.Host),
		Port: d.Port, User: strings.TrimSpace(d.User),
		Description: strings.TrimSpace(d.Description), Location: strings.TrimSpace(d.Location),
		Hardware: strings.TrimSpace(d.Hardware), Services: strings.TrimSpace(d.Services),
		Role: strings.TrimSpace(d.Role), Caveats: strings.TrimSpace(d.Caveats),
	}
	switch {
	case d.Password != "":
		cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte(d.Password)})
		if err != nil {
			return nil, err
		}
		srv.CredentialID, srv.AuthMethod = cid, models.AuthPassword
	case d.KeyPath != "":
		keyBytes, err := os.ReadFile(d.KeyPath)
		if err != nil {
			return nil, err
		}
		cid, err := st.SetCredential(&models.Credential{Type: models.CredPrivateKey, Secret: keyBytes, Passphrase: []byte(d.KeyPass)})
		if err != nil {
			return nil, err
		}
		srv.CredentialID, srv.AuthMethod = cid, models.AuthPrivateKey
	}
	if d.SudoPassword != "" {
		sid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte(d.SudoPassword)})
		if err != nil {
			return nil, err
		}
		srv.SudoCredentialID = sid
	}
	return srv, nil
}

// formOverlay wraps a huh form as an App overlay; on completion it emits
// formDoneMsg{after: action}, so the action runs AFTER the overlay closes.
type formOverlay struct {
	title  string
	form   *huh.Form
	action func() tea.Cmd
}

func newFormOverlay(title string, f *huh.Form, action func() tea.Cmd) *formOverlay {
	return &formOverlay{title: title, form: f, action: action}
}

func (o *formOverlay) Title() string { return o.title }
func (o *formOverlay) Init() tea.Cmd { return o.form.Init() }

func (o *formOverlay) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if kp, ok := msg.(tea.KeyPressMsg); ok && kp.Code == tea.KeyEsc {
		// Esc cancels the form (abort semantics). aborted distinguishes a
		// real back-out from a bare dismiss: consumers that gate on the
		// answers (e.g. the upgrade segment) must NOT trust the bound
		// values — huh may already have committed a preset default into
		// them (selects pre-commit their default, prefilled inputs keep
		// their prefill).
		return o, func() tea.Msg { return formDoneMsg{aborted: true} }
	}
	f, cmd := o.form.Update(msg)
	if nf, ok := f.(*huh.Form); ok {
		o.form = nf
	}
	if o.form.State == huh.StateCompleted {
		return o, func() tea.Msg { return formDoneMsg{after: o.action()} }
	}
	if o.form.State == huh.StateAborted {
		return o, func() tea.Msg { return formDoneMsg{aborted: true} }
	}
	return o, cmd
}

func (o *formOverlay) View() tea.View {
	return tea.NewView(titleStyle.Render(" "+o.title+" ") + "\n（Esc 取消）\n" + o.form.View())
}

// doAction runs a store mutation off the UI loop and reports via messages.
func doAction(st *store.Store, fn func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		desc, err := fn()
		if err != nil {
			return errMsg{err}
		}
		return actionDoneMsg{desc}
	}
}

// submitServer assembles + persists the draft. cur == nil means add; otherwise
// edit: id is preserved and empty secret fields keep the existing credentials
// (full-row UpdateServer, same semantics as the CLI serversEditCmd).
func submitServer(st *store.Store, cur *models.Server, d *serverDraft) tea.Cmd {
	return doAction(st, func() (string, error) {
		srv, err := d.toServer(st)
		if err != nil {
			return "", err
		}
		if cur == nil {
			// Add mode: credential is optional (Plan 20 C0) — a draft with
			// neither password nor key persists a credential-less server
			// (toServer mints nothing when both are empty).
			_, err := st.AddServer(srv)
			return "已新增 " + srv.Name, err
		}
		if d.Password == "" && d.KeyPath == "" { // keep existing credential
			srv.CredentialID, srv.AuthMethod = cur.CredentialID, cur.AuthMethod
		}
		if d.SudoPassword == "" {
			srv.SudoCredentialID = cur.SudoCredentialID
		}
		srv.ID = cur.ID
		srv.Tags = cur.Tags // the form has no tags field — keep existing (CLI serversEditCmd parity)
		return "已更新 " + srv.Name, st.UpdateServer(srv)
	})
}
