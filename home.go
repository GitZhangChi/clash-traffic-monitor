package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed home-applications.json
var homeApplicationsJSON []byte

type homeConfig struct {
	RouterSSH        string                `json:"routerSSH"`
	ExpectedHostname string                `json:"expectedHostname"`
	InventoryFile    string                `json:"inventoryFile"`
	RetentionDays    int                   `json:"retentionDays"`
	Applications     []homeApplicationSpec `json:"applications"`
	DeviceNames      map[string]string     `json:"deviceNames,omitempty"`
	AddressBindings  []homeAddress         `json:"addressBindings,omitempty"`
}

type homeApplicationSpec struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Domains     []string `json:"domains,omitempty"`
	Suffixes    []string `json:"suffixes,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	Regex       []string `json:"regex,omitempty"`
	RuleSets    []string `json:"ruleSets,omitempty"`
	Tunnel      bool     `json:"tunnel,omitempty"`
	Description string   `json:"description,omitempty"`
	IPCIDRs     []string `json:"ipCIDRs,omitempty"`
}

type homeApp struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Tunnel      bool   `json:"tunnel,omitempty"`
	Description string `json:"description,omitempty"`
}

type homeIPRule struct {
	prefix netip.Prefix
	app    homeApp
}

type homePattern struct {
	app     homeApp
	keyword string
	regex   *regexp.Regexp
}

type homeMonitor struct {
	mu              sync.RWMutex
	db              *sql.DB
	config          homeConfig
	accessToken     string
	exact           map[string]homeApp
	suffix          map[string]homeApp
	rules           map[string]homeApp
	patterns        []homePattern
	ipRules         []homeIPRule
	ready           bool
	connections     map[string]connection
	previousUp      int64
	previousDown    int64
	lastSample      int64
	lastInventory   int64
	lastCleanup     int64
	collectionError string
	inventoryError  string
	uploadRate      float64
	downloadRate    float64
	resets          int
}

type homeInventory struct {
	Hostname        string `json:"hostname"`
	ObservedAt      int64  `json:"observedAt"`
	Leases          string `json:"leases"`
	Neighbors       string `json:"neighbors"`
	RouterAddresses string `json:"routerAddresses"`
}

type homeAddress struct {
	Address string `json:"address"`
	Device  string `json:"device"`
	From    int64  `json:"from"`
	To      int64  `json:"to"`
}

type homeUsage struct {
	Upload   int64 `json:"upload"`
	Download int64 `json:"download"`
	Total    int64 `json:"total"`
}

func (u *homeUsage) add(up, down int64) {
	u.Upload += up
	u.Download += down
	u.Total += up + down
}

type homeAppUsage struct {
	homeApp
	homeUsage
}

type homeDevice struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	MAC          string         `json:"mac,omitempty"`
	Addresses    []string       `json:"addresses"`
	LastSeen     int64          `json:"lastSeen"`
	Applications []homeAppUsage `json:"applications"`
	homeUsage
}

type homeDomain struct {
	Host          string `json:"host"`
	Device        string `json:"device"`
	DeviceName    string `json:"deviceName"`
	Application   string `json:"application"`
	ApplicationID string `json:"applicationId"`
	Description   string `json:"description,omitempty"`
	homeUsage
}

type homePoint struct {
	Timestamp int64 `json:"timestamp"`
	homeUsage
}

type homeSummary struct {
	Start           int64          `json:"start"`
	End             int64          `json:"end"`
	GeneratedAt     int64          `json:"generatedAt"`
	CollectedSince  int64          `json:"collectedSince"`
	LastSample      int64          `json:"lastSample"`
	LastInventory   int64          `json:"lastInventory"`
	CollectionError string         `json:"collectionError,omitempty"`
	InventoryError  string         `json:"inventoryError,omitempty"`
	UploadRate      float64        `json:"uploadRate"`
	DownloadRate    float64        `json:"downloadRate"`
	Resets          int            `json:"resets"`
	RetentionDays   int            `json:"retentionDays"`
	Totals          homeUsage      `json:"totals"`
	Attributed      homeUsage      `json:"attributed"`
	Unattributed    homeUsage      `json:"unattributed"`
	Devices         []homeDevice   `json:"devices"`
	Applications    []homeAppUsage `json:"applications"`
	Domains         []homeDomain   `json:"domains"`
	Timeline        []homePoint    `json:"timeline"`
	DomainCount     int            `json:"domainCount"`
	DomainOffset    int            `json:"domainOffset"`
	DomainLimit     int            `json:"domainLimit"`
	DomainUsage     homeUsage      `json:"domainUsage"`
}

type homeDomainQuery struct {
	Application   string
	Offset, Limit int
}

func newHomeMonitor(db *sql.DB, cfg config) (*homeMonitor, error) {
	data, err := os.ReadFile(cfg.HomeConfig)
	if err != nil {
		return nil, err
	}
	var hc homeConfig
	if err := json.Unmarshal(data, &hc); err != nil {
		return nil, err
	}
	if hc.RetentionDays == 0 {
		hc.RetentionDays = 30
	}
	if hc.RetentionDays < 1 || hc.RetentionDays > 365 {
		return nil, errors.New("retentionDays must be between 1 and 365")
	}
	if hc.RouterSSH != "" && (!regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.@-]*$`).MatchString(hc.RouterSSH) || hc.ExpectedHostname == "") {
		return nil, errors.New("routerSSH must be a host alias and expectedHostname must be set")
	}
	if hc.InventoryFile != "" && !filepath.IsAbs(hc.InventoryFile) {
		hc.InventoryFile = filepath.Join(filepath.Dir(cfg.HomeConfig), hc.InventoryFile)
	}
	for key, name := range hc.DeviceNames {
		if mac, err := net.ParseMAC(key); err != nil || mac.String() != key || strings.TrimSpace(name) == "" {
			return nil, errors.New("deviceNames requires canonical MAC addresses and non-empty names")
		}
	}
	for i := range hc.AddressBindings {
		binding := &hc.AddressBindings[i]
		ip := net.ParseIP(binding.Address)
		mac, err := net.ParseMAC(binding.Device)
		if ip == nil || (binding.Device != "router" && (err != nil || mac.String() != binding.Device)) || binding.From <= 0 || binding.To <= binding.From {
			return nil, errors.New("addressBindings requires an IP, canonical device MAC (or router), and a finite from/to interval in Unix milliseconds")
		}
		binding.Address = ip.String()
		for _, previous := range hc.AddressBindings[:i] {
			if previous.Address == binding.Address && previous.From < binding.To && binding.From < previous.To {
				return nil, errors.New("addressBindings intervals for an address must not overlap")
			}
		}
	}
	h := &homeMonitor{
		db: db, config: hc, accessToken: getenv("TRAFFIC_MONITOR_ACCESS_TOKEN", cfg.MihomoSecret),
		exact: map[string]homeApp{}, suffix: map[string]homeApp{}, rules: map[string]homeApp{},
		connections: map[string]connection{},
	}
	listenHost, _, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address: %w", err)
	}
	if h.accessToken == "" && listenHost != "localhost" && !net.ParseIP(listenHost).IsLoopback() {
		return nil, errors.New("LAN access requires TRAFFIC_MONITOR_ACCESS_TOKEN or MIHOMO_SECRET")
	}
	var bundled struct {
		Applications []homeApplicationSpec `json:"applications"`
	}
	if err := json.Unmarshal(homeApplicationsJSON, &bundled); err != nil {
		return nil, err
	}
	// User rules win exact ties; longest matching domain suffix wins otherwise.
	for _, spec := range append(hc.Applications, bundled.Applications...) {
		if err := h.addApplication(spec); err != nil {
			return nil, err
		}
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS home_devices (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, mac TEXT NOT NULL, last_seen INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS home_addresses (
			address TEXT NOT NULL, device_id TEXT NOT NULL, valid_from INTEGER NOT NULL,
			valid_to INTEGER NOT NULL DEFAULT 0, last_seen INTEGER NOT NULL,
			PRIMARY KEY(address, valid_from)
		);
		CREATE INDEX IF NOT EXISTS idx_home_addresses_time ON home_addresses(address, valid_from, valid_to);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_home_addresses_current ON home_addresses(address) WHERE valid_to = 0;
		CREATE TABLE IF NOT EXISTS home_totals (
			bucket_start INTEGER PRIMARY KEY, upload INTEGER NOT NULL, download INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS home_metadata (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
	`)
	return h, err
}

func (h *homeMonitor) addApplication(spec homeApplicationSpec) error {
	if spec.ID == "" || spec.Name == "" {
		return errors.New("application id and name are required")
	}
	app := homeApp{ID: spec.ID, Name: spec.Name, Tunnel: spec.Tunnel, Description: spec.Description}
	for _, cidr := range spec.IPCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return fmt.Errorf("application %s: invalid IP prefix: %w", spec.ID, err)
		}
		h.ipRules = append(h.ipRules, homeIPRule{prefix.Masked(), app})
	}
	for _, domain := range spec.Domains {
		if key := cleanHomeHost(domain); key != "" {
			if _, exists := h.exact[key]; !exists {
				h.exact[key] = app
			}
		}
	}
	for _, domain := range spec.Suffixes {
		if key := cleanHomeHost(domain); key != "" {
			if _, exists := h.suffix[key]; !exists {
				h.suffix[key] = app
			}
		}
	}
	for _, key := range spec.RuleSets {
		key = strings.ToLower(key)
		if _, exists := h.rules[key]; !exists {
			h.rules[key] = app
		}
	}
	for _, keyword := range spec.Keywords {
		h.patterns = append(h.patterns, homePattern{app: app, keyword: strings.ToLower(keyword)})
	}
	for _, pattern := range spec.Regex {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("application %s: %w", spec.ID, err)
		}
		h.patterns = append(h.patterns, homePattern{app: app, regex: re})
	}
	return nil
}

func cleanHomeHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.Trim(strings.ToLower(strings.TrimSpace(host)), ".")
}

func (h *homeMonitor) classify(host, rule string) homeApp {
	host = cleanHomeHost(host)
	if app, ok := h.exact[host]; ok {
		return app
	}
	for suffix := host; suffix != ""; {
		if app, ok := h.suffix[suffix]; ok {
			return app
		}
		index := strings.IndexByte(suffix, '.')
		if index < 0 {
			break
		}
		suffix = suffix[index+1:]
	}
	for _, pattern := range h.patterns {
		if (pattern.regex != nil && pattern.regex.MatchString(host)) || (pattern.keyword != "" && strings.Contains(host, pattern.keyword)) {
			return pattern.app
		}
	}
	for _, token := range strings.FieldsFunc(strings.ToLower(rule), func(r rune) bool {
		return strings.ContainsRune("=[] ,\t", r)
	}) {
		if app, ok := h.rules[token]; ok {
			return app
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		var match *homeIPRule
		for i := range h.ipRules {
			rule := &h.ipRules[i]
			if rule.prefix.Contains(ip.Unmap()) && (match == nil || rule.prefix.Bits() > match.prefix.Bits()) {
				match = rule
			}
		}
		if match != nil {
			return match.app
		}
	}
	return homeApp{ID: "unknown", Name: "未识别服务", Description: "尚未匹配服务规则；裸 IP 或共享基础设施可能无法确定具体 App"}
}

// Home mode never changes proxy settings. A single transaction stores observed
// core totals and sampled connection deltas; a restart only establishes a baseline.
func (s *service) processHomeConnections(payload *connectionsResponse) error {
	h := s.home
	h.mu.Lock()
	defer h.mu.Unlock()
	now := s.currentTime().UnixMilli()
	if !h.ready || payload.UploadTotal < h.previousUp || payload.DownloadTotal < h.previousDown {
		if h.ready {
			h.resets++
		}
		h.ready = true
		h.previousUp, h.previousDown = payload.UploadTotal, payload.DownloadTotal
		h.connections = map[string]connection{}
		for _, conn := range payload.Connections {
			h.connections[conn.ID] = conn
		}
		h.lastSample, h.uploadRate, h.downloadRate, h.collectionError = now, 0, 0, ""
		_, err := h.db.Exec(`INSERT OR IGNORE INTO home_metadata(key, value) VALUES ('collected_since', ?)`, now)
		return err
	}
	up, down := payload.UploadTotal-h.previousUp, payload.DownloadTotal-h.previousDown
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	bucket := now / 60000 * 60000
	if _, err := tx.Exec(`INSERT INTO home_totals(bucket_start, upload, download) VALUES (?, ?, ?)
		ON CONFLICT(bucket_start) DO UPDATE SET upload=upload+excluded.upload, download=download+excluded.download`, bucket, up, down); err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO traffic_aggregated
		(bucket_start,bucket_end,source_ip,host,destination_ip,process,outbound,chains,rule_group,upload,download,count)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,1)
		ON CONFLICT(bucket_start,bucket_end,source_ip,host,destination_ip,process,outbound,chains)
		DO UPDATE SET upload=upload+excluded.upload,download=download+excluded.download,count=count+1`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	current := make(map[string]connection, len(payload.Connections))
	for _, conn := range payload.Connections {
		current[conn.ID] = conn
		prev := h.connections[conn.ID]
		du, dd := conn.Upload-prev.Upload, conn.Download-prev.Download
		if du < 0 || dd < 0 {
			continue // A replaced/reset connection is baselined rather than replayed.
		}
		if du == 0 && dd == 0 {
			continue
		}
		chains, err := json.Marshal(sanitizeChains(conn.Chains))
		if err != nil {
			return err
		}
		if _, err := stmt.Exec(bucket, bucket+60000, defaultString(conn.Metadata.SourceIP, "Inner"),
			defaultString(firstNonEmpty(conn.Metadata.Host, conn.Metadata.DestinationIP), "Unknown"), conn.Metadata.DestinationIP,
			conn.Metadata.Process, outboundName(conn.Chains), string(chains), conn.Rule, du, dd); err != nil {
			return err
		}
	}
	if now-h.lastCleanup > int64(time.Hour/time.Millisecond) {
		cutoff := now - int64(h.config.RetentionDays)*86400000
		for _, query := range []string{
			`DELETE FROM home_totals WHERE bucket_start < ?`,
			`DELETE FROM traffic_aggregated WHERE bucket_end < ?`,
			`DELETE FROM home_addresses WHERE valid_to > 0 AND valid_to < ?`,
		} {
			if _, err := tx.Exec(query, cutoff); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	elapsed := float64(now-h.lastSample) / 1000
	if elapsed > 0 {
		h.uploadRate, h.downloadRate = float64(up)/elapsed, float64(down)/elapsed
	}
	h.previousUp, h.previousDown, h.connections = payload.UploadTotal, payload.DownloadTotal, current
	h.lastSample, h.collectionError = now, ""
	if now-h.lastCleanup > int64(time.Hour/time.Millisecond) {
		h.lastCleanup = now
	}
	return nil
}

func (h *homeMonitor) setCollectionError(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.collectionError = "暂时无法读取网关流量"
	log.Printf("home collection: %v", err)
}

const homeInventoryScript = `import json,os,subprocess,time
def run(args):
 return subprocess.check_output(args,text=True,timeout=5)
with open('/tmp/dhcp.leases') as f:
 leases=f.read()
print(json.dumps({'hostname':os.uname().nodename,'observedAt':int(time.time()*1000),'leases':leases,'neighbors':run(['ip','neigh','show'])+'\n'+run(['ip','-6','neigh','show']),'routerAddresses':run(['ip','-o','addr','show'])}))`

func (h *homeMonitor) refreshInventory(ctx context.Context) {
	if h.config.RouterSSH == "" && h.config.InventoryFile == "" {
		return
	}
	var data []byte
	var err error
	if h.config.RouterSSH != "" {
		commandCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		remote := "python3 -c '" + strings.ReplaceAll(homeInventoryScript, "'", "'\"'\"'") + "'"
		data, err = exec.CommandContext(commandCtx, "/usr/bin/ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1", h.config.RouterSSH, remote).Output()
	} else {
		data, err = os.ReadFile(h.config.InventoryFile)
	}
	if err == nil {
		var inventory homeInventory
		err = json.Unmarshal(data, &inventory)
		if err == nil && h.config.ExpectedHostname != "" && inventory.Hostname != h.config.ExpectedHostname {
			err = fmt.Errorf("router hostname mismatch: got %q", inventory.Hostname)
		}
		if err == nil {
			err = h.storeInventory(inventory)
		}
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("home inventory: %v", err)
		h.mu.Lock()
		h.inventoryError = "设备地址暂未更新，现有记录仍保留"
		h.mu.Unlock()
	}
}

func (h *homeMonitor) runInventory(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.refreshInventory(ctx)
		}
	}
}

func (h *homeMonitor) storeInventory(inventory homeInventory) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if inventory.ObservedAt <= 0 {
		return errors.New("inventory timestamp is required")
	}
	type device struct{ name, mac string }
	devices := map[string]device{"router": {"MT6000 路由器", ""}}
	addresses := map[string]string{}
	for _, line := range strings.Split(inventory.Leases, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || net.ParseIP(fields[2]) == nil {
			continue
		}
		mac, err := net.ParseMAC(fields[1])
		if err != nil {
			continue
		}
		id, name := mac.String(), fields[3]
		if name == "*" {
			name = ""
		}
		devices[id] = device{name, id}
		addresses[net.ParseIP(fields[2]).String()] = id
	}
	for _, line := range strings.Split(inventory.Neighbors, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || net.ParseIP(fields[0]) == nil {
			continue
		}
		for index, field := range fields {
			if field != "lladdr" || index+1 >= len(fields) {
				continue
			}
			if mac, err := net.ParseMAC(fields[index+1]); err == nil {
				id := mac.String()
				addresses[net.ParseIP(fields[0]).String()] = id
				if _, exists := devices[id]; !exists {
					devices[id] = device{"", id}
				}
			}
			break
		}
	}
	for _, line := range strings.Split(inventory.RouterAddresses, "\n") {
		fields := strings.Fields(line)
		for index, field := range fields {
			if (field == "inet" || field == "inet6") && index+1 < len(fields) {
				if ip, _, err := net.ParseCIDR(fields[index+1]); err == nil {
					addresses[ip.String()] = "router"
				} else if ip := net.ParseIP(fields[index+1]); ip != nil {
					// Point-to-point interfaces print the local address without a prefix.
					addresses[ip.String()] = "router"
				}
			}
		}
	}
	tx, err := h.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, d := range devices {
		if _, err := tx.Exec(`INSERT INTO home_devices(id,name,mac,last_seen) VALUES (?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET name=CASE WHEN excluded.name='' THEN home_devices.name ELSE excluded.name END,
			last_seen=MAX(home_devices.last_seen,excluded.last_seen)`, id, d.name, d.mac, inventory.ObservedAt); err != nil {
			return err
		}
	}
	for address, id := range addresses {
		var existing string
		var from int64
		err := tx.QueryRow(`SELECT device_id,valid_from FROM home_addresses WHERE address=? AND valid_to=0`, address).Scan(&existing, &from)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if existing == id {
			if _, err := tx.Exec(`UPDATE home_addresses SET last_seen=MAX(last_seen,?) WHERE address=? AND valid_to=0`, inventory.ObservedAt, address); err != nil {
				return err
			}
			continue
		}
		if inventory.ObservedAt < from {
			return errors.New("out-of-order inventory cannot replace an address owner")
		}
		if _, err := tx.Exec(`UPDATE home_addresses SET valid_to=? WHERE address=? AND valid_to=0`, inventory.ObservedAt, address); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO home_addresses(address,device_id,valid_from,last_seen) VALUES (?,?,?,?)`, address, id, inventory.ObservedAt, inventory.ObservedAt); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	h.lastInventory, h.inventoryError = inventory.ObservedAt, ""
	return nil
}

func (h *homeMonitor) summary(start, end int64, selected string) (homeSummary, error) {
	return h.summaryPage(start, end, selected, homeDomainQuery{Limit: 100})
}

func (h *homeMonitor) summaryPage(start, end int64, selected string, query homeDomainQuery) (homeSummary, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := homeSummary{Start: start, End: end, GeneratedAt: time.Now().UnixMilli(), LastSample: h.lastSample,
		LastInventory: h.lastInventory, CollectionError: h.collectionError, InventoryError: h.inventoryError,
		UploadRate: h.uploadRate, DownloadRate: h.downloadRate, Resets: h.resets,
		RetentionDays: h.config.RetentionDays,
		Devices:       []homeDevice{}, Applications: []homeAppUsage{}, Domains: []homeDomain{}, Timeline: []homePoint{}}
	out.DomainOffset, out.DomainLimit = query.Offset, query.Limit
	_ = h.db.QueryRow(`SELECT value FROM home_metadata WHERE key='collected_since'`).Scan(&out.CollectedSince)
	devices := map[string]*homeDevice{}
	rows, err := h.db.Query(`SELECT id,name,mac,last_seen FROM home_devices`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		d := &homeDevice{Addresses: []string{}, Applications: []homeAppUsage{}}
		if err := rows.Scan(&d.ID, &d.Name, &d.MAC, &d.LastSeen); err != nil {
			rows.Close()
			return out, err
		}
		d.Name = firstNonEmpty(h.config.DeviceNames[d.ID], d.Name, "未命名设备 · "+d.ID)
		devices[d.ID] = d
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	addresses := map[string][]homeAddress{}
	rows, err = h.db.Query(`SELECT address,device_id,valid_from,valid_to FROM home_addresses WHERE valid_from<? AND (valid_to=0 OR valid_to>?) ORDER BY valid_from`, end, start)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var address homeAddress
		if err := rows.Scan(&address.Address, &address.Device, &address.From, &address.To); err != nil {
			rows.Close()
			return out, err
		}
		addresses[address.Address] = append(addresses[address.Address], address)
		if d := devices[address.Device]; d != nil && address.To == 0 {
			d.Addresses = append(d.Addresses, address.Address)
		}
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	resolve := func(address string, timestamp int64) string {
		if ip := net.ParseIP(address); ip != nil {
			address = ip.String()
		}
		for _, binding := range addresses[address] {
			// Accounting has minute resolution; align ownership to the same bucket.
			if timestamp >= binding.From/60000*60000 && (binding.To == 0 || timestamp < binding.To/60000*60000) {
				return binding.Device
			}
		}
		// Explicit, time-bounded corrections only fill gaps; observed ownership wins.
		for _, binding := range h.config.AddressBindings {
			if binding.Address == address && timestamp >= binding.From && timestamp < binding.To {
				return binding.Device
			}
		}
		return "ip:" + address
	}
	appTotals := map[string]*homeAppUsage{}
	deviceApps := map[string]map[string]*homeAppUsage{}
	domains := map[string]*homeDomain{}
	points := map[int64]*homePoint{}
	step := int64(300000)
	if end-start > 2*86400000 {
		step = 3600000
	}
	rows, err = h.db.Query(`SELECT bucket_start,source_ip,host,rule_group,SUM(upload),SUM(download)
		FROM traffic_aggregated WHERE bucket_start<? AND bucket_end>? GROUP BY bucket_start,source_ip,host,rule_group`, end, start)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var timestamp, up, down int64
		var source, host, rule string
		if err := rows.Scan(&timestamp, &source, &host, &rule, &up, &down); err != nil {
			rows.Close()
			return out, err
		}
		id := resolve(source, timestamp)
		d := devices[id]
		if d == nil {
			name := firstNonEmpty(h.config.DeviceNames[id], "未归属地址 · "+source)
			d = &homeDevice{ID: id, Name: name, Addresses: []string{source}, Applications: []homeAppUsage{}}
			if mac, err := net.ParseMAC(id); err == nil {
				d.MAC = mac.String()
			}
			devices[id] = d
		}
		d.homeUsage.add(up, down)
		app := h.classify(host, rule)
		if deviceApps[id] == nil {
			deviceApps[id] = map[string]*homeAppUsage{}
		}
		if deviceApps[id][app.ID] == nil {
			deviceApps[id][app.ID] = &homeAppUsage{homeApp: app}
		}
		deviceApps[id][app.ID].homeUsage.add(up, down)
		if selected != "" && selected != id {
			continue
		}
		out.Attributed.add(up, down)
		if appTotals[app.ID] == nil {
			appTotals[app.ID] = &homeAppUsage{homeApp: app}
		}
		appTotals[app.ID].homeUsage.add(up, down)
		key := id + "\x00" + host + "\x00" + app.ID
		if domains[key] == nil {
			domains[key] = &homeDomain{Host: host, Device: id, DeviceName: d.Name, Application: app.Name, ApplicationID: app.ID, Description: app.Description}
		}
		domains[key].homeUsage.add(up, down)
		if selected != "" {
			bucket := timestamp / step * step
			if points[bucket] == nil {
				points[bucket] = &homePoint{Timestamp: bucket}
			}
			points[bucket].homeUsage.add(up, down)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	if selected == "" {
		rows, err = h.db.Query(`SELECT bucket_start,upload,download FROM home_totals WHERE bucket_start>=? AND bucket_start<?`, start/60000*60000, end)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var timestamp, up, down int64
			if err := rows.Scan(&timestamp, &up, &down); err != nil {
				rows.Close()
				return out, err
			}
			out.Totals.add(up, down)
			bucket := timestamp / step * step
			if points[bucket] == nil {
				points[bucket] = &homePoint{Timestamp: bucket}
			}
			points[bucket].homeUsage.add(up, down)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, err
		}
		rows.Close()
		out.Unattributed.add(maxInt64(0, out.Totals.Upload-out.Attributed.Upload), maxInt64(0, out.Totals.Download-out.Attributed.Download))
	} else {
		out.Totals = out.Attributed
		out.UploadRate, out.DownloadRate = 0, 0 // Core-wide rate is not a per-device rate.
	}
	for _, d := range devices {
		for _, app := range deviceApps[d.ID] {
			d.Applications = append(d.Applications, *app)
		}
		sort.Slice(d.Applications, func(i, j int) bool { return d.Applications[i].Total > d.Applications[j].Total })
		sort.Strings(d.Addresses)
		out.Devices = append(out.Devices, *d)
	}
	for _, app := range appTotals {
		out.Applications = append(out.Applications, *app)
	}
	for _, domain := range domains {
		if query.Application == "" || domain.ApplicationID == query.Application {
			out.Domains = append(out.Domains, *domain)
			out.DomainUsage.add(domain.Upload, domain.Download)
		}
	}
	for _, point := range points {
		out.Timeline = append(out.Timeline, *point)
	}
	sort.Slice(out.Devices, func(i, j int) bool {
		if out.Devices[i].Total == out.Devices[j].Total {
			return out.Devices[i].Name < out.Devices[j].Name
		}
		return out.Devices[i].Total > out.Devices[j].Total
	})
	sort.Slice(out.Applications, func(i, j int) bool { return out.Applications[i].Total > out.Applications[j].Total })
	sort.Slice(out.Domains, func(i, j int) bool {
		a, b := out.Domains[i], out.Domains[j]
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Device != b.Device {
			return a.Device < b.Device
		}
		return a.ApplicationID < b.ApplicationID
	})
	out.DomainCount = len(out.Domains)
	first := min(query.Offset, len(out.Domains))
	last := first + min(query.Limit, len(out.Domains)-first)
	out.Domains = out.Domains[first:last]
	sort.Slice(out.Timeline, func(i, j int) bool { return out.Timeline[i].Timestamp < out.Timeline[j].Timestamp })
	return out, nil
}

func (s *service) homeRoutes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/login", s.home.handleLogin)
	mux.Handle("/", s.home.authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w)
			return
		}
		if r.URL.Path == "/api/home/summary" {
			start, end, err := parseTimeRange(r)
			if err != nil || start == end || end-start > 90*86400000 {
				writeError(w, http.StatusBadRequest, errors.New("请选择不超过 90 天的有效时间范围"))
				return
			}
			query := homeDomainQuery{Application: r.URL.Query().Get("application"), Limit: 100}
			if raw := r.URL.Query().Get("offset"); raw != "" {
				query.Offset, err = strconv.Atoi(raw)
			}
			if err == nil && r.URL.Query().Get("limit") != "" {
				query.Limit, err = strconv.Atoi(r.URL.Query().Get("limit"))
			}
			if err != nil || query.Offset < 0 || query.Limit < 1 || query.Limit > 200 {
				writeError(w, http.StatusBadRequest, errors.New("offset 必须为非负整数，limit 必须介于 1–200"))
				return
			}
			data, err := s.home.summaryPage(start, end, r.URL.Query().Get("device"), query)
			if err != nil {
				log.Printf("home summary: %v", err)
				writeError(w, http.StatusInternalServerError, errors.New("暂时无法读取统计"))
				return
			}
			writeJSON(w, http.StatusOK, data)
			return
		}
		path := map[string]string{"/": "web/home.html", "/home.js": "web/home.js", "/home.css": "web/home.css", "/favicon.svg": "web/favicon.svg", "/licenses/domain-list-community": "THIRD_PARTY_LICENSES/domain-list-community.txt", "/LICENSE": "LICENSE"}[r.URL.Path]
		if path == "" {
			http.NotFound(w, r)
			return
		}
		data, err := webAssets.ReadFile(path)
		if err != nil {
			http.Error(w, "asset unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if strings.HasSuffix(path, ".js") {
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		} else if strings.HasSuffix(path, ".css") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		} else if strings.HasSuffix(path, ".svg") {
			w.Header().Set("Content-Type", "image/svg+xml")
		} else if !strings.HasPrefix(path, "web/") {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		w.Write(data)
	})))
	return mux
}

func (h *homeMonitor) cookieValue(expiry string) string {
	mac := hmac.New(sha256.New, []byte(h.accessToken))
	mac.Write([]byte("home-monitor:" + expiry))
	return expiry + "." + hex.EncodeToString(mac.Sum(nil))
}

func (h *homeMonitor) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if h.accessToken == "" {
			next.ServeHTTP(w, r)
			return
		}
		if cookie, err := r.Cookie("home_session"); err == nil {
			parts := strings.Split(cookie.Value, ".")
			if len(parts) == 2 {
				expires, err := strconv.ParseInt(parts[0], 10, 64)
				if err == nil && expires > time.Now().Unix() && expires <= time.Now().Add(25*time.Hour).Unix() && hmac.Equal([]byte(cookie.Value), []byte(h.cookieValue(parts[0]))) {
					next.ServeHTTP(w, r)
					return
				}
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusUnauthorized, errors.New("请先登录"))
		} else {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
		}
	})
}

func (h *homeMonitor) handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 2048)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "请求无效", http.StatusBadRequest)
			return
		}
		expected, actual := sha256.Sum256([]byte(h.accessToken)), sha256.Sum256([]byte(r.FormValue("token")))
		if h.accessToken != "" && subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
			http.Error(w, "密钥不正确，请返回重试", http.StatusUnauthorized)
			return
		}
		expires := time.Now().Add(24 * time.Hour)
		http.SetCookie(w, &http.Cookie{Name: "home_session", Value: h.cookieValue(strconv.FormatInt(expires.Unix(), 10)), Path: "/", Expires: expires,
			HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet {
		writeMethodNotAllowed(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	io.WriteString(w, `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>全屋流量 · 登录</title><style>body{background:#f4f6f9;color:#182332;font:16px system-ui;margin:0;padding:24px}main{max-width:340px;margin:12vh auto;background:white;padding:32px;border-radius:20px;box-shadow:0 12px 50px #16334e12}h1{font-size:26px}p{color:#617083;line-height:1.7}input,button{box-sizing:border-box;width:100%;padding:14px;border:1px solid #dce2ea;border-radius:10px;font:inherit;margin-top:12px}button{background:#1767cf;color:white;cursor:pointer}</style><main><h1>全屋流量</h1><p>使用现有 sing-box 面板的 API Secret 登录。</p><form method="post" action="/login"><label for="token">访问密钥</label><input id="token" name="token" type="password" required autocomplete="current-password"><button>进入监控</button></form></main></html>`)
}
