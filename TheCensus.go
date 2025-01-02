package buntdb

import (
    "context"
    "encoding/binary"
    "fmt"
    "io"
    "net"
    "path/filepath"
    "sync"
    "time"

    "github.com/hashicorp/go-hclog"
    "github.com/hashicorp/raft"
    raftboltdb "github.com/hashicorp/raft-boltdb"
)

// ConsensusConfig holds configuration for the Raft consensus system
type ConsensusConfig struct {
    // NodeID is the unique identifier for this node in the cluster
    NodeID string

    // RaftDir is the directory where Raft logs and snapshots are stored
    RaftDir string

    // BindAddr is the address:port where Raft will bind for cluster communication
    BindAddr string

    // Bootstrap indicates if this node should bootstrap the cluster
    Bootstrap bool

    // RetainLogs indicates how many Raft logs to retain
    RetainLogs uint64

    // Logger is the logging interface
    Logger hclog.Logger
}

// consensusManager handles the Raft consensus implementation
type consensusManager struct {
    mu sync.RWMutex

    // Core components
    raft       *raft.Raft
    fsm        *buntFSM
    transport  raft.Transport
    config     *ConsensusConfig
    shutdownCh chan struct{}

    // Parent database reference
    db *DB

    // Logger
    logger hclog.Logger
}

// FSM command types
const (
    cmdSet uint32 = iota
    cmdDelete
    cmdDeleteAll
)

// FSM command structure
type command struct {
    Op       uint32
    Key      string
    Value    string
    ExpireAt *time.Time
}

// Initialize creates and starts the consensus system
func (cm *consensusManager) Initialize(config *ConsensusConfig) error {
    cm.mu.Lock()
    defer cm.mu.Unlock()

    if err := cm.validateConfig(config); err != nil {
        return fmt.Errorf("invalid consensus config: %w", err)
    }

    // Initialize the Raft FSM
    cm.fsm = &buntFSM{
        db:     cm.db,
        logger: cm.logger,
    }

    // Create Raft configuration
    raftConfig := raft.DefaultConfig()
    raftConfig.LocalID = raft.ServerID(config.NodeID)
    raftConfig.Logger = cm.logger

    // Setup transport
    addr, err := net.ResolveTCPAddr("tcp", config.BindAddr)
    if err != nil {
        return fmt.Errorf("failed to resolve bind address: %w", err)
    }

    transport, err := raft.NewTCPTransport(
        config.BindAddr,
        addr,
        3, // Maximum pool size
        10*time.Second,
        cm.logger,
    )
    if err != nil {
        return fmt.Errorf("failed to create transport: %w", err)
    }
    cm.transport = transport

    // Create the snapshot store
    snapshots, err := raft.NewFileSnapshotStore(
        config.RaftDir,
        3, // Retain 3 snapshots
        cm.logger,
    )
    if err != nil {
        return fmt.Errorf("failed to create snapshot store: %w", err)
    }

    // Create the log store and stable store
    logStore, err := raftboltdb.NewBoltStore(
        filepath.Join(config.RaftDir, "raft-log.db"),
    )
    if err != nil {
        return fmt.Errorf("failed to create log store: %w", err)
    }

    stableStore, err := raftboltdb.NewBoltStore(
        filepath.Join(config.RaftDir, "raft-stable.db"),
    )
    if err != nil {
        return fmt.Errorf("failed to create stable store: %w", err)
    }

    // Create Raft instance
    r, err := raft.NewRaft(
        raftConfig,
        cm.fsm,
        logStore,
        stableStore,
        snapshots,
        cm.transport,
    )
    if err != nil {
        return fmt.Errorf("failed to create raft instance: %w", err)
    }
    cm.raft = r

    // Bootstrap if configured
    if config.Bootstrap {
        configuration := raft.Configuration{
            Servers: []raft.Server{
                {
                    ID:      raft.ServerID(config.NodeID),
                    Address: cm.transport.LocalAddr(),
                },
            },
        }
        f := r.BootstrapCluster(configuration)
        if err := f.Error(); err != nil {
            return fmt.Errorf("failed to bootstrap cluster: %w", err)
        }
    }

    cm.config = config
    return nil
}

// buntFSM implements the raft.FSM interface for BuntDB
type buntFSM struct {
    db     *DB
    logger hclog.Logger
}

// Apply implements the raft.FSM interface
func (f *buntFSM) Apply(log *raft.Log) interface{} {
    var cmd command
    if err := decode(log.Data, &cmd); err != nil {
        f.logger.Error("failed to decode command", "error", err)
        return err
    }

    switch cmd.Op {
    case cmdSet:
        return f.applySet(cmd.Key, cmd.Value, cmd.ExpireAt)
    case cmdDelete:
        return f.applyDelete(cmd.Key)
    case cmdDeleteAll:
        return f.applyDeleteAll()
    default:
        err := fmt.Errorf("unknown command operation: %d", cmd.Op)
        f.logger.Error("invalid command", "error", err)
        return err
    }
}

// Snapshot implements the raft.FSM interface
func (f *buntFSM) Snapshot() (raft.FSMSnapshot, error) {
    f.db.mu.RLock()
    defer f.db.mu.RUnlock()

    // Create a new snapshot
    snap := &buntSnapshot{
        db:     f.db,
        logger: f.logger,
    }

    return snap, nil
}

// Restore implements the raft.FSM interface
func (f *buntFSM) Restore(rc io.ReadCloser) error {
    defer rc.Close()

    // Clear existing data
    if err := f.db.DeleteAll(); err != nil {
        return fmt.Errorf("failed to clear database: %w", err)
    }

    // Restore from snapshot
    decoder := newDecoder(rc)
    for {
        var cmd command
        if err := decoder.Decode(&cmd); err != nil {
            if err == io.EOF {
                break
            }
            return fmt.Errorf("failed to decode snapshot entry: %w", err)
        }

        switch cmd.Op {
        case cmdSet:
            if _, err := f.applySet(cmd.Key, cmd.Value, cmd.ExpireAt); err != nil {
                return fmt.Errorf("failed to restore set operation: %w", err)
            }
        case cmdDelete:
            if _, err := f.applyDelete(cmd.Key); err != nil {
                return fmt.Errorf("failed to restore delete operation: %w", err)
            }
        }
    }

    return nil
}

// applySet applies a Set operation to the FSM
func (f *buntFSM) applySet(key, value string, expireAt *time.Time) (interface{}, error) {
    var opts *SetOptions
    if expireAt != nil {
        opts = &SetOptions{
            Expires: true,
            TTL:     time.Until(*expireAt),
        }
    }

    return f.db.set(key, value, opts)
}

// applyDelete applies a Delete operation to the FSM
func (f *buntFSM) applyDelete(key string) (interface{}, error) {
    return f.db.delete(key)
}

// applyDeleteAll applies a DeleteAll operation to the FSM
func (f *buntFSM) applyDeleteAll() error {
    return f.db.deleteAll()
}

// Set performs a Set operation through the consensus system
func (cm *consensusManager) Set(key, value string, opts *SetOptions) (string, bool, error) {
    if cm.raft.State() != raft.Leader {
        return "", false, raft.ErrNotLeader
    }

    cmd := command{
        Op:    cmdSet,
        Key:   key,
        Value: value,
    }
    if opts != nil && opts.Expires {
        expireAt := time.Now().Add(opts.TTL)
        cmd.ExpireAt = &expireAt
    }

    data, err := encode(cmd)
    if err != nil {
        return "", false, err
    }

    future := cm.raft.Apply(data, 5*time.Second)
    if err := future.Error(); err != nil {
        return "", false, err
    }

    resp := future.Response()
    if err, ok := resp.(error); ok {
        return "", false, err
    }

    result := resp.([]interface{})
    return result[0].(string), result[1].(bool), nil
}

// Delete performs a Delete operation through the consensus system
func (cm *consensusManager) Delete(key string) (string, error) {
    if cm.raft.State() != raft.Leader {
        return "", raft.ErrNotLeader
    }

    cmd := command{
        Op:  cmdDelete,
        Key: key,
    }

    data, err := encode(cmd)
    if err != nil {
        return "", err
    }

    future := cm.raft.Apply(data, 5*time.Second)
    if err := future.Error(); err != nil {
        return "", err
    }

    resp := future.Response()
    if err, ok := resp.(error); ok {
        return "", err
    }

    return resp.(string), nil
}

// Helper functions for encoding/decoding commands
func encode(v interface{}) ([]byte, error) {
    // Implementation using encoding/gob or msgpack
    // Return serialized bytes
    return nil, nil
}

func decode(data []byte, v interface{}) error {
    // Implementation using encoding/gob or msgpack
    // Deserialize into v
    return nil
}

// State retrieval methods
func (cm *consensusManager) IsLeader() bool {
    return cm.raft.State() == raft.Leader
}

func (cm *consensusManager) GetLeader() string {
    return string(cm.raft.Leader())
}

func (cm *consensusManager) WaitForLeader(ctx context.Context) error {
    ticker := time.NewTicker(100 * time.Millisecond)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-ticker.C:
            if leader := cm.raft.Leader(); leader != "" {
                return nil
            }
        }
    }
}

// Cluster management methods
func (cm *consensusManager) AddVoter(id, addr string) error {
    if cm.raft.State() != raft.Leader {
        return raft.ErrNotLeader
    }

    future := cm.raft.AddVoter(
        raft.ServerID(id),
        raft.ServerAddress(addr),
        0,
        0,
    )
    return future.Error()
}

func (cm *consensusManager) RemoveServer(id string) error {
    if cm.raft.State() != raft.Leader {
        return raft.ErrNotLeader
    }

    future := cm.raft.RemoveServer(
        raft.ServerID(id),
        0,
        0,
    )
    return future.Error()
}

// Shutdown gracefully stops the consensus system
func (cm *consensusManager) Shutdown() error {
    cm.mu.Lock()
    defer cm.mu.Unlock()

    if cm.raft == nil {
        return nil
    }

    future := cm.raft.Shutdown()
    if err := future.Error(); err != nil {
        return fmt.Errorf("failed to shutdown raft: %w", err)
    }

    if cm.transport != nil {
        if err := cm.transport.Close(); err != nil {
            return fmt.Errorf("failed to close transport: %w", err)
        }
    }

    close(cm.shutdownCh)
    return nil
}
