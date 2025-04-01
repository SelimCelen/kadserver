package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/peerstore"
	kaddht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	routingdiscovery "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	stateFileName      = "krelay-state.json"
	stateBackupPrefix  = "krelay-state-"
	configFileName     = "config.json"
	maxStateBackups    = 5
	stateSaveInterval  = 5 * time.Minute
	stateSaveTimeout   = 10 * time.Second
	stateRetryInterval = 1 * time.Minute
	version           = "1.0.0"
)

type Config struct {
	ListenAddrs     []string `json:"listenAddrs"`
	BootstrapPeers  []string `json:"bootstrapPeers"`
	EnableRelay     bool     `json:"enableRelay"`
	EnableAutoRelay bool     `json:"enableAutoRelay"`
	APIPort         int      `json:"apiPort"`
	PrivateKeyPath  string   `json:"privateKeyPath"`
	DataDir         string   `json:"dataDir"`
}

type NodeState struct {
	PeerID        string   `json:"peerId"`
	ListenAddrs   []string `json:"listenAddrs"`
	ConnectedPeers []string `json:"connectedPeers"`
	DHTBuckets    []string `json:"dhtBuckets"`
	LastUpdated   int64    `json:"lastUpdated"`
	Version       string   `json:"version"`
}

type StateManager struct {
	config       *Config
	logger       *zap.Logger
	state        NodeState
	stateMutex   sync.RWMutex
	lastSaveTime time.Time
}

func NewStateManager(cfg *Config, logger *zap.Logger) *StateManager {
	return &StateManager{
		config: cfg,
		logger: logger,
	}
}

func (sm *StateManager) SaveState(h host.Host, dht *kaddht.IpfsDHT) error {
	sm.stateMutex.Lock()
	defer sm.stateMutex.Unlock()

	_, cancel := context.WithTimeout(context.Background(), stateSaveTimeout)
	defer cancel()

	peers := h.Network().Peers()
	peerIDs := make([]string, 0, len(peers))
	for _, p := range peers {
		peerIDs = append(peerIDs, p.String())
	}

	var buckets []string
	if rt := dht.RoutingTable(); rt != nil {
		for _, p := range rt.ListPeers() {
			buckets = append(buckets, p.String())
		}
	}

	sm.state = NodeState{
		PeerID:        h.ID().String(),
		ListenAddrs:   multiaddrListToStrings(h.Addrs()),
		ConnectedPeers: peerIDs,
		DHTBuckets:    buckets,
		LastUpdated:   time.Now().Unix(),
		Version:       version,
	}

	tempPath := filepath.Join(sm.config.DataDir, stateFileName+".tmp")
	data, err := json.MarshalIndent(sm.state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp state file: %w", err)
	}

	if err := sm.rotateBackups(); err != nil {
		sm.logger.Warn("Failed to rotate state backups", zap.Error(err))
	}

	finalPath := filepath.Join(sm.config.DataDir, stateFileName)
	if err := os.Rename(tempPath, finalPath); err != nil {
		return fmt.Errorf("failed to rename temp state file: %w", err)
	}

	sm.lastSaveTime = time.Now()
	return nil
}

func (sm *StateManager) rotateBackups() error {
	finalPath := filepath.Join(sm.config.DataDir, stateFileName)
	if _, err := os.Stat(finalPath); os.IsNotExist(err) {
		return nil
	}

	data, err := os.ReadFile(finalPath)
	if err != nil {
		return fmt.Errorf("failed to read state file for backup: %w", err)
	}

	backupPath := filepath.Join(sm.config.DataDir, fmt.Sprintf("%s%d.json", stateBackupPrefix, time.Now().Unix()))
	if err := os.WriteFile(backupPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write backup file: %w", err)
	}

	files, err := filepath.Glob(filepath.Join(sm.config.DataDir, stateBackupPrefix+"*.json"))
	if err != nil {
		return fmt.Errorf("failed to list backup files: %w", err)
	}

	if len(files) > maxStateBackups {
		sortFilesByModTime(files)
		for i := 0; i < len(files)-maxStateBackups; i++ {
			if err := os.Remove(files[i]); err != nil {
				sm.logger.Warn("Failed to remove old backup", zap.String("file", files[i]), zap.Error(err))
			}
		}
	}

	return nil
}

func sortFilesByModTime(files []string) {
	sort.Slice(files, func(i, j int) bool {
		info1, _ := os.Stat(files[i])
		info2, _ := os.Stat(files[j])
		return info1.ModTime().Before(info2.ModTime())
	})
}

func (sm *StateManager) LoadState() (*NodeState, error) {
	sm.stateMutex.RLock()
	defer sm.stateMutex.RUnlock()

	path := filepath.Join(sm.config.DataDir, stateFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	var state NodeState
	if err := json.Unmarshal(data, &state); err != nil {
		if recoveredState, err := sm.tryRecoverState(); err == nil {
			return recoveredState, nil
		}
		return nil, fmt.Errorf("failed to unmarshal state: %w", err)
	}

	if state.Version != version {
		sm.logger.Warn("State version mismatch", zap.String("saved", state.Version), zap.String("current", version))
	}

	return &state, nil
}

func (sm *StateManager) tryRecoverState() (*NodeState, error) {
	files, err := filepath.Glob(filepath.Join(sm.config.DataDir, stateBackupPrefix+"*.json"))
	if err != nil {
		return nil, fmt.Errorf("failed to list backup files: %w", err)
	}

	if len(files) == 0 {
		return nil, errors.New("no backup files available")
	}

	sortFilesByModTime(files)
	for i := len(files) - 1; i >= 0; i-- {
		data, err := os.ReadFile(files[i])
		if err != nil {
			continue
		}

		var state NodeState
		if err := json.Unmarshal(data, &state); err == nil {
			sm.logger.Info("Recovered state from backup", zap.String("file", files[i]))
			return &state, nil
		}
	}

	return nil, errors.New("no valid backup found")
}

func (sm *StateManager) PeriodicSave(ctx context.Context, h host.Host, dht *kaddht.IpfsDHT) {
	ticker := time.NewTicker(stateSaveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := sm.SaveState(h, dht); err != nil {
				sm.logger.Error("Failed to save state", zap.Error(err))
				time.Sleep(stateRetryInterval)
				if err := sm.SaveState(h, dht); err != nil {
					sm.logger.Error("Retry failed to save state", zap.Error(err))
				}
			}
		}
	}
}

type DiscoveryManager struct {
	host      host.Host
	dht       *kaddht.IpfsDHT
	mdns      mdns.Service
	discovery *routingdiscovery.RoutingDiscovery
	logger    *zap.Logger
}

func NewDiscoveryManager(h host.Host, dht *kaddht.IpfsDHT, logger *zap.Logger) *DiscoveryManager {
	return &DiscoveryManager{
		host:      h,
		dht:       dht,
		mdns:      mdns.NewMdnsService(h, "krelay", &mdnsNotifee{h: h, logger: logger}),
		discovery: routingdiscovery.NewRoutingDiscovery(dht),
		logger:    logger,
	}
}

func (dm *DiscoveryManager) Start(ctx context.Context) {
	if err := dm.mdns.Start(); err != nil {
		dm.logger.Error("Failed to start mDNS", zap.Error(err))
	}

	go dm.advertiseService(ctx)
	go dm.findPeers(ctx)
}

func (dm *DiscoveryManager) advertiseService(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			util.Advertise(ctx, dm.discovery, "krelay-service")
		}
	}
}

func (dm *DiscoveryManager) findPeers(ctx context.Context) {
	ticker := time.NewTicker(time.Nanosecond)
	defer ticker.Stop()

	for {peerChan, err := dm.discovery.FindPeers(ctx, "krelay-service")
			if err != nil {
				dm.logger.Info("Failed to find peers", zap.Error(err))
				continue
			}

			for p := range peerChan {
				if p.ID == dm.host.ID() {
					continue
				}
				dm.host.Peerstore().AddAddrs(p.ID, p.Addrs, peerstore.TempAddrTTL)
				dm.logger.Info("Discovered peer", zap.String("peer", p.ID.String()))
			}

	}
}

type KademliaRelay struct {
	host      host.Host
	dht       *kaddht.IpfsDHT
	discovery *DiscoveryManager
	relay     *relay.Relay
	config    *Config
	logger    *zap.Logger
	ctx       context.Context
	cancel    context.CancelFunc
	apiServer *http.Server
	stateMgr  *StateManager
	connStats struct {
		total atomic.Int32
		relay atomic.Int32
	}
}

func NewKademliaRelay(ctx context.Context, cfg *Config, logger *zap.Logger) (*KademliaRelay, error) {
	ctx, cancel := context.WithCancel(ctx)

	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	stateMgr := NewStateManager(cfg, logger)
	savedState, err := stateMgr.LoadState()
	if err != nil {
		logger.Warn("Failed to load previous state", zap.Error(err))
	}

	priv, err := loadOrCreatePrivateKey(cfg.PrivateKeyPath)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("private key error: %w", err)
	}

	connMgr, err := connmgr.NewConnManager(100, 1000, connmgr.WithGracePeriod(time.Minute))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("connection manager error: %w", err)
	}

	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(cfg.ListenAddrs...),
		libp2p.NATPortMap(),
		libp2p.EnableNATService(),
		libp2p.ConnectionManager(connMgr),
		libp2p.EnableHolePunching(),
	}

	if cfg.EnableRelay {
		opts = append(opts, libp2p.EnableRelay())
	}

	h, err := libp2p.New(opts...)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("host creation failed: %w", err)
	}

	if savedState != nil {
		for _, addrStr := range savedState.ListenAddrs {
			ma, err := multiaddr.NewMultiaddr(addrStr)
			if err == nil {
				h.Peerstore().AddAddr(h.ID(), ma, peerstore.PermanentAddrTTL)
			}
		}
	}

	dht, err := kaddht.New(ctx, h, kaddht.Mode(kaddht.ModeServer))
	if err != nil {
		cancel()
		h.Close()
		return nil, fmt.Errorf("DHT initialization failed: %w", err)
	}

	if err := dht.Bootstrap(ctx); err != nil {
		cancel()
		h.Close()
		return nil, fmt.Errorf("DHT bootstrap failed: %w", err)
	}

	if savedState != nil {
		go func() {
			for _, peerIDStr := range savedState.ConnectedPeers {
				if p, err := peer.Decode(peerIDStr); err == nil {
					if addrs := h.Peerstore().Addrs(p); len(addrs) > 0 {
						h.Connect(ctx, peer.AddrInfo{
							ID:    p,
							Addrs: addrs,
						})
					}
				}
			}
		}()
	}

	if cfg.EnableAutoRelay {
		_, err = autorelay.NewAutoRelay(h, autorelay.WithPeerSource(func(ctx context.Context, numPeers int) <-chan peer.AddrInfo {
			r := make(chan peer.AddrInfo)
			go func() {
				defer close(r)
				for _, peerID := range h.Peerstore().Peers() {
					if len(h.Peerstore().Addrs(peerID)) > 0 {
						select {
						case r <- peer.AddrInfo{ID: peerID, Addrs: h.Peerstore().Addrs(peerID)}:
						case <-ctx.Done():
							return
						}
					}
				}
			}()
			return r
		}))
		if err != nil {
			cancel()
			h.Close()
			return nil, fmt.Errorf("autorelay setup failed: %w", err)
		}
	}

	var relaySvc *relay.Relay
	if cfg.EnableRelay {
		relaySvc, err = relay.New(h)
		if err != nil {
			cancel()
			h.Close()
			return nil, fmt.Errorf("relay service failed: %w", err)
		}
	}

	node := &KademliaRelay{
		host:     h,
		dht:      dht,
		config:   cfg,
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
		stateMgr: stateMgr,
		relay:    relaySvc,
	}

	node.discovery = NewDiscoveryManager(h, dht, logger)

	h.Network().Notify(&network.NotifyBundle{
		ConnectedF:    node.onConnected,
		DisconnectedF: node.onDisconnected,
	})

	node.apiServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.APIPort),
		Handler: node.setupAPI(),
	}

	go node.stateMgr.PeriodicSave(ctx, h, dht)

	return node, nil
}

func (kr *KademliaRelay) Start() error {
	kr.logger.Info("Starting Kademlia relay",
		zap.String("id", kr.host.ID().String()),
		zap.Any("addresses", kr.host.Addrs()),
		zap.String("version", version),
	)

	kr.discovery.Start(kr.ctx)
	go kr.connectBootstrapPeers()

	go func() {
		kr.logger.Info("API server starting", zap.String("addr", kr.apiServer.Addr))
		if err := kr.apiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			kr.logger.Error("API server failed", zap.Error(err))
		}
	}()

	return nil
}

func (kr *KademliaRelay) connectBootstrapPeers() {
	var eg errgroup.Group

	for _, addr := range kr.config.BootstrapPeers {
		addr := addr
		eg.Go(func() error {
			ma, err := multiaddr.NewMultiaddr(addr)
			if err != nil {
				kr.logger.Error("Invalid bootstrap address", zap.String("addr", addr), zap.Error(err))
				return nil
			}

			pi, err := peer.AddrInfoFromP2pAddr(ma)
			if err != nil {
				kr.logger.Error("Failed to parse bootstrap peer", zap.String("addr", addr), zap.Error(err))
				return nil
			}

			ctx, cancel := context.WithTimeout(kr.ctx, 10*time.Second)
			defer cancel()

			if err := kr.host.Connect(ctx, *pi); err != nil {
				kr.logger.Warn("Bootstrap connection failed",
					zap.String("peer", pi.ID.String()),
					zap.Error(err),
				)
				return nil
			}

			kr.logger.Info("Connected to bootstrap peer",
				zap.String("peer", pi.ID.String()),
			)
			kr.host.ConnManager().Protect(pi.ID, "bootstrap")
			return nil
		})
	}

	if err := eg.Wait(); err != nil {
		kr.logger.Error("Error connecting to bootstrap peers", zap.Error(err))
	}
}

func (kr *KademliaRelay) Stop() error {
	kr.logger.Info("Stopping Kademlia relay")

	if err := kr.stateMgr.SaveState(kr.host, kr.dht); err != nil {
		kr.logger.Error("Failed to save state", zap.Error(err))
	}

	kr.cancel()

	if err := kr.dht.Close(); err != nil {
		kr.logger.Error("DHT shutdown error", zap.Error(err))
	}

	if err := kr.host.Close(); err != nil {
		kr.logger.Error("Host shutdown error", zap.Error(err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := kr.apiServer.Shutdown(ctx); err != nil {
		kr.logger.Error("API server shutdown error", zap.Error(err))
	}

	kr.logger.Info("Kademlia relay stopped")
	return nil
}

func (kr *KademliaRelay) onConnected(_ network.Network, c network.Conn) {
	peerID := c.RemotePeer()
	direction := c.Stat().Direction

	kr.connStats.total.Add(1)
	if direction == network.DirInbound && kr.relay != nil {
		kr.connStats.relay.Add(1)
	}

	kr.logger.Info("Connected to peer",
		zap.String("peer", peerID.String()),
		zap.String("direction", direction.String()),
	)
}

func (kr *KademliaRelay) onDisconnected(_ network.Network, c network.Conn) {
	peerID := c.RemotePeer()
	direction := c.Stat().Direction

	kr.connStats.total.Add(-1)
	if direction == network.DirInbound && kr.relay != nil {
		kr.connStats.relay.Add(-1)
	}

	kr.logger.Info("Disconnected from peer",
		zap.String("peer", peerID.String()),
	)
}

func (kr *KademliaRelay) setupAPI() *mux.Router {
	r := mux.NewRouter()

	r.HandleFunc("/api/v1/peers", kr.listPeersHandler).Methods("GET")
	r.HandleFunc("/api/v1/peers/{peerID}", kr.connectPeerHandler).Methods("POST")
	r.HandleFunc("/api/v1/info", kr.nodeInfoHandler).Methods("GET")
	r.HandleFunc("/api/v1/relay", kr.relayStatusHandler).Methods("GET")
	r.HandleFunc("/api/v1/config", kr.getConfigHandler).Methods("GET")
	r.HandleFunc("/api/v1/config", kr.updateConfigHandler).Methods("PUT")
	r.HandleFunc("/api/v1/save", kr.saveStateHandler).Methods("POST")

	return r
}

func (kr *KademliaRelay) listPeersHandler(w http.ResponseWriter, r *http.Request) {
	peers := kr.host.Network().Peers()
	peerInfos := make([]peer.AddrInfo, 0, len(peers))
	
	for _, p := range peers {
		peerInfos = append(peerInfos, peer.AddrInfo{
			ID:    p,
			Addrs: kr.host.Peerstore().Addrs(p),
		})
	}

	respondWithJSON(w, http.StatusOK, peerInfos)
}

func (kr *KademliaRelay) connectPeerHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	peerIDStr := vars["peerID"]

	pid, err := peer.Decode(peerIDStr)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "invalid peer ID")
		return
	}

	addrs := kr.host.Peerstore().Addrs(pid)
	if len(addrs) == 0 {
		respondWithError(w, http.StatusNotFound, "no addresses found for peer")
		return
	}

	err = kr.host.Connect(kr.ctx, peer.AddrInfo{
		ID:    pid,
		Addrs: addrs,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "connected"})
}

func (kr *KademliaRelay) nodeInfoHandler(w http.ResponseWriter, r *http.Request) {
	info := map[string]interface{}{
		"id":         kr.host.ID().String(),
		"addresses":  kr.host.Addrs(),
		"protocols":  kr.host.Mux().Protocols(),
		"peersCount": len(kr.host.Network().Peers()),
		"relay": map[string]interface{}{
			"enabled":      kr.config.EnableRelay,
			"connections":  kr.connStats.relay.Load(),
		},
		"dht": map[string]interface{}{
			"routingTableSize": kr.dht.RoutingTable().Size(),
		},
		"version": version,
	}

	respondWithJSON(w, http.StatusOK, info)
}

func (kr *KademliaRelay) relayStatusHandler(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"enabled":       kr.config.EnableRelay,
		"autoRelay":     kr.config.EnableAutoRelay,
		"activeRelays":  kr.connStats.relay.Load(),
		"isRelay":       kr.relay != nil,
	}

	respondWithJSON(w, http.StatusOK, status)
}

func (kr *KademliaRelay) getConfigHandler(w http.ResponseWriter, r *http.Request) {
	respondWithJSON(w, http.StatusOK, kr.config)
}

func (kr *KademliaRelay) updateConfigHandler(w http.ResponseWriter, r *http.Request) {
	var newConfig Config
	if err := json.NewDecoder(r.Body).Decode(&newConfig); err != nil {
		respondWithError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if newConfig.APIPort <= 0 || newConfig.APIPort > 65535 {
		respondWithError(w, http.StatusBadRequest, "invalid API port")
		return
	}

	kr.config.APIPort = newConfig.APIPort
	kr.config.EnableRelay = newConfig.EnableRelay
	kr.config.EnableAutoRelay = newConfig.EnableAutoRelay

	if err := kr.config.Save(filepath.Join(kr.config.DataDir, configFileName)); err != nil {
		respondWithError(w, http.StatusInternalServerError, "failed to save config")
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "config updated"})
}

func (kr *KademliaRelay) saveStateHandler(w http.ResponseWriter, r *http.Request) {
	if err := kr.stateMgr.SaveState(kr.host, kr.dht); err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]string{"status": "state saved"})
}

func multiaddrListToStrings(addrs []multiaddr.Multiaddr) []string {
	strs := make([]string, len(addrs))
	for i, addr := range addrs {
		strs[i] = addr.String()
	}
	return strs
}

func respondWithError(w http.ResponseWriter, code int, message string) {
	respondWithJSON(w, code, map[string]string{"error": message})
}

func respondWithJSON(w http.ResponseWriter, code int, payload interface{}) {
	response, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(response)
}

func loadOrCreatePrivateKey(path string) (crypto.PrivKey, error) {
	if _, err := os.Stat(path); err == nil {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read private key: %w", err)
		}
		return crypto.UnmarshalPrivateKey(data)
	}

	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("failed to generate private key: %w", err)
	}

	data, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return nil, fmt.Errorf("failed to write private key: %w", err)
	}

	return priv, nil
}

type mdnsNotifee struct {
	h      host.Host
	logger *zap.Logger
}

func (n *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	n.logger.Info("Discovered peer via mDNS", zap.String("peer", pi.ID.String()))
	
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	
	if err := n.h.Connect(ctx, pi); err != nil {
		n.logger.Error("Failed to connect to discovered peer", 
			zap.String("peer", pi.ID.String()),
			zap.Error(err),
		)
	}
}

func DefaultConfig() *Config {
	return &Config{
		ListenAddrs: []string{
			"/ip4/0.0.0.0/tcp/4001",
			"/ip6/::/tcp/4001",
			"/ip4/0.0.0.0/udp/4001/quic",
			"/ip6/::/udp/4001/quic",
		},
		BootstrapPeers: []string{
			"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
			"/dnsaddr/bootstrap.libp2p.io/p2p/QmQCU2EcMqAqQPR2i9bChDtGNJchTbq5TbXJJ16u19uLTa",
		},
		EnableRelay:     true,
		EnableAutoRelay: true,
		APIPort:         5000,
		PrivateKeyPath:  "identity.key",
		DataDir:        "data",
	}
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg := DefaultConfig()
			if err := cfg.Save(path); err != nil {
				return nil, fmt.Errorf("failed to save default config: %w", err)
			}
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	return &cfg, nil
}

func (c *Config) Save(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}
	return nil
}
func main(){
	Init()
}
//export
func Init() {
	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("Failed to initialize logger: %v", err)
	}
	defer logger.Sync()

	configPath := "config.json"
	cfg, err := LoadConfig(configPath)
	if err != nil {
		logger.Fatal("Config load failed", zap.Error(err))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, err := NewKademliaRelay(ctx, cfg, logger)
	if err != nil {
		logger.Fatal("Node creation failed", zap.Error(err))
	}

	if err := node.Start(); err != nil {
		logger.Fatal("Node start failed", zap.Error(err))
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	if err := node.Stop(); err != nil {
		logger.Error("Shutdown error", zap.Error(err))
	}
}
