package main

import (
	"crypto/rand"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// Set by CI: -ldflags "-X main.version=v1.0.0"
var version = "dev"

//go:embed web/static/*
var webFS embed.FS

type ForwardRule struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ListenAddr string `json:"listen_addr"`
	ListenPort int    `json:"listen_port"`
	TargetAddr string `json:"target_addr"`
	TargetPort int    `json:"target_port"`
	Direction  string `json:"direction"` // legacy; kept for compatibility
	DialSide   string `json:"dial_side"` // "peer" (default) = peer dials target; "local" = this machine dials target
	Enabled    bool   `json:"enabled"`
}

type Config struct {
	Token              string        `json:"token"`
	TokenPendingReveal bool          `json:"token_pending_reveal,omitempty"`
	Peers              []PeerConfig  `json:"peers"`
	Rules              []ForwardRule `json:"rules"`
}

type PeerConfig struct {
	Name string `json:"name"`
	Addr string `json:"addr"` // host:port of peer's main port
}

type PeerSession struct {
	Name           string
	Addr           string
	RemoteHostname string
	RemoteIPs      string
	Self           bool
	IsClient       bool
	Session        *yamux.Session
	Control        net.Conn
	mu             sync.Mutex
}

type App struct {
	port       int
	configPath string
	config     Config
	mu         sync.RWMutex
	peers      map[string]*PeerSession
	peerRules  map[string][]ForwardRule // peer name -> remote rules snapshot
	listeners  map[string]net.Listener  // rule ID -> listener
	listenErrs map[string]string        // rule ID -> last listener startup error
	listenerMu sync.Mutex
}

func main() {
	host := flag.String("host", "0.0.0.0", "主端口监听 IPv4 地址")
	port := flag.Int("port", 9000, "主端口（管理网页+隧道）")
	configFile := flag.String("config", "", "配置文件路径（默认 ~/.port_fwd.json）")
	flag.Parse()

	cfgPath := *configFile
	if cfgPath == "" {
		home, _ := os.UserHomeDir()
		cfgPath = filepath.Join(home, ".port_fwd.json")
	}

	if flag.NArg() > 0 {
		if flag.NArg() != 1 || flag.Arg(0) != "reset" {
			log.Fatalf("未知命令 %q；可用命令: reset", flag.Arg(0))
		}
		if err := resetConfig(cfgPath); err != nil {
			log.Fatal(err)
		}
		return
	}

	app := &App{
		port:       *port,
		configPath: cfgPath,
		peers:      make(map[string]*PeerSession),
		peerRules:  make(map[string][]ForwardRule),
		listeners:  make(map[string]net.Listener),
		listenErrs: make(map[string]string),
	}

	app.loadConfig()
	app.startLocalRules()
	go app.connectPeers()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", app.handleConfig)
	mux.HandleFunc("/api/rules", app.handleRules)
	mux.HandleFunc("/api/rules/", app.handleRuleByID)
	mux.HandleFunc("/api/peers", app.handlePeers)
	mux.HandleFunc("/api/peers/", app.handlePeerByID)
	mux.HandleFunc("/api/status", app.handleStatus)
	mux.HandleFunc("/api/diag", app.handleDiag)
	mux.HandleFunc("/api/diag/dial", app.handleDiagDial)
	mux.HandleFunc("/tunnel", app.handleTunnel)
	mux.HandleFunc("/", app.handleWeb)

	addr := net.JoinHostPort(*host, strconv.Itoa(app.port))
	log.Printf("port_fwd %s 启动: http://%s", version, addr)
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// --- Config ---

func resetConfig(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		fmt.Printf("配置文件不存在，无需清除: %s\n", path)
		return nil
	}
	if err != nil {
		return fmt.Errorf("清除配置 %s 失败: %w", path, err)
	}
	fmt.Printf("已清除配置: %s\n", path)
	return nil
}

func (a *App) loadConfig() {
	data, err := os.ReadFile(a.configPath)
	if err == nil {
		if err := json.Unmarshal(data, &a.config); err != nil {
			log.Fatalf("读取配置文件 %s 失败: %v", a.configPath, err)
		}
	} else if !os.IsNotExist(err) {
		log.Fatalf("读取配置文件 %s 失败: %v", a.configPath, err)
	}

	if a.config.Token == "" {
		tokenBytes := make([]byte, 24)
		if _, err := rand.Read(tokenBytes); err != nil {
			log.Fatalf("生成随机 Token 失败: %v", err)
		}
		a.config.Token = hex.EncodeToString(tokenBytes)
		a.config.TokenPendingReveal = true
		if err := a.saveConfig(); err != nil {
			log.Fatalf("保存初始配置 %s 失败: %v", a.configPath, err)
		}
		log.Printf("已生成随机 Token 并保存到 %s", a.configPath)
	}
}

func (a *App) saveConfig() error {
	a.mu.RLock()
	data, _ := json.MarshalIndent(a.config, "", "  ")
	a.mu.RUnlock()
	if err := os.MkdirAll(filepath.Dir(a.configPath), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(a.configPath, data, 0600); err != nil {
		return err
	}
	return os.Chmod(a.configPath, 0600)
}

// --- Tunnel (yamux over raw TCP upgrade) ---

func (a *App) handleTunnel(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Token")
	if token != a.config.Token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	peerName := r.Header.Get("X-Peer-Name")
	if peerName == "" {
		peerName = r.RemoteAddr
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Tunnel", "ok")
	w.WriteHeader(http.StatusSwitchingProtocols)

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		log.Printf("hijack error: %v", err)
		return
	}
	bufrw.Flush()

	session, err := yamux.Server(conn, yamux.DefaultConfig())
	if err != nil {
		log.Printf("yamux server error: %v", err)
		conn.Close()
		return
	}

	remote, err := a.exchangeHelloAsServer(session)
	if err != nil {
		log.Printf("节点身份握手失败(%s): %v", peerName, err)
		session.Close()
		return
	}
	if remote.Hostname != "" {
		peerName = remote.Hostname
	}

	ps := &PeerSession{
		Name:           peerName,
		Addr:           r.RemoteAddr,
		RemoteHostname: remote.Hostname,
		RemoteIPs:      remote.IPs,
		Self:           isSelfPeer(remote.Hostname, remote.IPs),
		IsClient:       false,
		Session:        session,
	}
	if ps.Self {
		log.Printf("警告: 连入节点 %s 实际是本机(ips=%s)。若用「对端拨号」，会在本机执行，无法访问对端内网。", peerName, remote.IPs)
	} else {
		log.Printf("节点连入: %s (%s) ips=%s", peerName, r.RemoteAddr, remote.IPs)
	}

	if err := a.attachControl(ps); err != nil {
		log.Printf("控制通道建立失败(%s): %v", peerName, err)
		session.Close()
		return
	}

	a.mu.Lock()
	a.peers[peerName] = ps
	a.mu.Unlock()

	a.pushRulesToPeer(ps)
	go a.servePeerStreams(ps)
}

func (a *App) servePeerStreams(ps *PeerSession) {
	defer func() {
		ps.Session.Close()
		if ps.Control != nil {
			ps.Control.Close()
		}
		a.mu.Lock()
		delete(a.peers, ps.Name)
		delete(a.peerRules, ps.Name)
		a.mu.Unlock()
		log.Printf("节点断开: %s", ps.Name)
	}()

	for {
		stream, err := ps.Session.Accept()
		if err != nil {
			return
		}
		go a.handleIncomingStream(stream)
	}
}

func (a *App) handleIncomingStream(stream net.Conn) {
	defer stream.Close()

	var req struct {
		Target string `json:"target"`
	}
	if err := readJSONFrame(stream, &req); err != nil || req.Target == "" {
		return
	}

	hostname, _ := os.Hostname()
	ips := localIPv4List()
	log.Printf("收到转发请求: %s （本机 %s 负责拨号, ips=%s）", req.Target, hostname, ips)

	target, err := net.DialTimeout("tcp4", req.Target, 15*time.Second)
	if err != nil {
		log.Printf("本机(%s)拨号 %s 失败: %v", hostname, req.Target, err)
		_ = writeJSONFrame(stream, map[string]any{
			"ok":        false,
			"error":     err.Error(),
			"dialer":    hostname,
			"target":    req.Target,
			"dial_side": "peer",
			"ips":       ips,
		})
		return
	}
	defer target.Close()

	if err := writeJSONFrame(stream, map[string]any{"ok": true, "dialer": hostname, "ips": ips}); err != nil {
		return
	}

	relay(stream, target)
}

// --- Peer connection (client side) ---

func (a *App) connectPeers() {
	for {
		a.mu.RLock()
		peers := make([]PeerConfig, len(a.config.Peers))
		copy(peers, a.config.Peers)
		a.mu.RUnlock()

		for _, p := range peers {
			a.mu.RLock()
			_, exists := a.peers[p.Name]
			a.mu.RUnlock()
			if exists {
				continue
			}
			go a.dialPeer(p)
		}
		time.Sleep(5 * time.Second)
	}
}

func (a *App) dialPeer(p PeerConfig) {
	conn, err := net.DialTimeout("tcp", p.Addr, 10*time.Second)
	if err != nil {
		log.Printf("连接节点 %s (%s) 失败: %v", p.Name, p.Addr, err)
		return
	}

	hostname, _ := os.Hostname()
	reqLine := fmt.Sprintf("GET /tunnel HTTP/1.1\r\nHost: %s\r\nX-Token: %s\r\nX-Peer-Name: %s\r\nConnection: Upgrade\r\nUpgrade: yamux\r\n\r\n",
		p.Addr, a.config.Token, hostname)
	conn.Write([]byte(reqLine))

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return
	}
	resp := string(buf[:n])
	if len(resp) < 12 || resp[9:12] != "101" {
		log.Printf("节点 %s 握手失败: %s", p.Name, resp[:min(len(resp), 80)])
		conn.Close()
		return
	}

	session, err := yamux.Client(conn, yamux.DefaultConfig())
	if err != nil {
		log.Printf("yamux client error for %s: %v", p.Name, err)
		conn.Close()
		return
	}

	remote, err := a.exchangeHelloAsClient(session)
	if err != nil {
		log.Printf("节点身份握手失败(%s): %v", p.Name, err)
		session.Close()
		return
	}

	ps := &PeerSession{
		Name:           p.Name,
		Addr:           p.Addr,
		RemoteHostname: remote.Hostname,
		RemoteIPs:      remote.IPs,
		Self:           isSelfPeer(remote.Hostname, remote.IPs),
		IsClient:       true,
		Session:        session,
	}
	if ps.Self {
		log.Printf("警告: 节点 %s (%s) 实际连回了本机(ips=%s)。请把节点地址改成家里 frp visitor，而不是公司本机。", p.Name, p.Addr, remote.IPs)
	} else {
		log.Printf("已连接节点: %s (%s) remote=%s ips=%s", p.Name, p.Addr, remote.Hostname, remote.IPs)
	}

	if err := a.attachControl(ps); err != nil {
		log.Printf("控制通道建立失败(%s): %v", p.Name, err)
		session.Close()
		return
	}

	a.mu.Lock()
	a.peers[p.Name] = ps
	a.mu.Unlock()

	a.pushRulesToPeer(ps)
	go a.servePeerStreams(ps)
}

// --- Local port forwarding ---

func (a *App) startLocalRules() {
	a.mu.RLock()
	rules := make([]ForwardRule, len(a.config.Rules))
	copy(rules, a.config.Rules)
	a.mu.RUnlock()

	for _, r := range rules {
		if ruleShouldListen(r) {
			go a.startLocalListener(r)
		}
	}
}

func (a *App) startLocalListener(rule ForwardRule) {
	addr := fmt.Sprintf("%s:%d", rule.ListenAddr, rule.ListenPort)
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		a.listenerMu.Lock()
		a.listenErrs[rule.ID] = err.Error()
		a.listenerMu.Unlock()
		log.Printf("监听 %s 失败: %v", addr, err)
		return
	}

	a.listenerMu.Lock()
	if old, ok := a.listeners[rule.ID]; ok {
		old.Close()
	}
	a.listeners[rule.ID] = ln
	delete(a.listenErrs, rule.ID)
	a.listenerMu.Unlock()

	target := fmt.Sprintf("%s:%d", rule.TargetAddr, rule.TargetPort)
	dialSide := normalizeDialSide(rule.DialSide)
	log.Printf("本地转发: %s → [%s] %s", addr, dialSide, target)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go a.forwardConnection(conn, target, dialSide)
	}
}

func normalizeDialSide(side string) string {
	if strings.EqualFold(strings.TrimSpace(side), "local") {
		return "local"
	}
	return "peer"
}

func (a *App) forwardConnection(local net.Conn, target, dialSide string) {
	if dialSide == "local" {
		a.forwardLocal(local, target)
		return
	}
	a.forwardToPeer(local, target)
}

func (a *App) forwardLocal(local net.Conn, target string) {
	defer local.Close()
	hostname, _ := os.Hostname()
	remote, err := net.DialTimeout("tcp4", target, 15*time.Second)
	if err != nil {
		log.Printf("本机(%s)本地拨号 %s 失败: %v —— 「目标在本机网络」会由监听端拨号；要让家里拨，请改选「目标在对端网络」且节点必须是家里", hostname, target, err)
		return
	}
	defer remote.Close()
	relay(local, remote)
}

func (a *App) pickPeerSession() (name string, session *yamux.Session, self bool, remoteHost, remoteIPs string) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	type candidate struct {
		name       string
		session    *yamux.Session
		self       bool
		remoteHost string
		remoteIPs  string
	}
	var all []candidate
	for n, ps := range a.peers {
		if ps != nil && ps.Session != nil && !ps.Session.IsClosed() {
			all = append(all, candidate{n, ps.Session, ps.Self, ps.RemoteHostname, ps.RemoteIPs})
		}
	}
	if len(all) == 0 {
		return "", nil, false, "", ""
	}
	sort.Slice(all, func(i, j int) bool { return all[i].name < all[j].name })
	for _, c := range all {
		if !c.self {
			return c.name, c.session, false, c.remoteHost, c.remoteIPs
		}
	}
	c := all[0]
	return c.name, c.session, true, c.remoteHost, c.remoteIPs
}

func (a *App) forwardToPeer(local net.Conn, target string) {
	defer local.Close()

	peerName, session, self, remoteHost, remoteIPs := a.pickPeerSession()
	if session == nil {
		log.Printf("无可用节点，丢弃连接到 %s", target)
		return
	}
	if self {
		log.Printf("拒绝转发 %s: 节点 %s 实际是本机(remote=%s ips=%s)。请把节点地址改成家里的 port_fwd（frp visitor），不要连回公司自己。", target, peerName, remoteHost, remoteIPs)
		return
	}

	stream, err := session.Open()
	if err != nil {
		log.Printf("打开流失败: %v", err)
		return
	}
	defer stream.Close()

	log.Printf("经节点 %s(remote=%s ips=%s) 请求拨号 %s", peerName, remoteHost, remoteIPs, target)
	if err := writeJSONFrame(stream, map[string]string{"target": target}); err != nil {
		log.Printf("发送转发请求失败: %v", err)
		return
	}

	var resp struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Dialer string `json:"dialer"`
		Target string `json:"target"`
		IPs    string `json:"ips"`
	}
	if err := readJSONFrame(stream, &resp); err != nil {
		log.Printf("读取对端拨号结果失败: %v", err)
		return
	}
	if !resp.OK {
		who := resp.Dialer
		if who == "" {
			who = peerName
		}
		log.Printf("对端(%s)连接 %s 失败: %s (对端网卡: %s)", who, target, resp.Error, resp.IPs)
		return
	}

	relay(local, stream)
}

func (a *App) stopRule(id string) {
	a.listenerMu.Lock()
	if ln, ok := a.listeners[id]; ok {
		ln.Close()
		delete(a.listeners, id)
	}
	delete(a.listenErrs, id)
	a.listenerMu.Unlock()
}

// --- HTTP API ---

func (a *App) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.mu.Lock()
		response := struct {
			Token    string `json:"token,omitempty"`
			HasToken bool   `json:"has_token"`
			FirstRun bool   `json:"first_run"`
		}{
			HasToken: a.config.Token != "",
			FirstRun: a.config.TokenPendingReveal,
		}
		if a.config.TokenPendingReveal {
			response.Token = a.config.Token
			a.config.TokenPendingReveal = false
		}
		a.mu.Unlock()

		if response.FirstRun {
			if err := a.saveConfig(); err != nil {
				a.mu.Lock()
				a.config.TokenPendingReveal = true
				a.mu.Unlock()
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	case http.MethodPut:
		var request struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		request.Token = strings.TrimSpace(request.Token)
		if request.Token == "" {
			http.Error(w, "Token 不能为空", http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		a.config.Token = request.Token
		a.config.TokenPendingReveal = false
		a.mu.Unlock()
		if err := a.saveConfig(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func validateRule(rule ForwardRule) error {
	if rule.ListenPort < 1 || rule.ListenPort > 65535 {
		return fmt.Errorf("本地监听端口必须是 1–65535")
	}
	if rule.ListenPort < 1024 {
		return fmt.Errorf("本地端口 %d 属于特权端口（1–1023），普通用户没有权限绑定，请改用 1024–65535", rule.ListenPort)
	}
	if strings.TrimSpace(rule.TargetAddr) == "" {
		return fmt.Errorf("目标地址不能为空")
	}
	if rule.TargetPort < 1 || rule.TargetPort > 65535 {
		return fmt.Errorf("目标端口必须是 1–65535")
	}
	if strings.TrimSpace(rule.ListenAddr) == "" {
		return fmt.Errorf("监听地址不能为空")
	}
	side := strings.ToLower(strings.TrimSpace(rule.DialSide))
	if side != "" && side != "peer" && side != "local" {
		return fmt.Errorf("dial_side 只能是 peer 或 local")
	}
	return nil
}

func normalizeRule(rule *ForwardRule) {
	if rule.Direction == "" {
		rule.Direction = "local"
	}
	rule.DialSide = normalizeDialSide(rule.DialSide)
}

func ruleShouldListen(rule ForwardRule) bool {
	return rule.Enabled && (rule.Direction == "local" || rule.Direction == "")
}

func (a *App) handleRules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.mu.RLock()
		json.NewEncoder(w).Encode(a.config.Rules)
		a.mu.RUnlock()
	case http.MethodPost:
		var rule ForwardRule
		if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := validateRule(rule); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		normalizeRule(&rule)
		if rule.ID == "" {
			rule.ID = fmt.Sprintf("%d", time.Now().UnixNano())
		}
		a.mu.Lock()
		a.config.Rules = append(a.config.Rules, rule)
		a.mu.Unlock()
		a.saveConfig()
		if ruleShouldListen(rule) {
			go a.startLocalListener(rule)
		}
		a.broadcastRules()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rule)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handleRuleByID(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[len("/api/rules/"):]
	if id == "" {
		http.Error(w, "missing id", 400)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		a.stopRule(id)
		a.mu.Lock()
		for i, rule := range a.config.Rules {
			if rule.ID == id {
				a.config.Rules = append(a.config.Rules[:i], a.config.Rules[i+1:]...)
				break
			}
		}
		a.mu.Unlock()
		a.saveConfig()
		a.broadcastRules()
		w.Write([]byte(`{"ok":true}`))
	case http.MethodPut:
		var updated ForwardRule
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := validateRule(updated); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		normalizeRule(&updated)
		found := false
		a.mu.Lock()
		for i, rule := range a.config.Rules {
			if rule.ID == id {
				updated.ID = id
				a.config.Rules[i] = updated
				found = true
				break
			}
		}
		a.mu.Unlock()
		if !found {
			http.Error(w, "rule not found", http.StatusNotFound)
			return
		}
		a.stopRule(id)
		if err := a.saveConfig(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if ruleShouldListen(updated) {
			go a.startLocalListener(updated)
		}
		a.broadcastRules()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(updated)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handlePeers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.mu.RLock()
		json.NewEncoder(w).Encode(a.config.Peers)
		a.mu.RUnlock()
	case http.MethodPost:
		var p PeerConfig
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		a.mu.Lock()
		a.config.Peers = append(a.config.Peers, p)
		a.mu.Unlock()
		a.saveConfig()
		go a.dialPeer(p)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handlePeerByID(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[len("/api/peers/"):]
	if name == "" {
		http.Error(w, "missing name", 400)
		return
	}

	switch r.Method {
	case http.MethodDelete:
		a.mu.Lock()
		if ps, ok := a.peers[name]; ok {
			ps.Session.Close()
			delete(a.peers, name)
			delete(a.peerRules, name)
		}
		for i, p := range a.config.Peers {
			if p.Name == name {
				a.config.Peers = append(a.config.Peers[:i], a.config.Peers[i+1:]...)
				break
			}
		}
		a.mu.Unlock()
		a.saveConfig()
		w.Write([]byte(`{"ok":true}`))
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	type PeerStatus struct {
		Name           string `json:"name"`
		Addr           string `json:"addr"`
		Connected      bool   `json:"connected"`
		RemoteHostname string `json:"remote_hostname,omitempty"`
		RemoteIPs      string `json:"remote_ips,omitempty"`
		Self           bool   `json:"self"`
	}
	type RuleStatus struct {
		ForwardRule
		Listening bool   `json:"listening"`
		Error     string `json:"error,omitempty"`
	}
	var ps []PeerStatus
	for _, p := range a.config.Peers {
		item := PeerStatus{Name: p.Name, Addr: p.Addr}
		if sess, ok := a.peers[p.Name]; ok && sess.Session != nil && !sess.Session.IsClosed() {
			item.Connected = true
			item.RemoteHostname = sess.RemoteHostname
			item.RemoteIPs = sess.RemoteIPs
			item.Self = sess.Self
		}
		ps = append(ps, item)
	}
	// include incoming peers not in config
	for name, sess := range a.peers {
		found := false
		for _, p := range ps {
			if p.Name == name {
				found = true
				break
			}
		}
		if !found {
			ps = append(ps, PeerStatus{
				Name:           name,
				Addr:           sess.Addr,
				Connected:      sess.Session != nil && !sess.Session.IsClosed(),
				RemoteHostname: sess.RemoteHostname,
				RemoteIPs:      sess.RemoteIPs,
				Self:           sess.Self,
			})
		}
	}
	var rs []RuleStatus
	a.listenerMu.Lock()
	for _, rule := range a.config.Rules {
		_, listening := a.listeners[rule.ID]
		rs = append(rs, RuleStatus{
			ForwardRule: rule,
			Listening:   listening,
			Error:       a.listenErrs[rule.ID],
		})
	}
	a.listenerMu.Unlock()

	type PeerRulesView struct {
		Peer     string        `json:"peer"`
		Hostname string        `json:"hostname"`
		Rules    []ForwardRule `json:"rules"`
	}
	var remoteRules []PeerRulesView
	for name, rules := range a.peerRules {
		item := PeerRulesView{
			Peer:     name,
			Hostname: name,
			Rules:    append([]ForwardRule(nil), rules...),
		}
		if sess, ok := a.peers[name]; ok && sess.RemoteHostname != "" {
			item.Hostname = sess.RemoteHostname
		}
		remoteRules = append(remoteRules, item)
	}
	a.mu.RUnlock()

	hostname, _ := os.Hostname()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"peers":      ps,
		"rules":      rs,
		"peer_rules": remoteRules,
		"hostname":   hostname,
		"version":    version,
		"ips":        localIPv4List(),
	})
}

func (a *App) handleDiag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	hostname, _ := os.Hostname()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"hostname": hostname,
		"version":  version,
		"ips":      localIPv4List(),
	})
}

func (a *App) handleDiagDial(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req struct {
		Addr string `json:"addr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	req.Addr = strings.TrimSpace(req.Addr)
	if req.Addr == "" {
		http.Error(w, "addr 不能为空", 400)
		return
	}
	hostname, _ := os.Hostname()
	start := time.Now()
	conn, err := net.DialTimeout("tcp4", req.Addr, 5*time.Second)
	result := map[string]any{
		"addr":     req.Addr,
		"hostname": hostname,
		"ips":      localIPv4List(),
		"ms":       time.Since(start).Milliseconds(),
	}
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
	} else {
		conn.Close()
		result["ok"] = true
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// --- Web UI ---

func (a *App) handleWeb(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" {
		path = "/index.html"
	}
	data, err := webFS.ReadFile("web/static" + path)
	if err != nil {
		data, _ = webFS.ReadFile("web/static/index.html")
	}
	if len(data) == 0 {
		http.NotFound(w, r)
		return
	}
	switch filepath.Ext(path) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "application/javascript")
	case ".css":
		w.Header().Set("Content-Type", "text/css")
	}
	w.Write(data)
}

// --- Utils ---

func localIPv4List() string {
	ifaces, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	var ips []string
	for _, addr := range ifaces {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP == nil {
			continue
		}
		ip := ipNet.IP.To4()
		if ip == nil || ip.IsLoopback() {
			continue
		}
		ips = append(ips, ip.String())
	}
	sort.Strings(ips)
	return strings.Join(ips, ",")
}

type peerHello struct {
	Hostname string `json:"hostname"`
	IPs      string `json:"ips"`
	Version  string `json:"version"`
}

func localHello() peerHello {
	hostname, _ := os.Hostname()
	return peerHello{
		Hostname: hostname,
		IPs:      localIPv4List(),
		Version:  version,
	}
}

func isSelfPeer(remoteHost, remoteIPs string) bool {
	localHost, _ := os.Hostname()
	if remoteHost != "" && remoteHost == localHost {
		return true
	}
	localSet := map[string]struct{}{}
	for _, ip := range strings.Split(localIPv4List(), ",") {
		ip = strings.TrimSpace(ip)
		if ip != "" {
			localSet[ip] = struct{}{}
		}
	}
	for _, ip := range strings.Split(remoteIPs, ",") {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if _, ok := localSet[ip]; ok {
			return true
		}
	}
	return false
}

func (a *App) attachControl(ps *PeerSession) error {
	var (
		ctrl net.Conn
		err  error
	)
	if ps.IsClient {
		ctrl, err = ps.Session.Open()
	} else {
		ctrl, err = ps.Session.Accept()
	}
	if err != nil {
		return err
	}
	ps.Control = ctrl
	go a.controlLoop(ps)
	return nil
}

type controlMsg struct {
	Type  string        `json:"type"`
	Rules []ForwardRule `json:"rules,omitempty"`
}

func (a *App) controlLoop(ps *PeerSession) {
	defer ps.Control.Close()
	for {
		var msg controlMsg
		if err := readJSONFrame(ps.Control, &msg); err != nil {
			return
		}
		switch msg.Type {
		case "rules_sync":
			rules := append([]ForwardRule(nil), msg.Rules...)
			a.mu.Lock()
			a.peerRules[ps.Name] = rules
			a.mu.Unlock()
			log.Printf("已同步节点 %s 的转发规则 %d 条", ps.Name, len(rules))
		}
	}
}

func (a *App) snapshotRules() []ForwardRule {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]ForwardRule, len(a.config.Rules))
	copy(out, a.config.Rules)
	return out
}

func (a *App) pushRulesToPeer(ps *PeerSession) {
	if ps == nil || ps.Control == nil || ps.Self {
		return
	}
	msg := controlMsg{Type: "rules_sync", Rules: a.snapshotRules()}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.Control == nil {
		return
	}
	if err := writeJSONFrame(ps.Control, msg); err != nil {
		log.Printf("向节点 %s 同步规则失败: %v", ps.Name, err)
	}
}

func (a *App) broadcastRules() {
	a.mu.RLock()
	peers := make([]*PeerSession, 0, len(a.peers))
	for _, ps := range a.peers {
		peers = append(peers, ps)
	}
	a.mu.RUnlock()
	for _, ps := range peers {
		a.pushRulesToPeer(ps)
	}
}

func (a *App) exchangeHelloAsClient(session *yamux.Session) (*peerHello, error) {
	stream, err := session.Open()
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	if err := writeJSONFrame(stream, localHello()); err != nil {
		return nil, err
	}
	var remote peerHello
	if err := readJSONFrame(stream, &remote); err != nil {
		return nil, err
	}
	return &remote, nil
}

func (a *App) exchangeHelloAsServer(session *yamux.Session) (*peerHello, error) {
	stream, err := session.Accept()
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(10 * time.Second))
	var remote peerHello
	if err := readJSONFrame(stream, &remote); err != nil {
		return nil, err
	}
	if err := writeJSONFrame(stream, localHello()); err != nil {
		return nil, err
	}
	return &remote, nil
}

func writeJSONFrame(w net.Conn, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(payload) > 1<<20 {
		return fmt.Errorf("frame too large")
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func readJSONFrame(r net.Conn, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > 1<<20 {
		return fmt.Errorf("invalid frame size %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(b, a)
		if tc, ok := b.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		io.Copy(a, b)
		if tc, ok := a.(*net.TCPConn); ok {
			tc.CloseWrite()
		}
	}()
	wg.Wait()
}
