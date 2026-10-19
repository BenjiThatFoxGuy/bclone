// Package api provides the Telegram MTProto client wrapper and types
// for the bclone Telegram backend.
package api

import "time"

// ChatType classifies a Telegram chat.
type ChatType int

const (
	ChatTypeUser    ChatType = iota // private / DM
	ChatTypeGroup                   // basic group or supergroup
	ChatTypeChannel                 // broadcast channel
)

// ChatInfo holds cached metadata about a Telegram chat.
type ChatInfo struct {
	ID              int64    `json:"id"`
	Title           string   `json:"title,omitempty"`
	Type            ChatType `json:"type"`
	RestrictSaving  bool     `json:"restrict_saving"`
	IsAdmin         bool     `json:"is_admin"`
	AccessHash      int64    `json:"access_hash"`
	CachedAt        time.Time `json:"cached_at"`
}

// CachedFile holds metadata about a file-bearing message in a chat,
// persisted in the on-disk metadata cache.
type CachedFile struct {
	MessageID    int       `json:"msg_id"`
	ChatID       int64     `json:"chat_id"`
	FileName     string    `json:"name"`
	Size         int64     `json:"size"`
	MimeType     string    `json:"mime"`
	Date         time.Time `json:"date"`
	FileRef      []byte    `json:"file_ref,omitempty"`
	AccessHash   int64     `json:"access_hash"`
	FileID       int64     `json:"file_id"`
	DCid         int       `json:"dc_id"`
	ThumbSize    string    `json:"thumb_size,omitempty"` // for photos: size type (e.g. "y")
	IsDocument   bool      `json:"is_doc"`
	IsPhoto      bool      `json:"is_photo"`
	IsVideo      bool      `json:"is_video"`
	PhotoID      int64     `json:"photo_id,omitempty"` // for photos, the photo ID
}

// ChatCache holds the file metadata cache for a single chat.
type ChatCache struct {
	ChatID       int64                `json:"chat_id"`
	Files        map[string]*CachedFile `json:"files"`          // key: constructed filename
	LastMsgID    int                  `json:"last_msg_id"`     // highest message ID we've scanned
	UpdatedAt    time.Time            `json:"updated_at"`
}
