package main

import (
	"crypto/rand"
	"embed"
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
	Direction  string `json:"direction"` // "local" = listen locally, forward to peer's target; "remote" = peer listens, forward to our target
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
	Name    string
	Addr    string
	Session *yamux.Session
	mu      sync.Mutex
}

type App struct {
	port       int
	configPath string
	config     Config
	mu         sync.RWMutex
	peers      map[string]*PeerSession
	listeners  map[string]net.Listener // rule ID -> listener
	listenErrs map[string]string       // rule ID -> last listener startup error
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

	log.Printf("节点连入: %s (%s)", peerName, r.RemoteAddr)

	ps := &PeerSession{Name: peerName, Addr: r.RemoteAddr, Session: session}
	a.mu.Lock()
	a.peers[peerName] = ps
	a.mu.Unlock()

	go a.servePeerStreams(ps)
}

func (a *App) servePeerStreams(ps *PeerSession) {
	defer func() {
		ps.Session.Close()
		a.mu.Lock()
		delete(a.peers, ps.Name)
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

	buf := make([]byte, 1024)
	n, err := stream.Read(buf)
	if err != nil {
		return
	}

	var req struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(buf[:n], &req); err != nil || req.Target == "" {
		return
	}

	target, err := net.DialTimeout("tcp", req.Target, 5*time.Second)
	if err != nil {
		stream.Write([]byte(`{"error":"` + err.Error() + `"}`))
		return
	}
	defer target.Close()

	stream.Write([]byte(`{"ok":true}`))

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

	log.Printf("已连接节点: %s (%s)", p.Name, p.Addr)

	ps := &PeerSession{Name: p.Name, Addr: p.Addr, Session: session}
	a.mu.Lock()
	a.peers[p.Name] = ps
	a.mu.Unlock()

	go a.servePeerStreams(ps)
}

// --- Local port forwarding ---

func (a *App) startLocalRules() {
	a.mu.RLock()
	rules := make([]ForwardRule, len(a.config.Rules))
	copy(rules, a.config.Rules)
	a.mu.RUnlock()

	for _, r := range rules {
		if r.Enabled && r.Direction == "local" {
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
	log.Printf("本地转发: %s → 对端 %s", addr, target)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go a.forwardToPeer(conn, target)
	}
}

func (a *App) forwardToPeer(local net.Conn, target string) {
	defer local.Close()

	a.mu.RLock()
	var session *yamux.Session
	for _, ps := range a.peers {
		if ps.Session != nil && !ps.Session.IsClosed() {
			session = ps.Session
			break
		}
	}
	a.mu.RUnlock()

	if session == nil {
		log.Printf("无可用节点，丢弃连接到 %s", target)
		return
	}

	stream, err := session.Open()
	if err != nil {
		log.Printf("打开流失败: %v", err)
		return
	}
	defer stream.Close()

	req, _ := json.Marshal(map[string]string{"target": target})
	stream.Write(req)

	buf := make([]byte, 1024)
	n, err := stream.Read(buf)
	if err != nil {
		return
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	json.Unmarshal(buf[:n], &resp)
	if !resp.OK {
		log.Printf("对端连接 %s 失败: %s", target, resp.Error)
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
		if rule.ID == "" {
			rule.ID = fmt.Sprintf("%d", time.Now().UnixNano())
		}
		a.mu.Lock()
		a.config.Rules = append(a.config.Rules, rule)
		a.mu.Unlock()
		a.saveConfig()
		if rule.Enabled && rule.Direction == "local" {
			go a.startLocalListener(rule)
		}
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
		w.Write([]byte(`{"ok":true}`))
	case http.MethodPut:
		var updated ForwardRule
		if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
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
		if updated.Enabled && updated.Direction == "local" {
			go a.startLocalListener(updated)
		}
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
		Name      string `json:"name"`
		Addr      string `json:"addr"`
		Connected bool   `json:"connected"`
	}
	type RuleStatus struct {
		ForwardRule
		Listening bool   `json:"listening"`
		Error     string `json:"error,omitempty"`
	}
	var ps []PeerStatus
	for _, p := range a.config.Peers {
		connected := false
		if sess, ok := a.peers[p.Name]; ok && sess.Session != nil && !sess.Session.IsClosed() {
			connected = true
		}
		ps = append(ps, PeerStatus{Name: p.Name, Addr: p.Addr, Connected: connected})
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
			ps = append(ps, PeerStatus{Name: name, Addr: sess.Addr, Connected: !sess.Session.IsClosed()})
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
	a.mu.RUnlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"peers": ps,
		"rules": rs,
	})
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
