package tui

import (
	"errors"
	"os"
	"strconv"
	"strings"

	"charm.land/huh/v2"
	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

type serverDraft struct {
	Name, Host, User   string
	Port               int
	Password, KeyPath, KeyPass, SudoPassword string
	Description, Location, Hardware, Services, Role, Caveats string
}

// newServerForm builds the add/edit form bound to d by pointer. Secret fields
// are masked and OPTIONAL in edit mode (empty = keep existing credential).
func newServerForm(d *serverDraft, editing bool) *huh.Form {
	credTitle := "密码（留空=保持不变）"
	if !editing {
		credTitle = "密码（与密钥二选一）"
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
			huh.NewInput().Title("私钥路径（与密码二选一；编辑时留空=不变）").Value(&d.KeyPath),
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
		return o, func() tea.Msg { return formDoneMsg{} } // Esc cancels the form (abort semantics)
	}
	f, cmd := o.form.Update(msg)
	if nf, ok := f.(*huh.Form); ok {
		o.form = nf
	}
	if o.form.State == huh.StateCompleted {
		return o, func() tea.Msg { return formDoneMsg{after: o.action()} }
	}
	if o.form.State == huh.StateAborted {
		return o, func() tea.Msg { return formDoneMsg{} }
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
			// Add mode: require exactly one credential (CLI serversAddCmd parity)
			if d.Password == "" && d.KeyPath == "" {
				return "", errors.New("凭据必填：密码或私钥路径二选一（与 CLI servers add 一致）")
			}
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
