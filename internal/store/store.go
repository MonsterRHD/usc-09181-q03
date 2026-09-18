// Package store 提供观察站的 JSON 文件持久化：
// 写入采用临时文件 + 原子改名，保证隔日重新启动后未决复核、
// 版本链与下一截止等状态可完整恢复。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"example.com/09181/q003/internal/service"
)

// FileStore 以单个 JSON 文件保存全量快照。
type FileStore struct {
	path string
	mu   sync.Mutex
}

func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

func (f *FileStore) Load() (*service.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return service.NewSnapshot(), nil
		}
		return nil, fmt.Errorf("读取状态文件失败: %w", err)
	}
	snap := service.NewSnapshot()
	if len(b) == 0 {
		return snap, nil
	}
	if err := json.Unmarshal(b, snap); err != nil {
		return nil, fmt.Errorf("状态文件已损坏: %w", err)
	}
	if snap.Contracts == nil {
		snap = service.NewSnapshot()
	}
	if snap.Covenants == nil {
		snap.Covenants = map[string]*service.Covenant{}
	}
	if snap.Drawdowns == nil {
		snap.Drawdowns = map[string]*service.Drawdown{}
	}
	if snap.Filings == nil {
		snap.Filings = map[string]*service.FilingChain{}
	}
	return snap, nil
}

// Save 原子写入：先写临时文件再 rename，避免进程崩溃留下半截状态。
func (f *FileStore) Save(snap *service.Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化状态失败: %w", err)
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("写临时状态文件失败: %w", err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return fmt.Errorf("原子替换状态文件失败: %w", err)
	}
	return nil
}

// MemoryStore 内存实现，供测试使用。
type MemoryStore struct {
	mu   sync.Mutex
	Snap *service.Snapshot
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{Snap: service.NewSnapshot()} }

func (m *MemoryStore) Load() (*service.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Snap, nil
}

func (m *MemoryStore) Save(snap *service.Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Snap = snap
	return nil
}
