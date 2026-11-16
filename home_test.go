package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestHomeService(t *testing.T) *service {
	t.Helper()
	s := newTestService(t)
	path := filepath.Join(t.TempDir(), "home.json")
	if err := os.WriteFile(path, []byte(`{"retentionDays":30}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg = config{HomeConfig: path, ListenAddr: "127.0.0.1:0"}
	h, err := newHomeMonitor(s.db, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.home = h
	return s
}

func TestHomeRestartOnlyRecordsNewBytes(t *testing.T) {
	s := newTestHomeService(t)
	now := time.Unix(1800000000, 0)
	s.now = func() time.Time { return now }
	payload := &connectionsResponse{UploadTotal: 100000, DownloadTotal: 200000,
		Connections: []connection{testConnection("long-lived", []string{"direct"}, 10000, 20000)}}
	if err := s.processConnections(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	initial, err := s.home.summary(now.Add(-time.Minute).UnixMilli(), now.Add(time.Minute).UnixMilli(), "")
	if err != nil || initial.Totals.Total != 0 || initial.Attributed.Total != 0 {
		t.Fatalf("startup replayed historical counters: %+v, %v", initial, err)
	}
	now = now.Add(5 * time.Second)
	payload.UploadTotal += 50
	payload.DownloadTotal += 100
	payload.Connections[0].Upload += 50
	payload.Connections[0].Download += 100
	if err := s.processConnections(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	restarted := newTestServiceOnDB(s.db)
	restarted.cfg = s.cfg
	restarted.home, err = newHomeMonitor(s.db, s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = func() time.Time { return now }
	if err := restarted.processConnections(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Second)
	payload.UploadTotal += 50
	payload.DownloadTotal += 100
	payload.Connections[0].Upload += 50
	payload.Connections[0].Download += 100
	if err := restarted.processConnections(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	got, err := restarted.home.summary(now.Add(-time.Minute).UnixMilli(), now.Add(time.Minute).UnixMilli(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals.Upload != 100 || got.Totals.Download != 200 || got.Attributed.Total != 300 {
		t.Fatalf("restart changed observed totals: %+v", got)
	}
}

func TestHomeUnsampledShortConnectionsRemainInTotals(t *testing.T) {
	s := newTestHomeService(t)
	now := time.Unix(1800000000, 0)
	s.now = func() time.Time { return now }
	if err := s.processHomeConnections(&connectionsResponse{UploadTotal: 1000, DownloadTotal: 2000}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Second)
	if err := s.processHomeConnections(&connectionsResponse{UploadTotal: 1030, DownloadTotal: 2090}); err != nil {
		t.Fatal(err)
	}
	got, err := s.home.summary(now.Add(-time.Minute).UnixMilli(), now.Add(time.Minute).UnixMilli(), "")
	if err != nil || got.Totals.Total != 120 || got.Attributed.Total != 0 || got.Unattributed.Total != 120 {
		t.Fatalf("lost short-connection bytes: %+v, %v", got, err)
	}
}

func TestHomeCoreCounterResetPreservesHistory(t *testing.T) {
	s := newTestHomeService(t)
	now := time.Unix(1800000000, 0)
	s.now = func() time.Time { return now }
	for _, frame := range []connectionsResponse{
		{UploadTotal: 1000, DownloadTotal: 2000},
		{UploadTotal: 1100, DownloadTotal: 2200},
		{UploadTotal: 10, DownloadTotal: 20},
		{UploadTotal: 60, DownloadTotal: 120},
	} {
		if err := s.processHomeConnections(&frame); err != nil {
			t.Fatal(err)
		}
		now = now.Add(5 * time.Second)
	}
	got, err := s.home.summary(now.Add(-time.Minute).UnixMilli(), now.Add(time.Minute).UnixMilli(), "")
	if err != nil || got.Totals.Upload != 150 || got.Totals.Download != 300 || got.Resets != 1 {
		t.Fatalf("reset changed history: %+v, %v", got, err)
	}
}

func TestHomeRetentionKeepsRecentHistory(t *testing.T) {
	s := newTestHomeService(t)
	now := time.Unix(1800000000, 0)
	s.now = func() time.Time { return now }
	old, recent := now.Add(-31*24*time.Hour).UnixMilli(), now.Add(-24*time.Hour).UnixMilli()
	for _, timestamp := range []int64{old, recent} {
		if _, err := s.db.Exec(`INSERT INTO home_totals(bucket_start,upload,download) VALUES (?,10,20)`, timestamp); err != nil {
			t.Fatal(err)
		}
		insertTestAggregates(t, s.db, []aggregatedEntry{{BucketStart: timestamp, BucketEnd: timestamp + 60000, SourceIP: "192.168.1.2", Host: "example.com", Chains: `[]`, Upload: 10, Download: 20, Count: 1}})
	}
	if err := s.processHomeConnections(&connectionsResponse{UploadTotal: 100, DownloadTotal: 200}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Second)
	if err := s.processHomeConnections(&connectionsResponse{UploadTotal: 101, DownloadTotal: 202}); err != nil {
		t.Fatal(err)
	}
	var count, total int64
	if err := s.db.QueryRow(`SELECT COUNT(*), SUM(upload+download) FROM home_totals`).Scan(&count, &total); err != nil {
		t.Fatal(err)
	}
	if count != 2 || total != 33 {
		t.Fatalf("retention lost recent totals or kept expired data: count=%d total=%d", count, total)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM traffic_aggregated WHERE bucket_start=?`, recent).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recent detail history disappeared: %d %v", count, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM traffic_aggregated WHERE bucket_start=?`, old).Scan(&count); err != nil || count != 0 {
		t.Fatalf("expired detail history was retained: %d %v", count, err)
	}
}

func TestHomeMergesIPv4IPv6AndPreservesReassignedIPHistory(t *testing.T) {
	s := newTestHomeService(t)
	base := int64(1800000000000)
	first := homeInventory{ObservedAt: base, Hostname: "test-router", Leases: "0 aa:bb:cc:dd:ee:01 192.168.1.2 apple-tv *\n",
		Neighbors: "fd00::2 dev br-lan lladdr aa:bb:cc:dd:ee:01 REACHABLE\n"}
	if err := s.home.storeInventory(first); err != nil {
		t.Fatal(err)
	}
	second := homeInventory{ObservedAt: base + 120000, Hostname: "test-router", Leases: "0 aa:bb:cc:dd:ee:02 192.168.1.2 phone *\n"}
	if err := s.home.storeInventory(second); err != nil {
		t.Fatal(err)
	}
	insertTestAggregates(t, s.db, []aggregatedEntry{
		{BucketStart: base, BucketEnd: base + 60000, SourceIP: "192.168.1.2", Host: "www.youtube.com", Process: "", Outbound: "direct", Chains: `[]`, Upload: 10, Download: 20, Count: 1},
		{BucketStart: base, BucketEnd: base + 60000, SourceIP: "fd00::2", Host: "www.youtube.com", Process: "", Outbound: "direct", Chains: `[]`, Upload: 30, Download: 40, Count: 1},
		{BucketStart: base + 120000, BucketEnd: base + 180000, SourceIP: "192.168.1.2", Host: "www.youtube.com", Process: "", Outbound: "direct", Chains: `[]`, Upload: 100, Download: 200, Count: 1},
	})
	got, err := s.home.summary(base, base+180000, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]homeDevice{}
	for _, d := range got.Devices {
		byID[d.ID] = d
	}
	if byID["aa:bb:cc:dd:ee:01"].Total != 100 || byID["aa:bb:cc:dd:ee:02"].Total != 300 {
		t.Fatalf("address history was misattributed: %+v", got.Devices)
	}
	filtered, err := s.home.summary(base, base+180000, "aa:bb:cc:dd:ee:01")
	if err != nil || filtered.Totals.Total != 100 || len(filtered.Applications) != 1 || filtered.Applications[0].ID != "youtube" {
		t.Fatalf("device drilldown incorrect: %+v, %v", filtered, err)
	}
}

func TestHomeApplicationDomainBoundariesAndPrivateOverrides(t *testing.T) {
	s := newTestHomeService(t)
	if err := s.home.addApplication(homeApplicationSpec{ID: "private-emby", Name: "Private Emby", Domains: []string{"emby.example.com"}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ host, rule, want string }{
		{"www.youtube.com", "", "youtube"},
		{"r1.googlevideo.com", "", "youtube"},
		{"youtube.com.evil.test", "", "unknown"},
		{"notyoutube.com", "", "unknown"},
		{"emby.example.com", "", "private-emby"},
		{"emby.example.com.evil.test", "", "unknown"},
		{"149.154.167.51", "rule_set=GitHub-telegram => route(proxy)", "telegram"},
		{"unrelated.example", "rule_set=GitHub-telegram-unknown => route(proxy)", "unknown"},
	} {
		if got := s.home.classify(tc.host, tc.rule); got.ID != tc.want {
			t.Errorf("classify(%q,%q)=%q, want %q", tc.host, tc.rule, got.ID, tc.want)
		}
	}
}

func TestHomeRecognizesPointToPointRouterAddress(t *testing.T) {
	s := newTestHomeService(t)
	base := int64(1800000000000)
	if err := s.home.storeInventory(homeInventory{ObservedAt: base, RouterAddresses: "13: pppoe-wan inet 100.64.12.34 peer 100.64.0.1/32 scope global pppoe-wan"}); err != nil {
		t.Fatal(err)
	}
	insertTestAggregates(t, s.db, []aggregatedEntry{{BucketStart: base, BucketEnd: base + 60000, SourceIP: "100.64.12.34", Host: "example.com", Chains: `[]`, Upload: 100, Download: 200, Count: 1}})
	got, err := s.home.summary(base, base+60000, "router")
	if err != nil || got.Totals.Total != 300 {
		t.Fatalf("point-to-point router traffic was not recognized: %+v, %v", got.Totals, err)
	}
}

func TestHomeRoutesRequireLoginAndExposeNoProxyControls(t *testing.T) {
	s := newTestHomeService(t)
	s.home.accessToken = "test-only-token"
	handler := s.routes()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/home/summary?start=1000&end=2000", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated statistics returned %d", response.Code)
	}
	for _, token := range []string{"wrong", "test-only-token"} {
		form := url.Values{"token": {token}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if token == "wrong" && rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password accepted: %d", rec.Code)
		}
		if token != "wrong" {
			if rec.Code != http.StatusSeeOther || len(rec.Result().Cookies()) != 1 || !rec.Result().Cookies()[0].HttpOnly {
				t.Fatalf("valid login did not establish a session: %d", rec.Code)
			}
		}
	}
	cookie := &http.Cookie{Name: "home_session", Value: s.home.cookieValue(strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/api/home/summary?start=1000&end=2000", http.StatusOK},
		{http.MethodGet, "/api/settings/mihomo", http.StatusNotFound},
		{http.MethodPut, "/api/settings/mihomo", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/traffic/logs", http.StatusMethodNotAllowed},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("%s %s returned %d, want %d", tc.method, tc.path, rec.Code, tc.status)
		}
		if strings.Contains(rec.Body.String(), "test-only-token") {
			t.Error("statistics leaked access credentials")
		}
	}
}

func TestHomeRejectsUnauthenticatedLANListener(t *testing.T) {
	s := newTestHomeService(t)
	t.Setenv("TRAFFIC_MONITOR_ACCESS_TOKEN", "")
	cfg := s.cfg
	cfg.ListenAddr = "0.0.0.0:8080"
	if _, err := newHomeMonitor(s.db, cfg); err == nil {
		t.Fatal("LAN listener accepted without authentication")
	}
	cfg.MihomoSecret = "test-only-token"
	if _, err := newHomeMonitor(s.db, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestHomeConfigRejectsInvalidCatalogue(t *testing.T) {
	s := newTestHomeService(t)
	data, _ := json.Marshal(homeConfig{Applications: []homeApplicationSpec{{ID: "invalid", Name: "Invalid", Regex: []string{"["}}}})
	if err := os.WriteFile(s.cfg.HomeConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newHomeMonitor(s.db, s.cfg); err == nil {
		t.Fatal("invalid classifier regexp accepted")
	}
}
