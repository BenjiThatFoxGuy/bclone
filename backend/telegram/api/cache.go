package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Cache provides thread-safe metadata caching for Telegram chats and files.
type Cache struct {
	mu       sync.Mutex
	dir      string                    // on-disk persistence directory ("" = memory only)
	chats    map[int64]*ChatCache      // chat ID -> file metadata
	chatInfo map[int64]*ChatInfo       // chat ID -> chat metadata
	chatTTL  time.Duration             // how long chat metadata stays fresh
}

// NewCache creates a new metadata cache. If dir is non-empty, the cache
// will be persisted to disk.
func NewCache(dir string) *Cache {
	return &Cache{
		dir:      dir,
		chats:    make(map[int64]*ChatCache),
		chatInfo: make(map[int64]*ChatInfo),
		chatTTL:  5 * time.Minute,
	}
}

// GetChatCache returns the cached file listing for a chat, or nil if not cached.
func (c *Cache) GetChatCache(chatID int64) *ChatCache {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chats[chatID]
}

// SetChatCache stores or replaces the file listing cache for a chat.
func (c *Cache) SetChatCache(chatID int64, cc *ChatCache) {
	c.mu.Lock()
	c.chats[chatID] = cc
	c.mu.Unlock()
}

// GetChatInfo returns cached chat metadata, or nil if absent or stale.
func (c *Cache) GetChatInfo(chatID int64) *ChatInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	info := c.chatInfo[chatID]
	if info == nil {
		return nil
	}
	if time.Since(info.CachedAt) > c.chatTTL {
		return nil // stale
	}
	return info
}

// SetChatInfo stores chat metadata.
func (c *Cache) SetChatInfo(info *ChatInfo) {
	c.mu.Lock()
	info.CachedAt = time.Now()
	c.chatInfo[info.ID] = info
	c.mu.Unlock()
}

// UpdateFileRef updates the file reference for a specific file in the cache.
func (c *Cache) UpdateFileRef(chatID int64, fileName string, newRef []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cc := c.chats[chatID]
	if cc == nil {
		return
	}
	if f, ok := cc.Files[fileName]; ok {
		f.FileRef = newRef
	}
}

// AddFile adds or updates a single file entry in the cache.
func (c *Cache) AddFile(chatID int64, cf *CachedFile) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cc := c.chats[chatID]
	if cc == nil {
		cc = &ChatCache{
			ChatID: chatID,
			Files:  make(map[string]*CachedFile),
		}
		c.chats[chatID] = cc
	}
	cc.Files[cf.FileName] = cf
	if cf.MessageID > cc.LastMsgID {
		cc.LastMsgID = cf.MessageID
	}
	cc.UpdatedAt = time.Now()
}

// RemoveFile removes a file entry from the cache.
func (c *Cache) RemoveFile(chatID int64, fileName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cc := c.chats[chatID]
	if cc == nil {
		return
	}
	delete(cc.Files, fileName)
}

// diskCacheFile is the on-disk format for the full cache.
type diskCacheFile struct {
	Chats    map[int64]*ChatCache `json:"chats"`
	ChatInfo map[int64]*ChatInfo  `json:"chat_info"`
	SavedAt  time.Time            `json:"saved_at"`
}

// Load reads the cache from disk if available.
func (c *Cache) Load() {
	if c.dir == "" {
		return
	}
	path := filepath.Join(c.dir, "telegram_cache.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return // no cache file yet
	}
	var cached diskCacheFile
	if err := json.Unmarshal(data, &cached); err != nil {
		return // corrupt, start fresh
	}
	c.mu.Lock()
	if cached.Chats != nil {
		c.chats = cached.Chats
	}
	if cached.ChatInfo != nil {
		c.chatInfo = cached.ChatInfo
	}
	c.mu.Unlock()
}

// Save writes the cache to disk.
func (c *Cache) Save() {
	if c.dir == "" {
		return
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return
	}
	c.mu.Lock()
	cached := diskCacheFile{
		Chats:    c.chats,
		ChatInfo: c.chatInfo,
		SavedAt:  time.Now(),
	}
	c.mu.Unlock()

	data, err := json.Marshal(cached)
	if err != nil {
		return
	}
	path := filepath.Join(c.dir, "telegram_cache.json")
	_ = os.WriteFile(path, data, 0o644)
}

// InvalidateChatInfo removes cached chat metadata, forcing a refresh on next access.
func (c *Cache) InvalidateChatInfo(chatID int64) {
	c.mu.Lock()
	delete(c.chatInfo, chatID)
	c.mu.Unlock()
}

// LookupFile finds a file in the cache by its constructed filename.
func (c *Cache) LookupFile(chatID int64, fileName string) *CachedFile {
	c.mu.Lock()
	defer c.mu.Unlock()
	cc := c.chats[chatID]
	if cc == nil {
		return nil
	}
	return cc.Files[fileName]
}

// ListFiles returns all cached files for a chat. The caller must not
// modify the returned map.
func (c *Cache) ListFiles(chatID int64) map[string]*CachedFile {
	c.mu.Lock()
	defer c.mu.Unlock()
	cc := c.chats[chatID]
	if cc == nil {
		return nil
	}
	return cc.Files
}

// LastMessageID returns the highest scanned message ID for a chat.
func (c *Cache) LastMessageID(chatID int64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	cc := c.chats[chatID]
	if cc == nil {
		return 0
	}
	return cc.LastMsgID
}

// FormatCacheDir expands ~ in the cache directory path.
func FormatCacheDir(dir string) string {
	if len(dir) > 1 && dir[:2] == "~/" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, dir[2:])
		}
	}
	return dir
}

// MakeFileName constructs the display filename for a Telegram file,
// following the convention from the spec: use the telegram-provided
// filename when available, otherwise synthesize one.
// When a collision is detected (nameSet already contains the name),
// the message ID is appended as a suffix.
func MakeFileName(original string, msgID int, date time.Time, isPhoto, isVideo bool, nameSet map[string]bool) string {
	name := original
	if name == "" {
		ts := date.Format("2006-01-02_15-04-05")
		switch {
		case isPhoto:
			name = fmt.Sprintf("photo_%s.jpg", ts)
		case isVideo:
			name = fmt.Sprintf("video_%s.mp4", ts)
		default:
			name = fmt.Sprintf("file_%s_%d", ts, msgID)
		}
	}
	if nameSet[name] {
		ext := filepath.Ext(name)
		base := name[:len(name)-len(ext)]
		name = fmt.Sprintf("%s-%d%s", base, msgID, ext)
	}
	nameSet[name] = true
	return name
}
