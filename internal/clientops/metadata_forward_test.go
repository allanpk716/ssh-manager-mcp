package clientops

// Plan 51 T6 (spec §9): the metadata forwarder's branch table — every §2.1
// text is a verbatim contract, asserted byte-for-byte (forward_test.go's
// pattern, including the pinned-TLS construction and the redirect posture).

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
)

// newTestMetaForwarder builds a capable forwarder against srv (TLS test
// server), same construction as newTestForwarder.
func newTestMetaForwarder(t *testing.T, srv *httptest.Server) *MetadataForwarder {
	t.Helper()
	pin := mcpserver.SPKIFingerprint(srv.Certificate())
	f, err := NewMetadataForwarder(CacheCred{URL: srv.URL, Token: "devcode-1:" + pin, Pin: pin})
	if err != nil {
		t.Fatalf("NewMetadataForwarder: %v", err)
	}
	return f
}

func metaEdit(v string) *string { return &v }

// assertMetaPost checks the wire shape of one edit POST (§1.1 request).
func assertMetaPost(t *testing.T, r *http.Request, serverID string) {
	t.Helper()
	if r.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.Method)
	}
	if r.URL.Path != "/server-metadata" {
		t.Errorf("path = %q, want /server-metadata", r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer devcode-1" {
		t.Errorf("Authorization = %q, want the bare code after SplitTokenPin strip", got)
	}
	var in metaEditRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		t.Errorf("body decode: %v", err)
		return
	}
	if in.ServerID != serverID || in.ExpectedRevision != 3 {
		t.Errorf("body = %+v, want server %s @rev 3", in, serverID)
	}
	if len(in.Fields) != 1 || in.Fields["hardware"] == nil || *in.Fields["hardware"] != "H200" {
		t.Errorf("fields = %+v, want only hardware=H200", in.Fields)
	}
}

func TestMetadataForwarder_BranchTexts(t *testing.T) {
	const sid = "srv-1"
	cases := []struct {
		name     string
		status   int
		body     string
		want     string // exact Error() text; "" = want nil error
		wantStal bool   // errors.As *MetaStaleError
	}{
		{"200", http.StatusOK, `{"server_name":"gpu","revision":4,"updated_at":1760000000}`, "", false},
		{"401", http.StatusUnauthorized, "Unauthorized", forwardMsg401, false},
		{"403 grant", http.StatusForbidden, "refused for this entry", metaMsg403(sid), false},
		{"403 disabled", http.StatusForbidden, "metadata editing is disabled by the owner", metaMsgDisabled, false},
		{"404 old broker", http.StatusNotFound, "404 page not found", metaMsg404, false},
		{"409 stale", http.StatusConflict,
			`{"error":"stale revision","current_revision":4,"current":{"role":"r","services":"","location":"","hardware":"H100","caveats":"c","description":""}}`,
			metaMsg409(sid, 4, map[string]string{"role": "r", "services": "", "location": "", "hardware": "H100", "caveats": "c", "description": ""}), true},
		{"409 unparsable = fail closed", http.StatusConflict, `not-json`,
			"stale revision for srv-1 — the entry changed since your listing; re-run list_servers and retry", false},
		{"400 passthrough json", http.StatusBadRequest, `{"error":"unknown metadata field \"tags\""}`,
			"metadata edit for srv-1: broker returned 400 — unknown metadata field \"tags\"", false},
		{"413 passthrough plain", http.StatusRequestEntityTooLarge, "request body too large",
			"metadata edit for srv-1: broker returned 413 — request body too large", false},
		{"500", http.StatusInternalServerError, "boom", metaMainMessage(sid), false},
		{"503 empty", http.StatusServiceUnavailable, "", metaMainMessage(sid), false},
		{"302 unfollowed", http.StatusFound, "", metaMainMessage(sid), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertMetaPost(t, r, sid)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			res, err := newTestMetaForwarder(t, srv).Edit(sid, 3, map[string]*string{"hardware": metaEdit("H200")})
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Edit: want nil, got %v", err)
				}
				if res.ServerName != "gpu" || res.Revision != 4 || res.UpdatedAt != 1760000000 {
					t.Fatalf("200 result = %+v", res)
				}
				return
			}
			if err == nil {
				t.Fatal("Edit: want error, got nil")
			}
			if err.Error() != tc.want {
				t.Fatalf("branch text mismatch (verbatim contract):\n got: %q\nwant: %q", err.Error(), tc.want)
			}
			var stale *MetaStaleError
			if got := errors.As(err, &stale); got != tc.wantStal {
				t.Fatalf("errors.As(*MetaStaleError) = %v, want %v", got, tc.wantStal)
			}
		})
	}
}

func TestMetadataForwarder_EditMeta_Adapter(t *testing.T) {
	// The EditMeta adapter satisfies mcpserver.MetadataEditor and maps the
	// outcome type one-for-one (the import-cycle-safe injection seam).
	var _ mcpserver.MetadataEditor = (*MetadataForwarder)(nil)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"server_name":"gpu","revision":2,"updated_at":99}`))
	}))
	defer srv.Close()
	out, err := newTestMetaForwarder(t, srv).EditMeta("srv-1", 1, map[string]*string{"role": metaEdit("x")})
	if err != nil {
		t.Fatal(err)
	}
	if out != (mcpserver.MetadataEditOutcome{ServerName: "gpu", Revision: 2, UpdatedAt: 99}) {
		t.Fatalf("EditMeta outcome = %+v", out)
	}
}

func TestMetadataForwarder_PlaintextRefused(t *testing.T) {
	f, err := NewMetadataForwarder(CacheCred{URL: "http://plain.example", Token: "code"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Edit("srv-1", 0, map[string]*string{"role": metaEdit("x")})
	if err == nil || err.Error() != metaMsgPlaintext {
		t.Fatalf("plaintext: %v", err)
	}
	// A pin + non-https URL is a hard misconfiguration (construction error).
	if _, err := NewMetadataForwarder(CacheCred{URL: "http://plain.example", Token: "code", Pin: "sha256/abc"}); err == nil {
		t.Fatal("pin + http URL must refuse construction")
	}
}

func TestMetadataForwarder_NoCredential(t *testing.T) {
	// The deleted-cache.auth.json flavor: no capability, fail closed with the
	// main message — never a silent local write.
	f, err := NewMetadataForwarder(CacheCred{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.EditMeta("srv-1", 0, map[string]*string{"role": metaEdit("x")})
	if err == nil || err.Error() != metaMainMessage("srv-1") {
		t.Fatalf("no-cred: %v", err)
	}
}

func TestMetadataForwarder_Unreachable_MainMessage(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()                                 // port now refuses
	pin := "sha256:" + strings.Repeat("ab", 32) // valid pin FORMAT; never dialed
	f, err := NewMetadataForwarder(CacheCred{URL: url, Token: "code", Pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Edit("srv-1", 0, map[string]*string{"role": metaEdit("x")})
	if err == nil || err.Error() != metaMainMessage("srv-1") {
		t.Fatalf("unreachable: %v", err)
	}
	if !strings.Contains(err.Error(), "NOT applied anywhere") {
		t.Fatalf("main message must state the nothing-applied guarantee: %v", err)
	}
}

func TestMetadataForwarder_Timeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // hang past the 30s cap... made instant by the short client below
	}))
	defer srv.Close()
	defer close(release)
	pin := mcpserver.SPKIFingerprint(srv.Certificate())
	f, err := NewMetadataForwarder(CacheCred{URL: srv.URL, Token: "c:" + pin, Pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	// Shrink only the HTTP timeout so the test is fast; the branch is the same
	// (client.Do error → main message).
	f.client.Timeout = 100 * time.Millisecond
	start := time.Now()
	_, err = f.Edit("srv-1", 0, map[string]*string{"role": metaEdit("x")})
	if err == nil || err.Error() != metaMainMessage("srv-1") {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout branch took %s — the cap did not bound the call", time.Since(start))
	}
}

func TestMetadataForwarder_RedirectNotFollowed(t *testing.T) {
	var followed bool
	mux := http.NewServeMux()
	mux.HandleFunc("/server-metadata", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, r *http.Request) {
		followed = true
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()

	_, err := newTestMetaForwarder(t, srv).Edit("srv-1", 0, map[string]*string{"role": metaEdit("x")})
	if followed {
		t.Fatal("the 302 was followed — redirects must never be followed")
	}
	if err == nil || err.Error() != metaMainMessage("srv-1") {
		t.Fatalf("unfollowed redirect must land on the main message: %v", err)
	}
}
