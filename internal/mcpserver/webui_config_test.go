package mcpserver

import "testing"

func TestReadWebUIConfigUsesDefaultsAndSettings(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()

	port, manual, err := ReadWebUIConfig(st)
	if err != nil {
		t.Fatal(err)
	}
	if port != defaultWebUIPort || manual != "" {
		t.Fatalf("缺省 Web UI 配置 = %d/%q，应为 7879 与空串", port, manual)
	}
	if err := st.SetSetting(settingWebUIPort, "7880"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(settingWebUIListen, "100.64.10.5,100.64.10.6"); err != nil {
		t.Fatal(err)
	}
	port, manual, err = ReadWebUIConfig(st)
	if err != nil {
		t.Fatal(err)
	}
	if port != 7880 || manual != "100.64.10.5,100.64.10.6" {
		t.Fatalf("设置后的 Web UI 配置 = %d/%q", port, manual)
	}
	if err := st.SetSetting(settingWebUIPort, "70000"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadWebUIConfig(st); err == nil {
		t.Fatal("serve.ui_port 超出端口范围应报错")
	}
}
