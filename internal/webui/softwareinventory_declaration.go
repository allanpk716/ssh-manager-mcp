package webui

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// SoftwareInventoryFormPort 是声明表单里的一个监听端口。
type SoftwareInventoryFormPort struct {
	Address  string
	Port     int
	Protocol string
	Exposure string
}

// SoftwareInventoryDeclarationForm 是新增与修改声明共用的表单数据。
type SoftwareInventoryDeclarationForm struct {
	ID                 string
	Device             string
	Category           string
	Name               string
	Version            string
	PackageURL         string
	CPE                string
	ListeningPortsText string
	BelongsTo          string
	Note               string
	ObservedStableKey  string
	ListeningPorts     []SoftwareInventoryFormPort
	Error              string
}

// SoftwareInventoryDeclaration 是一条生效声明的页面数据。
type SoftwareInventoryDeclaration struct {
	ID                 string
	Device             string
	Category           string
	CategoryLabel      string
	Name               string
	Version            string
	PackageURL         string
	CPE                string
	BelongsTo          string
	Note               string
	ListeningPorts     []SoftwareInventoryFormPort
	ClaimedObservedKey string
}

// SoftwareInventoryDeclarationNotFoundError 表示声明不存在或已不可见。
type SoftwareInventoryDeclarationNotFoundError struct {
	ID string
}

func (e *SoftwareInventoryDeclarationNotFoundError) Error() string {
	return "软件声明 " + e.ID + " 不存在"
}

// handleSoftwareInventoryDeclarationNew 渲染声明表单并处理新增提交。从待
// 办进入时，表单携带设备、端口与实测条目稳定键，提交后由来源接口完成认领。
func (h *handler) handleSoftwareInventoryDeclarationNew(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		query := r.URL.Query()
		form := declarationFormFromQuery(query)
		h.renderDeclarationForm(w, r, http.StatusOK, "新增软件声明", form)
	case http.MethodPost:
		form := declarationFormFromPost(r)
		ports, err := parseSoftwareInventoryPorts(form.ListeningPortsText)
		if err != nil {
			form.Error = err.Error()
			h.renderDeclarationForm(w, r, http.StatusBadRequest, "新增软件声明", form)
			return
		}
		form.ListeningPorts = ports
		_, err = h.cfg.SoftwareInventory.AddSoftwareInventoryDeclaration(
			form, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
		)
		if err != nil {
			h.renderDeclarationWriteError(w, r, "新增软件声明", form, err)
			return
		}
		http.Redirect(w, r, "/ui/software-inventory/todos?notice=declaration-added", http.StatusSeeOther)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "新增软件声明页只接受 GET 与 POST 请求。")
	}
}

func (h *handler) handleSoftwareInventoryDeclarations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "生效软件声明页只接受 GET 请求。")
		return
	}
	declarations, err := h.cfg.SoftwareInventory.SoftwareInventoryDeclarations()
	if err != nil {
		h.renderError(w, r, http.StatusInternalServerError, "读取生效软件声明失败", "读取生效软件声明失败，请稍后重试。")
		return
	}
	data := pageData{Title: "生效软件声明", SoftwareInventoryDeclarations: declarations}
	if r.URL.Query().Get("notice") == "declaration-deleted" {
		data.SoftwareInventoryNotice = "软件声明已删除。"
	}
	h.renderPage(w, r, http.StatusOK, "softwareinventory-declarations.tmpl", data)
}

func (h *handler) handleSoftwareInventoryDeclarationEdit(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		declaration, err := h.cfg.SoftwareInventory.SoftwareInventoryDeclaration(id)
		if err != nil {
			h.renderDeclarationReadError(w, r, err)
			return
		}
		h.renderDeclarationForm(w, r, http.StatusOK, "修改软件声明", declarationForm(declaration))
	case http.MethodPost:
		form := declarationFormFromPost(r)
		ports, err := parseSoftwareInventoryPorts(form.ListeningPortsText)
		if err != nil {
			form.Error = err.Error()
			h.renderDeclarationForm(w, r, http.StatusBadRequest, "修改软件声明", form)
			return
		}
		form.ListeningPorts = ports
		if err := h.cfg.SoftwareInventory.EditSoftwareInventoryDeclaration(
			form.ID, form, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
		); err != nil {
			h.renderDeclarationWriteError(w, r, "修改软件声明", form, err)
			return
		}
		notice := "declaration-edited"
		if r.PostFormValue("action") == "claim" && form.ObservedStableKey != "" {
			if err := h.cfg.SoftwareInventory.ClaimSoftwareInventoryDeclaration(
				form.ID, form.ObservedStableKey, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
			); err != nil {
				h.renderDeclarationWriteError(w, r, "修改软件声明", form, err)
				return
			}
			notice = "declaration-claimed"
		}
		http.Redirect(w, r, "/ui/software-inventory/todos?notice="+notice, http.StatusSeeOther)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "修改软件声明页只接受 GET 与 POST 请求。")
	}
}

func (h *handler) handleSoftwareInventoryDeclarationDelete(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		declaration, err := h.cfg.SoftwareInventory.SoftwareInventoryDeclaration(id)
		if err != nil {
			h.renderDeclarationReadError(w, r, err)
			return
		}
		h.renderPage(w, r, http.StatusOK, "softwareinventory-declaration-delete.tmpl", pageData{
			Title: "删除软件声明", SoftwareInventoryDeclaration: declaration,
		})
	case http.MethodPost:
		id := strings.TrimSpace(r.PostFormValue("id"))
		if err := h.cfg.SoftwareInventory.DeleteSoftwareInventoryDeclaration(
			id, SoftwareInventoryWriteSource{RemoteAddress: remoteIP(r)},
		); err != nil {
			var missing *SoftwareInventoryDeclarationNotFoundError
			if errors.As(err, &missing) {
				h.renderDeclarationReadError(w, r, err)
				return
			}
			h.renderError(w, r, http.StatusInternalServerError, "删除软件声明失败", "删除软件声明失败，请稍后重试。")
			return
		}
		http.Redirect(w, r, "/ui/software-inventory/declarations?notice=declaration-deleted", http.StatusSeeOther)
	default:
		h.renderError(w, r, http.StatusMethodNotAllowed, "方法不允许", "删除软件声明页只接受 GET 与 POST 请求。")
	}
}

func declarationFormFromQuery(query url.Values) SoftwareInventoryDeclarationForm {
	get := query.Get
	form := SoftwareInventoryDeclarationForm{
		Device: strings.TrimSpace(get("device")), Category: "service_program",
		Name: strings.TrimSpace(get("name")), Version: strings.TrimSpace(get("version")),
		PackageURL: get("purl"), CPE: get("cpe"), BelongsTo: get("belongs_to"), Note: get("note"),
		ObservedStableKey: strings.TrimSpace(get("observed_key")),
	}
	port := strings.TrimSpace(get("port_number"))
	if port != "" {
		form.ListeningPortsText = strings.Join([]string{
			get("port_address"), port, get("port_protocol"), get("port_exposure"),
		}, ",")
	}
	return form
}

func declarationFormFromPost(r *http.Request) SoftwareInventoryDeclarationForm {
	return SoftwareInventoryDeclarationForm{
		ID:                 strings.TrimSpace(r.PostFormValue("id")),
		Device:             strings.TrimSpace(r.PostFormValue("device")),
		Category:           strings.TrimSpace(r.PostFormValue("category")),
		Name:               strings.TrimSpace(r.PostFormValue("name")),
		Version:            strings.TrimSpace(r.PostFormValue("version")),
		PackageURL:         strings.TrimSpace(r.PostFormValue("purl")),
		CPE:                strings.TrimSpace(r.PostFormValue("cpe")),
		ListeningPortsText: strings.TrimSpace(r.PostFormValue("ports")),
		BelongsTo:          strings.TrimSpace(r.PostFormValue("belongs_to")),
		Note:               strings.TrimSpace(r.PostFormValue("note")),
		ObservedStableKey:  strings.TrimSpace(r.PostFormValue("observed_key")),
	}
}

func declarationForm(declaration *SoftwareInventoryDeclaration) SoftwareInventoryDeclarationForm {
	lines := make([]string, 0, len(declaration.ListeningPorts))
	for _, port := range declaration.ListeningPorts {
		lines = append(lines, strings.Join([]string{
			port.Address, strconv.Itoa(port.Port), port.Protocol, port.Exposure,
		}, ","))
	}
	return SoftwareInventoryDeclarationForm{
		ID: declaration.ID, Device: declaration.Device, Category: declaration.Category,
		Name: declaration.Name, Version: declaration.Version, PackageURL: declaration.PackageURL,
		CPE: declaration.CPE, ListeningPortsText: strings.Join(lines, "\n"),
		BelongsTo: declaration.BelongsTo, Note: declaration.Note,
		ObservedStableKey: declaration.ClaimedObservedKey,
	}
}

func parseSoftwareInventoryPorts(text string) ([]SoftwareInventoryFormPort, error) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	ports := make([]SoftwareInventoryFormPort, 0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 4 {
			return nil, errors.New("监听端口格式不合法；每行应为 监听地址,端口,协议,暴露级别代号")
		}
		port, err := strconv.Atoi(strings.TrimSpace(fields[1]))
		if err != nil {
			return nil, errors.New("监听端口格式不合法；端口必须是数字")
		}
		ports = append(ports, SoftwareInventoryFormPort{
			Address: strings.TrimSpace(fields[0]), Port: port,
			Protocol: strings.TrimSpace(fields[2]), Exposure: strings.TrimSpace(fields[3]),
		})
	}
	return ports, nil
}

func (h *handler) renderDeclarationForm(
	w http.ResponseWriter, r *http.Request, status int, title string, form SoftwareInventoryDeclarationForm,
) {
	h.renderPage(w, r, status, "softwareinventory-declaration-form.tmpl", pageData{
		Title: title, SoftwareInventoryDeclarationForm: &form,
	})
}

func (h *handler) renderDeclarationWriteError(
	w http.ResponseWriter, r *http.Request, title string,
	form SoftwareInventoryDeclarationForm, err error,
) {
	if message, ok := softwareInventoryFormMessage(err); ok {
		form.Error = message
		h.renderDeclarationForm(w, r, http.StatusBadRequest, title, form)
		return
	}
	h.renderError(w, r, http.StatusInternalServerError, title+"失败", title+"失败，请稍后重试。")
}

func (h *handler) renderDeclarationReadError(w http.ResponseWriter, r *http.Request, err error) {
	var missing *SoftwareInventoryDeclarationNotFoundError
	if errors.As(err, &missing) {
		h.renderError(w, r, http.StatusNotFound, "软件声明不存在", missing.Error())
		return
	}
	h.renderError(w, r, http.StatusInternalServerError, "读取软件声明失败", "读取软件声明失败，请稍后重试。")
}

func softwareInventoryFormMessage(err error) (string, bool) {
	var formErr *SoftwareInventoryFormError
	if errors.As(err, &formErr) {
		return formErr.Message, true
	}
	return "", false
}
