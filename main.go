package main

import (
	"encoding/binary"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
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
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	routingdiscovery "github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/libp2p/go-libp2p/p2p/host/autorelay"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/transport/tcp"
	quic "github.com/libp2p/go-libp2p/p2p/transport/quic"
	"github.com/libp2p/go-libp2p/p2p/transport/websocket"
	kaddht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// Constants
const (
	stateFileName          = "krelay-state.json"
	stateBackupPrefix      = "krelay-state-"
	configFileName         = "config.json"
	maxStateBackups        = 5
	stateSaveInterval      = 5 * time.Minute
	stateSaveTimeout       = 10 * time.Second
	stateRetryInterval     = 1 * time.Minute
	peerstoreCleanInterval = 1 * time.Hour
	peerstoreTTL           = 24 * time.Hour
	version                = "1.2.0"
	negotiationProtocol    = "/krelay/negotiation/1.0.0"
	defaultMaxConnections  = 1000
)

// ConnectionPriority defines connection priority levels
type ConnectionPriority int

const (
	LowPriority ConnectionPriority = iota
	MediumPriority
	HighPriority
)

// Config represents node configuration
type Config struct {
	ListenAddrs              []string             `json:"listenAddrs"`
	BootstrapPeers           []string             `json:"bootstrapPeers"`
	EnableRelay              bool                 `json:"enableRelay"`
	EnableAutoRelay          bool                 `json:"enableAutoRelay"`
	APIPort                  int                  `json:"apiPort"`
	PrivateKeyPath           string               `json:"privateKeyPath"`
	DataDir                  string               `json:"dataDir"`
	PeerstoreCleanupEnabled  bool                 `json:"peerstoreCleanupEnabled"`
	PeerstoreCleanupInterval string               `json:"peerstoreCleanupInterval"`
	DefaultBridges           [][]string           `json:"defaultBridges"`
	ProtocolCapabilities     []ProtocolCapability `json:"protocolCapabilities"`
	MaxConnections           int                  `json:"maxConnections"`
	PubSub                   PubSubConfig         `json:"pubsub"`
}

type PubSubConfig struct {
	Enabled       bool     `json:"enabled"`
	DefaultTopics []string `json:"defaultTopics"`
}

type ProtocolCapability struct {
	ProtocolID string   `json:"protocolId"`
	Version    string   `json:"version"`
	Priority   int      `json:"priority"`
	Features   []string `json:"features"`
}

type NodeState struct {
	PeerID         string   `json:"peerId"`
	ListenAddrs    []string `json:"listenAddrs"`
	ConnectedPeers []string `json:"connectedPeers"`
	DHTBuckets     []string `json:"dhtBuckets"`
	LastUpdated    int64    `json:"lastUpdated"`
	Version        string   `json:"version"`
}


type PubSubManager struct {
    host       host.Host
    ps         *pubsub.PubSub
    logger     *zap.Logger
    topics     map[string]*pubsub.Topic
    topicsMx   sync.RWMutex
    subs       map[string]*pubsub.Subscription
    cancelFuncs map[string]context.CancelFunc
}
type PubSubMessage struct {
    From    peer.ID
    Topic   string
    Content []byte
    Seq     int64  // Changed to int64
}


func NewPubSubManager(ctx context.Context, h host.Host, logger *zap.Logger) (*PubSubManager, error) {
	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("failed to create pubsub: %w", err)
	}

	return &PubSubManager{
		host:   h,
		ps:     ps,
		logger: logger,
		topics: make(map[string]*pubsub.Topic),
		subs:   make(map[string]*pubsub.Subscription),
	}, nil
}

func (pm *PubSubManager) JoinTopic(topic string) error {
	pm.topicsMx.Lock()
	defer pm.topicsMx.Unlock()

	if _, exists := pm.topics[topic]; exists {
		return nil
	}

	t, err := pm.ps.Join(topic)
	if err != nil {
		return fmt.Errorf("failed to join topic %s: %w", topic, err)
	}

	pm.topics[topic] = t
	pm.logger.Info("Joined pubsub topic", zap.String("topic", topic))
	return nil
}

func (pm *PubSubManager) LeaveTopic(topic string) error {
	pm.topicsMx.Lock()
	defer pm.topicsMx.Unlock()

	t, exists := pm.topics[topic]
	if !exists {
		return nil
	}

	if err := t.Close(); err != nil {
		return fmt.Errorf("failed to close topic %s: %w", topic, err)
	}

	delete(pm.topics, topic)

	if sub, exists := pm.subs[topic]; exists {
		sub.Cancel()
		delete(pm.subs, topic)
	}

	pm.logger.Info("Left pubsub topic", zap.String("topic", topic))
	return nil
}

func (pm *PubSubManager) Publish(topic string, data []byte) error {
	pm.topicsMx.RLock()
	defer pm.topicsMx.RUnlock()

	t, exists := pm.topics[topic]
	if !exists {
		return fmt.Errorf("not subscribed to topic %s", topic)
	}

	return t.Publish(context.Background(), data)
}

func (pm *PubSubManager) Subscribe(topic string, handler func(*PubSubMessage)) error {
    pm.topicsMx.Lock()
    defer pm.topicsMx.Unlock()

    if _, exists := pm.subs[topic]; exists {
        return nil
    }

    if err := pm.JoinTopic(topic); err != nil {
        return err
    }

    t := pm.topics[topic]
    sub, err := t.Subscribe()
    if err != nil {
        return fmt.Errorf("failed to subscribe to topic %s: %w", topic, err)
    }

    ctx, cancel := context.WithCancel(context.Background())
    pm.subs[topic] = sub
    pm.cancelFuncs[topic] = cancel

    go func() {
        for {
            msg, err := sub.Next(ctx)
            if err != nil {
                if errors.Is(err, context.Canceled) {
                    return
                }
                pm.logger.Error("Subscription error",
                    zap.String("topic", topic),
                    zap.Error(err))
                continue
            }

            // Handle sequence number conversion safely
            seqBytes := msg.GetSeqno()
            var seqNo uint64
            if len(seqBytes) >= 8 {
                seqNo = binary.BigEndian.Uint64(seqBytes)
            } else {
                // Handle case where sequence number is not 8 bytes
                pm.logger.Warn("Invalid sequence number length",
                    zap.Int("length", len(seqBytes)))
                seqNo = 0
            }

            handler(&PubSubMessage{
                From:    msg.GetFrom(),
                Topic:   topic,
                Content: msg.GetData(),
                Seq:     int64(seqNo),
            })
        }
    }()

    pm.logger.Info("Subscribed to pubsub topic", zap.String("topic", topic))
    return nil
}
func (pm *PubSubManager) ListTopics() []string {
	pm.topicsMx.RLock()
	defer pm.topicsMx.RUnlock()

	topics := make([]string, 0, len(pm.topics))
	for topic := range pm.topics {
		topics = append(topics, topic)
	}
	return topics
}

func (pm *PubSubManager) GetTopicPeers(topic string) []peer.ID {
	pm.topicsMx.RLock()
	defer pm.topicsMx.RUnlock()

	t, exists := pm.topics[topic]
	if !exists {
		return nil
	}
	return t.ListPeers()
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
		PeerID:         h.ID().String(),
		ListenAddrs:    multiAddrsToStrings(h.Addrs()),
		ConnectedPeers: peerIDs,
		DHTBuckets:     buckets,
		LastUpdated:    time.Now().Unix(),
		Version:        version,
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
		sort.Slice(files, func(i, j int) bool {
			info1, _ := os.Stat(files[i])
			info2, _ := os.Stat(files[j])
			return info1.ModTime().Before(info2.ModTime())
		})
		for i := 0; i < len(files)-maxStateBackups; i++ {
			if err := os.Remove(files[i]); err != nil {
				sm.logger.Warn("Failed to remove old backup",
					zap.String("file", files[i]),
					zap.Error(err))
			}
		}
	}

	return nil
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
		sm.logger.Warn("State version mismatch",
			zap.String("saved", state.Version),
			zap.String("current", version))
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

	sort.Slice(files, func(i, j int) bool {
		info1, _ := os.Stat(files[i])
		info2, _ := os.Stat(files[j])
		return info1.ModTime().Before(info2.ModTime())
	})

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

type BridgeMapping struct {
	ProtocolA      protocol.ID
	ProtocolB      protocol.ID
	HandlerA       network.StreamHandler
	HandlerB       network.StreamHandler
	Active         bool
	SuccessCount   uint64
	FailureCount   uint64
	AvgLatency     time.Duration
	LastUsed       time.Time
}

type ProtocolBridge struct {
	host         host.Host
	logger       *zap.Logger
	bridges      map[string]*BridgeMapping
	bridgesLock  sync.RWMutex
	capabilities []ProtocolCapability
}

func NewProtocolBridge(h host.Host, logger *zap.Logger) *ProtocolBridge {
	return &ProtocolBridge{
		host:    h,
		logger:  logger,
		bridges: make(map[string]*BridgeMapping),
	}
}

func (pb *ProtocolBridge) AddBridge(protoA, protoB protocol.ID) error {
	key := fmt.Sprintf("%s-%s", protoA, protoB)

	pb.bridgesLock.Lock()
	defer pb.bridgesLock.Unlock()

	if _, exists := pb.bridges[key]; exists {
		return fmt.Errorf("bridge between %s and %s already exists", protoA, protoB)
	}

	mapping := &BridgeMapping{
		ProtocolA: protoA,
		ProtocolB: protoB,
		Active:    true,
	}

	mapping.HandlerA = func(s network.Stream) {
		pb.handleStreamWithNegotiation(s, protoB)
	}
	mapping.HandlerB = func(s network.Stream) {
		pb.handleStreamWithNegotiation(s, protoA)
	}

	pb.host.SetStreamHandler(protoA, mapping.HandlerA)
	pb.host.SetStreamHandler(protoB, mapping.HandlerB)

	pb.bridges[key] = mapping
	pb.logger.Info("Added protocol bridge",
		zap.String("protocolA", string(protoA)),
		zap.String("protocolB", string(protoB)))

	return nil
}

func (pb *ProtocolBridge) handleStreamWithNegotiation(inStream network.Stream, targetProto protocol.ID) {
	defer inStream.Close()

	peerID := inStream.Conn().RemotePeer()
	pb.logger.Debug("Bridging stream with negotiation",
		zap.String("peer", peerID.String()),
		zap.String("from", string(inStream.Protocol())),
		zap.String("to", string(targetProto)))

	outStream, err := pb.host.NewStream(context.Background(), peerID, targetProto)
	if err == nil {
		pb.pipeStreams(inStream, outStream)
		pb.recordBridgeSuccess(inStream.Protocol(), targetProto)
		return
	}

	negotiatedProto, err := pb.negotiateProtocol(inStream, targetProto)
	if err != nil {
		pb.logger.Error("Protocol negotiation failed",
			zap.Error(err),
			zap.String("originalProto", string(inStream.Protocol())),
			zap.String("targetProto", string(targetProto)))
		pb.recordBridgeFailure(inStream.Protocol(), targetProto)
		return
	}

	outStream, err = pb.host.NewStream(context.Background(), peerID, negotiatedProto)
	if err != nil {
		pb.logger.Error("Failed to open negotiated stream",
			zap.Error(err),
			zap.String("negotiatedProto", string(negotiatedProto)))
		pb.recordBridgeFailure(inStream.Protocol(), targetProto)
		return
	}
	defer outStream.Close()

	pb.recordBridgeSuccess(inStream.Protocol(), negotiatedProto)
	pb.pipeStreams(inStream, outStream)
}

func (pb *ProtocolBridge) negotiateProtocol(s network.Stream, targetProto protocol.ID) (protocol.ID, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	peerID := s.Conn().RemotePeer()
	negStream, err := pb.host.NewStream(ctx, peerID, protocol.ID(negotiationProtocol))
	if err != nil {
		return "", fmt.Errorf("failed to open negotiation stream: %w", err)
	}
	defer negStream.Close()

	encoder := json.NewEncoder(negStream)
	ourCaps := pb.getCapabilitiesForFamily(targetProto)
	if err := encoder.Encode(ourCaps); err != nil {
		return "", fmt.Errorf("failed to send capabilities: %w", err)
	}

	var peerCaps []ProtocolCapability
	decoder := json.NewDecoder(negStream)
	if err := decoder.Decode(&peerCaps); err != nil {
		return "", fmt.Errorf("failed to receive capabilities: %w", err)
	}

	mutual := pb.findMutualProtocols(ourCaps, peerCaps)
	if len(mutual) == 0 {
		return "", errors.New("no mutually supported protocols")
	}

	selected := pb.selectBestProtocol(mutual)

	if _, err := negStream.Write([]byte(selected.ProtocolID)); err != nil {
		return "", fmt.Errorf("failed to acknowledge protocol: %w", err)
	}

	return protocol.ID(selected.ProtocolID), nil
}

func (pb *ProtocolBridge) getCapabilitiesForFamily(proto protocol.ID) []ProtocolCapability {
	var caps []ProtocolCapability
	for _, c := range pb.capabilities {
		if isProtocolInFamily(protocol.ID(c.ProtocolID), proto) {
			caps = append(caps, c)
		}
	}
	return caps
}

func isProtocolInFamily(checkProto, familyProto protocol.ID) bool {
	checkParts := strings.Split(string(checkProto), "/")
	familyParts := strings.Split(string(familyProto), "/")

	if len(checkParts) < 2 || len(familyParts) < 2 {
		return false
	}

	return checkParts[1] == familyParts[1]
}

func (pb *ProtocolBridge) findMutualProtocols(local, remote []ProtocolCapability) []ProtocolCapability {
	var mutual []ProtocolCapability

	for _, l := range local {
		for _, r := range remote {
			if l.ProtocolID == r.ProtocolID {
				mutual = append(mutual, l)
				break
			}
		}
	}

	return mutual
}

func (pb *ProtocolBridge) selectBestProtocol(protos []ProtocolCapability) ProtocolCapability {
	sort.Slice(protos, func(i, j int) bool {
		if protos[i].Priority != protos[j].Priority {
			return protos[i].Priority > protos[j].Priority
		}
		return protos[i].Version > protos[j].Version
	})

	return protos[0]
}

func (pb *ProtocolBridge) pipeStreams(a, b network.Stream) {
	pb.bridgesLock.RLock()
	defer pb.bridgesLock.RUnlock()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if _, err := io.Copy(b, a); err != nil {
			pb.logger.Debug("Bridge copy error",
				zap.String("direction", "a->b"),
				zap.Error(err))
		}
		b.CloseWrite()
	}()

	go func() {
		defer wg.Done()
		if _, err := io.Copy(a, b); err != nil {
			pb.logger.Debug("Bridge copy error",
				zap.String("direction", "b->a"),
				zap.Error(err))
		}
		a.CloseWrite()
	}()

	wg.Wait()
}

func (pb *ProtocolBridge) recordBridgeSuccess(srcProto, dstProto protocol.ID) {
	key := fmt.Sprintf("%s-%s", srcProto, dstProto)

	pb.bridgesLock.Lock()
	defer pb.bridgesLock.Unlock()

	if bridge, exists := pb.bridges[key]; exists {
		bridge.SuccessCount++
		bridge.LastUsed = time.Now()
	}
}

func (pb *ProtocolBridge) recordBridgeFailure(srcProto, dstProto protocol.ID) {
	key := fmt.Sprintf("%s-%s", srcProto, dstProto)

	pb.bridgesLock.Lock()
	defer pb.bridgesLock.Unlock()

	if bridge, exists := pb.bridges[key]; exists {
		bridge.FailureCount++
	}
}

func (pb *ProtocolBridge) RemoveBridge(protoA, protoB protocol.ID) error {
	key := fmt.Sprintf("%s-%s", protoA, protoB)

	pb.bridgesLock.Lock()
	defer pb.bridgesLock.Unlock()

	_, exists := pb.bridges[key]
	if !exists {
		return fmt.Errorf("bridge between %s and %s not found", protoA, protoB)
	}

	pb.host.RemoveStreamHandler(protoA)
	pb.host.RemoveStreamHandler(protoB)
	delete(pb.bridges, key)

	pb.logger.Info("Removed protocol bridge",
		zap.String("protocolA", string(protoA)),
		zap.String("protocolB", string(protoB)))

	return nil
}

func (pb *ProtocolBridge) ListBridges() []string {
	pb.bridgesLock.RLock()
	defer pb.bridgesLock.RUnlock()

	bridges := make([]string, 0, len(pb.bridges))
	for key := range pb.bridges {
		bridges = append(bridges, key)
	}
	return bridges
}

func (pb *ProtocolBridge) GetStatistics() map[string]interface{} {
	pb.bridgesLock.RLock()
	defer pb.bridgesLock.RUnlock()

	stats := make(map[string]interface{})
	for key, bridge := range pb.bridges {
		stats[key] = map[string]interface{}{
			"successCount":   bridge.SuccessCount,
			"failureCount":   bridge.FailureCount,
			"lastUsed":      bridge.LastUsed.Format(time.RFC3339),
			"active":        bridge.Active,
		}
	}
	return stats
}

func (pb *ProtocolBridge) NegotiateWithPeer(ctx context.Context, pid peer.ID, protos []protocol.ID) (protocol.ID, error) {
	s, err := pb.host.NewStream(ctx, pid, protocol.ID(negotiationProtocol))
	if err != nil {
		return "", fmt.Errorf("failed to open negotiation stream: %w", err)
	}
	defer s.Close()

	var ourCaps []ProtocolCapability
	for _, p := range protos {
		caps := pb.getCapabilitiesForFamily(p)
		ourCaps = append(ourCaps, caps...)
	}

	encoder := json.NewEncoder(s)
	if err := encoder.Encode(ourCaps); err != nil {
		return "", fmt.Errorf("failed to send capabilities: %w", err)
	}

	var peerCaps []ProtocolCapability
	decoder := json.NewDecoder(s)
	if err := decoder.Decode(&peerCaps); err != nil {
		return "", fmt.Errorf("failed to receive capabilities: %w", err)
	}

	mutual := pb.findMutualProtocols(ourCaps, peerCaps)
	if len(mutual) == 0 {
		return "", errors.New("no mutually supported protocols")
	}

	selected := pb.selectBestProtocol(mutual)

	buf := make([]byte, 1024)
	n, err := s.Read(buf)
	if err != nil {
		return "", fmt.Errorf("failed to read protocol confirmation: %w", err)
	}

	if string(buf[:n]) != selected.ProtocolID {
		return "", fmt.Errorf("protocol mismatch: expected %s, got %s", selected.ProtocolID, string(buf[:n]))
	}

	return protocol.ID(selected.ProtocolID), nil
}

type ConnectionScore struct {
	PeerID           peer.ID
	LastActivity     time.Time
	BytesTransferred uint64
	Latency         time.Duration
	Stability       float64
	Priority        ConnectionPriority
}

type ConnectionManager struct {
	host              host.Host
	logger            *zap.Logger
	maxConnections    int
	priorityPeers     map[peer.ID]ConnectionPriority
	connectionLimiter chan struct{}
	maintenanceTicker *time.Ticker
	metrics           struct {
		totalConnections    atomic.Int32
		inboundConnections atomic.Int32
		outboundConnections atomic.Int32
	}
	scoreMutex sync.Mutex
}

func NewConnectionManager(h host.Host, maxConns int, logger *zap.Logger) *ConnectionManager {
	if maxConns <= 0 {
		maxConns = defaultMaxConnections
	}

	cm := &ConnectionManager{
		host:            h,
		logger:          logger,
		maxConnections:  maxConns,
		priorityPeers:   make(map[peer.ID]ConnectionPriority),
		connectionLimiter: make(chan struct{}, maxConns),
		maintenanceTicker: time.NewTicker(5 * time.Minute),
	}

	for i := 0; i < maxConns; i++ {
		cm.connectionLimiter <- struct{}{}
	}

	return cm
}

func (cm *ConnectionManager) Connect(ctx context.Context, pi peer.AddrInfo, priority ConnectionPriority) error {
	if cm.host.Network().Connectedness(pi.ID) == network.Connected {
		return nil
	}

	select {
	case <-cm.connectionLimiter:
	default:
		if priority >= HighPriority {
			cm.dropLowestPriorityConnection()
			<-cm.connectionLimiter
		} else {
			return fmt.Errorf("connection limit reached")
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	err := cm.host.Connect(ctx, pi)
	if err != nil {
		cm.connectionLimiter <- struct{}{}
		return err
	}

	cm.metrics.totalConnections.Add(1)
	if priority > LowPriority {
		cm.scoreMutex.Lock()
		cm.priorityPeers[pi.ID] = priority
		cm.scoreMutex.Unlock()
	}

	conns := cm.host.Network().ConnsToPeer(pi.ID)
	for _, conn := range conns {
		if conn.Stat().Direction == network.DirInbound {
			cm.metrics.inboundConnections.Add(1)
		} else {
			cm.metrics.outboundConnections.Add(1)
		}
	}

	return nil
}

func (cm *ConnectionManager) dropLowestPriorityConnection() {
	peers := cm.host.Network().Peers()
	if len(peers) == 0 {
		return
	}

	scores := cm.scoreConnections()
	var lowestScore *ConnectionScore

	for _, score := range scores {
		if lowestScore == nil || score.Priority < lowestScore.Priority ||
			(score.Priority == lowestScore.Priority && score.LastActivity.Before(lowestScore.LastActivity)) {
			lowestScore = &score
		}
	}

	if lowestScore != nil {
		cm.host.Network().ClosePeer(lowestScore.PeerID)
		cm.metrics.totalConnections.Add(-1)
	}
}

func (cm *ConnectionManager) scoreConnections() map[peer.ID]ConnectionScore {
	scores := make(map[peer.ID]ConnectionScore)

	cm.scoreMutex.Lock()
	defer cm.scoreMutex.Unlock()

	for _, p := range cm.host.Network().Peers() {
		conns := cm.host.Network().ConnsToPeer(p)
		var totalBytes uint64
		var lastActivity time.Time

		for _, c := range conns {
			stat := c.Stat()
			totalBytes += uint64(stat.NumStreams)
			if stat.Opened.After(lastActivity) {
				lastActivity = stat.Opened
			}
		}

		priority, exists := cm.priorityPeers[p]
		if !exists {
			priority = LowPriority
		}

		scores[p] = ConnectionScore{
			PeerID:           p,
			LastActivity:     lastActivity,
			BytesTransferred: totalBytes,
			Priority:        priority,
			Stability:       calculateStability(p, cm.host),
		}
	}

	return scores
}

func calculateStability(p peer.ID, h host.Host) float64 {
	conns := h.Network().ConnsToPeer(p)
	if len(conns) == 0 {
		return 0
	}

	var totalDuration time.Duration
	for _, c := range conns {
		totalDuration += time.Since(c.Stat().Opened)
	}
	avgDuration := totalDuration / time.Duration(len(conns))

	stability := float64(avgDuration) / float64(time.Hour)
	if stability > 1 {
		stability = 1
	}
	return stability
}

func (cm *ConnectionManager) adjustLimits() {
	inbound := cm.metrics.inboundConnections.Load()
	outbound := cm.metrics.outboundConnections.Load()

	newLimit := cm.maxConnections

	if inbound > outbound*2 {
		newLimit = int(float64(cm.maxConnections) * 1.2)
	}

	if outbound > inbound*2 {
		newLimit = int(float64(cm.maxConnections) * 1.1)
	}

	if newLimit > cm.maxConnections {
		for i := 0; i < newLimit-cm.maxConnections; i++ {
			select {
			case cm.connectionLimiter <- struct{}{}:
			default:
				break
			}
		}
	} else if newLimit < cm.maxConnections {
		for i := 0; i < cm.maxConnections-newLimit; i++ {
			select {
			case <-cm.connectionLimiter:
			default:
				break
			}
		}
	}

	cm.maxConnections = newLimit
}

func (cm *ConnectionManager) maintainConnections() {
	scores := cm.scoreConnections()
	currentCount := len(scores)

	if currentCount >= cm.maxConnections*9/10 {
		sorted := make([]ConnectionScore, 0, len(scores))
		for _, score := range scores {
			sorted = append(sorted, score)
		}

		sort.Slice(sorted, func(i, j int) bool {
			if sorted[i].Priority != sorted[j].Priority {
				return sorted[i].Priority > sorted[j].Priority
			}
			return sorted[i].BytesTransferred > sorted[j].BytesTransferred
		})

		toTrim := currentCount - int(float64(cm.maxConnections)*0.8)
		trimmed := 0
		for i := len(sorted) - 1; i >= 0 && trimmed < toTrim; i-- {
			if sorted[i].Priority == LowPriority {
				cm.host.Network().ClosePeer(sorted[i].PeerID)
				trimmed++
			}
		}
	}
}

func (cm *ConnectionManager) runMaintenance(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-cm.maintenanceTicker.C:
			cm.adjustLimits()
			cm.maintainConnections()
		}
	}
}

func (cm *ConnectionManager) IsConnected(p peer.ID) bool {

	return cm.host.Network().Connectedness(p) == network.Connected
}

func (cm *ConnectionManager) GetConnectionStats() map[string]interface{} {
	return map[string]interface{}{
		"total":    cm.metrics.totalConnections.Load(),
		"inbound":  cm.metrics.inboundConnections.Load(),
		"outbound": cm.metrics.outboundConnections.Load(),
		"limit":    cm.maxConnections,
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
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			peerChan, err := dm.discovery.FindPeers(ctx, "krelay-service")
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

type KademliaRelay struct {
	host        host.Host
	dht         *kaddht.IpfsDHT
	discovery   *DiscoveryManager
	bridge      *ProtocolBridge
	relay       *relay.Relay
	pubsub      *PubSubManager
	config      *Config
	logger      *zap.Logger
	ctx         context.Context
	cancel      context.CancelFunc
	apiServer   *http.Server
	stateMgr    *StateManager
	connManager *ConnectionManager
	lastSeen    sync.Map
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

	connmg, err := connmgr.NewConnManager(
		cfg.MaxConnections/10,
		cfg.MaxConnections,
		connmgr.WithGracePeriod(time.Minute),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create connection manager: %w", err)
	}

	opts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(cfg.ListenAddrs...),
		libp2p.NATPortMap(),
		libp2p.ConnectionManager(connmg),
		libp2p.EnableNATService(),
		libp2p.EnableHolePunching(),
		libp2p.ForceReachabilityPublic(),
		libp2p.Transport(websocket.New),
		libp2p.Transport(tcp.NewTCPTransport),
		libp2p.Transport(quic.NewTransport),
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

	node := &KademliaRelay{
		host:     h,
		dht:      dht,
		config:   cfg,
		logger:   logger,
		ctx:      ctx,
		cancel:   cancel,
		stateMgr: stateMgr,
		bridge:   NewProtocolBridge(h, logger),
	}

	pubsubMgr, err := NewPubSubManager(ctx, h, logger)
	if err != nil {
		cancel()
		h.Close()
		return nil, fmt.Errorf("failed to initialize pubsub: %w", err)
	}
	node.pubsub = pubsubMgr

	node.connManager = NewConnectionManager(h, cfg.MaxConnections, logger)
	node.discovery = NewDiscoveryManager(h, dht, logger)

	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(n network.Network, c network.Conn) {
			node.onConnected(n, c)
			node.connManager.metrics.totalConnections.Add(1)
			if c.Stat().Direction == network.DirInbound {
				node.connManager.metrics.inboundConnections.Add(1)
			} else {
				node.connManager.metrics.outboundConnections.Add(1)
			}
		},
		DisconnectedF: func(n network.Network, c network.Conn) {
			node.onDisconnected(n, c)
			node.connManager.metrics.totalConnections.Add(-1)
			if c.Stat().Direction == network.DirInbound {
				node.connManager.metrics.inboundConnections.Add(-1)
			} else {
				node.connManager.metrics.outboundConnections.Add(-1)
			}
		},
	})

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

	if cfg.EnableRelay {
		node.relay, err = relay.New(h)
		if err != nil {
			cancel()
			h.Close()
			return nil, fmt.Errorf("relay service failed: %w", err)
		}
	}

	node.apiServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.APIPort),
		Handler: node.setupAPI(),
	}

	go node.stateMgr.PeriodicSave(ctx, h, dht)
	go node.connManager.runMaintenance(ctx)

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

	if kr.config.PeerstoreCleanupEnabled {
		interval, err := time.ParseDuration(kr.config.PeerstoreCleanupInterval)
		if err != nil {
			interval = peerstoreCleanInterval
		}
		go kr.schedulePeerstoreCleanup(interval)
	}

	if kr.config.PubSub.Enabled {
		for _, topic := range kr.config.PubSub.DefaultTopics {
			if err := kr.pubsub.JoinTopic(topic); err != nil {
				kr.logger.Error("Failed to join default pubsub topic",
					zap.String("topic", topic),
					zap.Error(err))
			} else {
				kr.logger.Info("Joined default pubsub topic",
					zap.String("topic", topic))
			}
		}
	}

	go func() {
		kr.logger.Info("API server starting", zap.String("addr", kr.apiServer.Addr))
		if err := kr.apiServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			kr.logger.Error("API server failed", zap.Error(err))
		}
	}()

	return nil
}

func (kr *KademliaRelay) schedulePeerstoreCleanup(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-kr.ctx.Done():
			return
		case <-ticker.C:
			kr.cleanupPeerstore()
		}
	}
}

func (kr *KademliaRelay) cleanupPeerstore() {
	connectedPeers := kr.host.Network().Peers()
	allPeers := kr.host.Peerstore().Peers()
	now := time.Now()

	var cleaned int
	for _, p := range allPeers {
		if containsPeer(connectedPeers, p) {
			continue
		}

		if kr.host.ConnManager().IsProtected(p, "") {
			continue
		}

		if lastSeen, ok := kr.lastSeen.Load(p); ok {
			if now.Sub(lastSeen.(time.Time)) > peerstoreTTL {
				addrs := kr.host.Peerstore().Addrs(p)
				for _, addr := range addrs {
					kr.host.Peerstore().AddAddr(p, addr, peerstore.TempAddrTTL)
				}

				protos, err := kr.host.Peerstore().GetProtocols(p)
				if err == nil {
					for _, proto := range protos {
						kr.host.Peerstore().RemoveProtocols(p, proto)
					}
				}

				cleaned++
			}
		}
	}

	if cleaned > 0 {
		kr.logger.Info("Cleaned peerstore",
			zap.Int("peers", cleaned),
			zap.Int("remaining", len(allPeers)))
	}
}

func containsPeer(peers []peer.ID, target peer.ID) bool {
	for _, p := range peers {
		if p == target {
			return true
		}
	}
	return false
}

func (kr *KademliaRelay) connectBootstrapPeers() {
	var eg errgroup.Group
	sem := make(chan struct{}, 4)

	for _, addr := range kr.config.BootstrapPeers {
		addr := addr
		sem <- struct{}{}

		eg.Go(func() error {
			defer func() { <-sem }()

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

			if err := kr.connManager.Connect(kr.ctx, *pi, HighPriority); err != nil {
				kr.logger.Warn("Bootstrap connection failed",
					zap.String("peer", pi.ID.String()),
					zap.Error(err),
				)
				return nil
			}

			kr.logger.Info("Connected to bootstrap peer",
				zap.String("peer", pi.ID.String()),
			)
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

	kr.lastSeen.Store(peerID, time.Now())
	kr.logger.Info("Connected to peer",
		zap.String("peer", peerID.String()),
		zap.String("direction", direction.String()),
	)
}

func (kr *KademliaRelay) onDisconnected(_ network.Network, c network.Conn) {
	peerID := c.RemotePeer()
	direction := c.Stat().Direction

	kr.lastSeen.Store(peerID, time.Now())
	kr.logger.Info("Disconnected from peer",
		zap.String("peer", peerID.String()),
		zap.String("direction", direction.String()),
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
	r.HandleFunc("/api/v1/peerstore", kr.peerstoreInfoHandler).Methods("GET")
	r.HandleFunc("/api/v1/bridge", kr.listBridgesHandler).Methods("GET")
	r.HandleFunc("/api/v1/bridge", kr.addBridgeHandler).Methods("POST")
	r.HandleFunc("/api/v1/bridge/{protoA}/{protoB}", kr.removeBridgeHandler).Methods("DELETE")
	r.HandleFunc("/api/v1/protocols", kr.listSupportedProtocols).Methods("GET")
	r.HandleFunc("/api/v1/protocols/negotiate", kr.manualNegotiate).Methods("POST")
	r.HandleFunc("/api/v1/bridge/stats", kr.bridgeStatistics).Methods("GET")
	r.HandleFunc("/api/v1/connections", kr.connectionStatsHandler).Methods("GET")
	r.HandleFunc("/api/v1/connections/limit", kr.setConnectionLimitHandler).Methods("POST")
	r.HandleFunc("/api/v1/pubsub/topics", kr.listPubSubTopics).Methods("GET")
	r.HandleFunc("/api/v1/pubsub/topics/{topic}", kr.joinPubSubTopic).Methods("POST")
	r.HandleFunc("/api/v1/pubsub/topics/{topic}", kr.leavePubSubTopic).Methods("DELETE")
	r.HandleFunc("/api/v1/pubsub/topics/{topic}/publish", kr.publishToTopic).Methods("POST")
	r.HandleFunc("/api/v1/pubsub/topics/{topic}/subscribe", kr.subscribeToTopic).Methods("POST")
	r.HandleFunc("/api/v1/pubsub/topics/{topic}/peers", kr.listTopicPeers).Methods("GET")

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

	err = kr.connManager.Connect(kr.ctx, peer.AddrInfo{
		ID:    pid,
		Addrs: addrs,
	}, MediumPriority)
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
			"enabled":     kr.config.EnableRelay,
			"connections": kr.connManager.metrics.inboundConnections.Load(),
		},
		"dht": map[string]interface{}{
			"routingTableSize": kr.dht.RoutingTable().Size(),
		},
		"version": version,
		"pubsub": map[string]interface{}{
			"enabled":  kr.config.PubSub.Enabled,
			"topics":   kr.pubsub.ListTopics(),
			"protocol": "gossipsub",
		},
	}

	respondWithJSON(w, http.StatusOK, info)
}

func (kr *KademliaRelay) relayStatusHandler(w http.ResponseWriter, r *http.Request) {
	status := map[string]interface{}{
		"enabled":      kr.config.EnableRelay,
		"autoRelay":    kr.config.EnableAutoRelay,
		"activeRelays": kr.connManager.metrics.inboundConnections.Load(),
		"isRelay":      kr.relay != nil,
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
	kr.config.PeerstoreCleanupEnabled = newConfig.PeerstoreCleanupEnabled
	kr.config.PeerstoreCleanupInterval = newConfig.PeerstoreCleanupInterval
	kr.config.ProtocolCapabilities = newConfig.ProtocolCapabilities
	kr.config.MaxConnections = newConfig.MaxConnections

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

func (kr *KademliaRelay) peerstoreInfoHandler(w http.ResponseWriter, r *http.Request) {
	peers := kr.host.Peerstore().Peers()
	connected := kr.host.Network().Peers()

	info := map[string]interface{}{
		"totalPeers":     len(peers),
		"connectedPeers": len(connected),
		"cleanupEnabled": kr.config.PeerstoreCleanupEnabled,
	}

	respondWithJSON(w, http.StatusOK, info)
}

func (kr *KademliaRelay) listBridgesHandler(w http.ResponseWriter, r *http.Request) {
	bridges := kr.bridge.ListBridges()
	respondWithJSON(w, http.StatusOK, bridges)
}

func (kr *KademliaRelay) addBridgeHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProtocolA string `json:"protocolA"`
		ProtocolB string `json:"protocolB"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := kr.bridge.AddBridge(protocol.ID(req.ProtocolA), protocol.ID(req.ProtocolB)); err != nil {
		respondWithError(w, http.StatusBadRequest, err.Error())
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "bridge added"})
}

func (kr *KademliaRelay) removeBridgeHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	protoA := protocol.ID(vars["protoA"])
	protoB := protocol.ID(vars["protoB"])

	if err := kr.bridge.RemoveBridge(protoA, protoB); err != nil {
		respondWithError(w, http.StatusNotFound, err.Error())
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "bridge removed"})
}

func (kr *KademliaRelay) listSupportedProtocols(w http.ResponseWriter, r *http.Request) {
	respondWithJSON(w, http.StatusOK, kr.bridge.capabilities)
}

func (kr *KademliaRelay) manualNegotiate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeerID    string   `json:"peerId"`
		Protocols []string `json:"protocols"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	pid, err := peer.Decode(req.PeerID)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "invalid peer ID")
		return
	}

	var protos []protocol.ID
	for _, p := range req.Protocols {
		protos = append(protos, protocol.ID(p))
	}

	selected, err := kr.bridge.NegotiateWithPeer(context.Background(), pid, protos)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{
		"negotiatedProtocol": string(selected),
	})
}

func (kr *KademliaRelay) bridgeStatistics(w http.ResponseWriter, r *http.Request) {
	stats := kr.bridge.GetStatistics()
	respondWithJSON(w, http.StatusOK, stats)
}

func (kr *KademliaRelay) connectionStatsHandler(w http.ResponseWriter, r *http.Request) {
	stats := kr.connManager.GetConnectionStats()
	respondWithJSON(w, http.StatusOK, stats)
}

func (kr *KademliaRelay) setConnectionLimitHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Limit int `json:"limit"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Limit <= 0 {
		respondWithError(w, http.StatusBadRequest, "limit must be positive")
		return
	}

	kr.config.MaxConnections = req.Limit
	kr.connManager.maxConnections = req.Limit
	respondWithJSON(w, http.StatusOK, map[string]string{"status": "limit updated"})
}

func (kr *KademliaRelay) listPubSubTopics(w http.ResponseWriter, r *http.Request) {
	topics := kr.pubsub.ListTopics()
	respondWithJSON(w, http.StatusOK, topics)
}

func (kr *KademliaRelay) joinPubSubTopic(w http.ResponseWriter, r *http.Request) {
	topic := mux.Vars(r)["topic"]
	if err := kr.pubsub.JoinTopic(topic); err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]string{"status": "joined topic"})
}

func (kr *KademliaRelay) leavePubSubTopic(w http.ResponseWriter, r *http.Request) {
	topic := mux.Vars(r)["topic"]
	if err := kr.pubsub.LeaveTopic(topic); err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondWithJSON(w, http.StatusOK, map[string]string{"status": "left topic"})
}

func (kr *KademliaRelay) publishToTopic(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string `json:"message"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondWithError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Message == "" {
		respondWithError(w, http.StatusBadRequest, "message cannot be empty")
		return
	}

	topic := mux.Vars(r)["topic"]
	if err := kr.pubsub.Publish(topic, []byte(req.Message)); err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "message published"})
}

func (kr *KademliaRelay) subscribeToTopic(w http.ResponseWriter, r *http.Request) {
	topic := mux.Vars(r)["topic"]

	err := kr.pubsub.Subscribe(topic, func(msg *PubSubMessage) {
		kr.logger.Info("Received pubsub message",
			zap.String("topic", msg.Topic),
			zap.String("from", msg.From.String()),
			zap.ByteString("content", msg.Content),
			zap.Int64("seq", msg.Seq))
	})

	if err != nil {
		respondWithError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondWithJSON(w, http.StatusOK, map[string]string{"status": "subscribed to topic"})
}

func (kr *KademliaRelay) listTopicPeers(w http.ResponseWriter, r *http.Request) {
	topic := mux.Vars(r)["topic"]
	peers := kr.pubsub.GetTopicPeers(topic)

	peerIDs := make([]string, len(peers))
	for i, p := range peers {
		peerIDs[i] = p.String()
	}

	respondWithJSON(w, http.StatusOK, peerIDs)
}

func multiAddrsToStrings(addrs []multiaddr.Multiaddr) []string {
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
	response, err := json.Marshal(payload)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to marshal response"}`))
		return
	}

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
			"/dnsaddr/bootstrap.libp2p.io/p2p/QmcZf59bWwK5XFi76CZX8cbJ4BhTzzA3gU1ZjYZcYW3dwt",
			"/ip4/104.131.131.82/tcp/4001/p2p/QmaCpDMGvV2BGHeYERUEnRQAwe3N8SzbUtfsmvsqQLuvuJ",
		},
		EnableRelay:              true,
		EnableAutoRelay:          true,
		APIPort:                  5000,
		PrivateKeyPath:           "identity.key",
		DataDir:                  "data",
		PeerstoreCleanupEnabled:  true,
		PeerstoreCleanupInterval: "1h",
		MaxConnections:           defaultMaxConnections,
		DefaultBridges: [][]string{
			{"/chat/1.0.0", "/chat/2.0.0"},
			{"/file-transfer/1.0", "/file-transfer/2.0"},
		},
		ProtocolCapabilities: []ProtocolCapability{
			{
				ProtocolID: "/chat/1.0.0",
				Version:    "1.0.0",
				Priority:   1,
				Features:   []string{"basic-messaging"},
			},
			{
				ProtocolID: "/chat/2.0.0",
				Version:    "2.0.0",
				Priority:   10,
				Features:   []string{"basic-messaging", "encryption", "metadata"},
			},
			{
				ProtocolID: "/file-transfer/1.0",
				Version:    "1.0",
				Priority:   5,
				Features:   []string{"small-files"},
			},
			{
				ProtocolID: "/file-transfer/2.0",
				Version:    "2.0",
				Priority:   8,
				Features:   []string{"small-files", "resume", "checksum"},
			},
		},
		PubSub: PubSubConfig{
			Enabled: true,
			DefaultTopics: []string{
				"krelay-global",
				"krelay-announcements",
			},
		},
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

func main() {
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

	node.bridge.capabilities = cfg.ProtocolCapabilities

	for _, bridgePair := range cfg.DefaultBridges {
		if len(bridgePair) == 2 {
			if err := node.bridge.AddBridge(
				protocol.ID(bridgePair[0]),
				protocol.ID(bridgePair[1]),
			); err != nil {
				logger.Warn("Failed to add default bridge",
					zap.String("protocolA", bridgePair[0]),
					zap.String("protocolB", bridgePair[1]),
					zap.Error(err))
			}
		}
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
