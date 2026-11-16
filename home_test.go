package main

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestHomeDomainClassificationKeepsSeparateUsage(t *testing.T) {
	s := newTestHomeService(t)
	base := int64(1800000000000)
	insertTestAggregates(t, s.db, []aggregatedEntry{
		{BucketStart: base, BucketEnd: base + 60000, SourceIP: "192.0.2.2", Host: "unlisted.example", RuleGroup: "final", Chains: `[]`, Download: 10, Count: 1},
		{BucketStart: base + 60000, BucketEnd: base + 120000, SourceIP: "192.0.2.2", Host: "unlisted.example", RuleGroup: "rule_set=GitHub-telegram => route(proxy)", Chains: `[]`, Download: 90, Count: 1},
	})
	got, err := s.home.summary(base, base+120000, "")
	if err != nil || len(got.Domains) != 2 {
		t.Fatalf("different classifications were combined: %+v, %v", got.Domains, err)
	}
	for _, app := range got.Applications {
		var total int64
		for _, domain := range got.Domains {
			if domain.Application == app.Name {
				total += domain.Total
			}
		}
		if total != app.Total {
			t.Errorf("%s: domains=%d application=%d", app.Name, total, app.Total)
		}
	}
}

func TestHomeApplicationFilterPrecedesDomainPagination(t *testing.T) {
	s := newTestHomeService(t)
	base := int64(1800000000000)
	entries := make([]aggregatedEntry, 0, 202)
	for i := 0; i < 201; i++ {
		entries = append(entries, aggregatedEntry{BucketStart: base, BucketEnd: base + 60000, SourceIP: "192.0.2.2", Host: fmt.Sprintf("%03d.example", i), Chains: `[]`, Download: 1000, Count: 1})
	}
	entries = append(entries, aggregatedEntry{BucketStart: base, BucketEnd: base + 60000, SourceIP: "192.0.2.2", Host: "api.openai.com", Chains: `[]`, Download: 1, Count: 1})
	insertTestAggregates(t, s.db, entries)
	s.home.accessToken = ""
	for _, tc := range []struct {
		query       string
		count, rows int
		first       string
	}{
		{"application=openai&limit=50", 1, 1, "api.openai.com"},
		{"offset=200&limit=50", 202, 2, "200.example"},
		{"application=missing&limit=50", 0, 0, ""},
	} {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/home/summary?start=%d&end=%d&%s", base, base+60000, tc.query), nil))
		var got struct {
			Domains     []homeDomain `json:"domains"`
			DomainCount int          `json:"domainCount"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || got.DomainCount != tc.count || len(got.Domains) != tc.rows {
			t.Fatalf("%s: status=%d count=%d rows=%d (%v)", tc.query, rec.Code, got.DomainCount, len(got.Domains), err)
		}
		if tc.first != "" && got.Domains[0].Host != tc.first {
			t.Errorf("%s: first domain=%s", tc.query, got.Domains[0].Host)
		}
	}
}

func TestHomeExplicitAddressCorrectionsPreserveObservedOwners(t *testing.T) {
	s := newTestHomeService(t)
	base := int64(1800000000000)
	first, second := "aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"
	s.home.config.DeviceNames = map[string]string{first: "Named device"}
	s.home.config.AddressBindings = []homeAddress{{Address: "2001:db8::2", Device: first, From: base, To: base + 120000}}
	if err := s.home.storeInventory(homeInventory{ObservedAt: base + 60000, Neighbors: "2001:db8::2 dev br-lan lladdr " + second + " REACHABLE"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		ts := base + int64(i)*60000
		insertTestAggregates(t, s.db, []aggregatedEntry{{BucketStart: ts, BucketEnd: ts + 60000, SourceIP: "2001:db8::2", Host: "example.com", Chains: `[]`, Download: 100, Count: 1}})
	}
	got, err := s.home.summary(base, base+180000, "")
	if err != nil {
		t.Fatal(err)
	}
	usage := map[string]int64{}
	for _, d := range got.Devices {
		usage[d.ID] = d.Total
		if d.ID == first && (d.Name != "Named device" || d.MAC != first) {
			t.Fatalf("device annotation missing: %+v", d)
		}
	}
	if usage[first] != 100 || usage[second] != 200 {
		t.Fatalf("observed ownership was overwritten: %+v", usage)
	}
}

func TestHomeCatalogueRecognizesServicesAndCIDRBoundaries(t *testing.T) {
	s := newTestHomeService(t)
	for _, tc := range []struct{ host, want string }{
		{"91.108.56.185", "telegram"}, {"149.154.175.57", "telegram"}, {"2001:b28:f23d::1", "telegram"},
		{"149.154.159.255", "unknown"}, {"api.deepseek.com", "deepseek"},
		{"v1.fqnovelvod.com", "fqnovel"}, {"dns.alidns.com", "encrypted-dns"},
		{"img.cloud.ottcn.com", "cctv"}, {"bucket.oss-accelerate.aliyuncs.com", "cloud-storage"},
		{"bucket.oss-accelerate.aliyuncs.com.evil.test", "unknown"},
	} {
		if got := s.home.classify(tc.host, "final"); got.ID != tc.want {
			t.Errorf("%s: %s, want %s", tc.host, got.ID, tc.want)
		}
	}
	if got := s.home.classify("91.108.56.185", "rule_set=GitHub-youtube => route(proxy)"); got.ID != "youtube" {
		t.Fatalf("CIDR overrode observed rule: %+v", got)
	}
}

func TestHomeRejectsInvalidDomainPagination(t *testing.T) {
	s := newTestHomeService(t)
	for _, query := range []string{"offset=-1", "offset=no", "limit=0", "limit=201", "limit=no", "offset=999999999999999999999999"} {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/home/summary?start=1000&end=2000&"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s returned %d", query, rec.Code)
		}
	}
}

func TestHomeRejectsInvalidAddressAndIPRules(t *testing.T) {
	for _, body := range []string{
		`{"deviceNames":{"not-a-mac":"Name"}}`,
		`{"addressBindings":[{"address":"2001:db8::2","device":"aa:bb:cc:dd:ee:01","from":1000,"to":0}]}`,
		`{"addressBindings":[{"address":"2001:db8::2","device":"aa:bb:cc:dd:ee:01","from":1000,"to":2000},{"address":"2001:db8::2","device":"aa:bb:cc:dd:ee:02","from":1500,"to":2500}]}`,
		`{"applications":[{"id":"custom","name":"Custom","ipCIDRs":["bad-prefix"]}]}`,
	} {
		s := newTestHomeService(t)
		if err := os.WriteFile(s.cfg.HomeConfig, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := newHomeMonitor(s.db, s.cfg); err == nil {
			t.Errorf("invalid config accepted: %s", body)
		}
	}
}
