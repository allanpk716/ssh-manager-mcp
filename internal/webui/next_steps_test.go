package webui

import (
	"strings"
	"testing"
)

func TestStandaloneHomeShowsOnlyUnfinishedNextSteps(t *testing.T) {
	tests := []struct {
		name       string
		servers    []ServerRow
		profiles   []AccessProfileSummary
		projects   []AccessProjectSummary
		wantLabels []string
		wantLinks  []string
	}{
		{
			name:       "三项都未完成",
			wantLabels: []string{"录服务器", "建 Profile 并授权", "建项目并取令牌"},
			wantLinks: []string{
				`href="/ui/servers/import"`,
				`href="/ui/access/profiles"`,
				`href="/ui/access/projects"`,
			},
		},
		{
			name:       "已有服务器时只显示后两项",
			servers:    []ServerRow{{ID: "srv-1", Name: "web"}},
			wantLabels: []string{"建 Profile 并授权", "建项目并取令牌"},
			wantLinks: []string{
				`href="/ui/access/profiles"`,
				`href="/ui/access/projects"`,
			},
		},
		{
			name:       "已有服务器与 Profile 时只显示项目",
			servers:    []ServerRow{{ID: "srv-1", Name: "web"}},
			profiles:   []AccessProfileSummary{{ID: "profile-1", Name: "运维"}},
			wantLabels: []string{"建项目并取令牌"},
			wantLinks:  []string{`href="/ui/access/projects"`},
		},
		{
			name:     "三项都完成后不显示卡片",
			servers:  []ServerRow{{ID: "srv-1", Name: "web"}},
			profiles: []AccessProfileSummary{{ID: "profile-1", Name: "运维"}},
			projects: []AccessProjectSummary{{ID: "project-1", Name: "agent"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			access := &nextStepAccessStore{
				profiles: test.profiles,
				projects: test.projects,
			}
			handler, startup, err := New(Config{
				Role:    RoleStandalone,
				Servers: &stubServerSource{rows: test.servers},
				Access:  access,
				Port:    18080,
			})
			if err != nil {
				t.Fatalf("构造单机 Web UI 失败：%v", err)
			}
			session := exchangeStartupToken(t, handler, startup.URL("127.0.0.1:18080"))
			cookie := session.Cookies()[0]
			session.Body.Close()

			resp := doGetWithCookie(t, handler, cookie, "/ui/")
			defer resp.Body.Close()
			body := readResponseBody(t, resp)
			for _, label := range test.wantLabels {
				if !strings.Contains(body, label) {
					t.Fatalf("首页应显示下一步 %q：\n%s", label, body)
				}
			}
			for _, link := range test.wantLinks {
				if !strings.Contains(body, link) {
					t.Fatalf("首页应包含下一步链接 %s：\n%s", link, body)
				}
			}
			if len(test.wantLabels) == 0 && strings.Contains(body, "下一步引导") {
				t.Fatalf("三项都完成后首页不应显示下一步引导卡片：\n%s", body)
			}
		})
	}
}

func TestClientAndAuthorityHomeDoNotShowNextSteps(t *testing.T) {
	client, clientStartup, err := New(Config{
		Role:    RoleClient,
		Servers: &stubServerSource{rows: []ServerRow{{Name: "客户端服务器"}}},
		Port:    18080,
	})
	if err != nil {
		t.Fatalf("构造客户端 Web UI 失败：%v", err)
	}
	clientSession := exchangeStartupToken(t, client, clientStartup.URL("127.0.0.1:18080"))
	clientCookie := clientSession.Cookies()[0]
	clientSession.Body.Close()
	clientHome := readResponseBody(t, doGetWithCookie(t, client, clientCookie, "/ui/"))
	if strings.Contains(clientHome, "下一步引导") {
		t.Fatalf("客户端首页不应显示下一步引导：\n%s", clientHome)
	}

	authority, _, err := NewAuthority(Config{
		Role:         RoleServer,
		Servers:      &stubServerSource{rows: []ServerRow{{Name: "权威端服务器"}}},
		Access:       &stubAccessStore{},
		Auth:         &fakeAuthorityAuth{passwordSet: true, password: "correct-password"},
		AllowedHosts: []string{"127.0.0.1"},
		Port:         7879,
	})
	if err != nil {
		t.Fatalf("构造权威端 Web UI 失败：%v", err)
	}
	authorityCookie := authoritySuccessfulLogin(t, authority, "127.0.0.1:7879", "correct-password")
	authorityHome := readResponseBody(t, authorityGet(t, authority, "127.0.0.1:7879", "/ui/", authorityCookie))
	if strings.Contains(authorityHome, "下一步引导") {
		t.Fatalf("权威端首页不应显示下一步引导：\n%s", authorityHome)
	}
}

type nextStepAccessStore struct {
	*stubAccessStore
	profiles []AccessProfileSummary
	projects []AccessProjectSummary
}

func (s *nextStepAccessStore) ListProfiles() ([]AccessProfileSummary, error) {
	return s.profiles, nil
}

func (s *nextStepAccessStore) ListProjects(bool) ([]AccessProjectSummary, error) {
	return s.projects, nil
}
