package telegram

import (
	"testing"
	"time"

	"github.com/rclone/rclone/backend/telegram/api"
)

func TestParsePath(t *testing.T) {
	f := &Fs{root: ""}

	tests := []struct {
		path       string
		wantChatID int64
		wantFile   string
	}{
		{"chats/12345/test.txt", 12345, "test.txt"},
		{"chats/-1001234567890/doc.pdf", -1001234567890, "doc.pdf"},
		{"chats/12345", 12345, ""},
		{"chats/12345/", 12345, ""},
		{"invalid/path", 0, ""},
		{"", 0, ""},
		{"chats/notanumber/file.txt", 0, "file.txt"},
	}

	for _, tt := range tests {
		chatID, fileName := f.parsePath(tt.path)
		if chatID != tt.wantChatID {
			t.Errorf("parsePath(%q) chatID = %d, want %d", tt.path, chatID, tt.wantChatID)
		}
		if fileName != tt.wantFile {
			t.Errorf("parsePath(%q) fileName = %q, want %q", tt.path, fileName, tt.wantFile)
		}
	}
}

func TestParsePathWithRoot(t *testing.T) {
	f := &Fs{root: "chats/12345"}

	chatID, fileName := f.parsePath(f.fullPath("test.txt"))
	if chatID != 12345 {
		t.Errorf("chatID = %d, want 12345", chatID)
	}
	if fileName != "test.txt" {
		t.Errorf("fileName = %q, want %q", fileName, "test.txt")
	}
}

func TestFullPath(t *testing.T) {
	tests := []struct {
		root   string
		remote string
		want   string
	}{
		{"", "chats/123/file.txt", "chats/123/file.txt"},
		{"chats/123", "file.txt", "chats/123/file.txt"},
		{"chats/123", "", "chats/123"},
		{"", "", ""},
	}

	for _, tt := range tests {
		f := &Fs{root: tt.root}
		got := f.fullPath(tt.remote)
		if got != tt.want {
			t.Errorf("fullPath(%q) with root %q = %q, want %q", tt.remote, tt.root, got, tt.want)
		}
	}
}

func TestMakeFileName(t *testing.T) {
	date := time.Date(2024, 1, 15, 12, 34, 56, 0, time.UTC)
	nameSet := map[string]bool{}

	// Document with filename.
	name := api.MakeFileName("report.pdf", 100, date, false, false, nameSet)
	if name != "report.pdf" {
		t.Errorf("got %q, want %q", name, "report.pdf")
	}

	// Photo without filename.
	name = api.MakeFileName("", 101, date, true, false, nameSet)
	if name != "photo_2024-01-15_12-34-56.jpg" {
		t.Errorf("got %q, want %q", name, "photo_2024-01-15_12-34-56.jpg")
	}

	// Video without filename.
	name = api.MakeFileName("", 102, date, false, true, nameSet)
	if name != "video_2024-01-15_12-34-56.mp4" {
		t.Errorf("got %q, want %q", name, "video_2024-01-15_12-34-56.mp4")
	}

	// Collision - same photo timestamp.
	name = api.MakeFileName("", 103, date, true, false, nameSet)
	want := "photo_2024-01-15_12-34-56-103.jpg"
	if name != want {
		t.Errorf("got %q, want %q (collision handling)", name, want)
	}
}

func TestMakeFileNameCollisionWithExisting(t *testing.T) {
	nameSet := map[string]bool{"test.txt": true}

	name := api.MakeFileName("test.txt", 200, time.Now(), false, false, nameSet)
	if name != "test-200.txt" {
		t.Errorf("got %q, want %q", name, "test-200.txt")
	}
}

func TestIsChannelID(t *testing.T) {
	tests := []struct {
		id   int64
		want bool
	}{
		{-1001234567890, true},
		{-1000000000001, true},
		{-1000000000000, false}, // boundary
		{-999999999999, false},
		{12345, false},
		{0, false},
		{-12345, false},
	}

	for _, tt := range tests {
		got := api.IsChannelID(tt.id)
		if got != tt.want {
			t.Errorf("isChannelID(%d) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

func TestParseChatID(t *testing.T) {
	tests := []struct {
		s    string
		want int64
	}{
		{"12345", 12345},
		{"-1001234567890", -1001234567890},
		{"0", 0},
		{"notanumber", 0},
		{"", 0},
	}

	for _, tt := range tests {
		got := api.ParseChatID(tt.s)
		if got != tt.want {
			t.Errorf("ParseChatID(%q) = %d, want %d", tt.s, got, tt.want)
		}
	}
}

func TestDetectMime(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"test.pdf", "application/pdf"},
		{"test.jpg", "image/jpeg"},
		{"test.JPEG", "image/jpeg"},
		{"test.png", "image/png"},
		{"test.mp4", "video/mp4"},
		{"test.mp3", "audio/mpeg"},
		{"test.unknown", "application/octet-stream"},
	}

	for _, tt := range tests {
		got := api.DetectMime(tt.name)
		if got != tt.want {
			t.Errorf("detectMime(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestConfigSessionStorage(t *testing.T) {
	// New empty storage.
	s := api.NewConfigSessionStorage("")
	_, err := s.LoadSession(nil)
	if err == nil {
		t.Error("expected ErrNotFound for empty storage")
	}

	// Store and load.
	data := []byte("test-session-data")
	if err := s.StoreSession(nil, data); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}
	loaded, err := s.LoadSession(nil)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if string(loaded) != string(data) {
		t.Errorf("loaded = %q, want %q", loaded, data)
	}

	// Encode and decode round-trip.
	encoded := s.Encode()
	s2 := api.NewConfigSessionStorage(encoded)
	loaded2, err := s2.LoadSession(nil)
	if err != nil {
		t.Fatalf("LoadSession from encoded: %v", err)
	}
	if string(loaded2) != string(data) {
		t.Errorf("round-trip = %q, want %q", loaded2, data)
	}
}

func TestCacheOperations(t *testing.T) {
	cache := api.NewCache("")

	// Initially empty.
	if files := cache.ListFiles(123); files != nil {
		t.Error("expected nil for uncached chat")
	}

	// Add a file.
	cf := &api.CachedFile{
		MessageID: 1,
		ChatID:    123,
		FileName:  "test.txt",
		Size:      100,
		MimeType:  "text/plain",
		Date:      time.Now(),
	}
	cache.AddFile(123, cf)

	// Lookup.
	found := cache.LookupFile(123, "test.txt")
	if found == nil {
		t.Fatal("expected to find file")
	}
	if found.Size != 100 {
		t.Errorf("size = %d, want 100", found.Size)
	}

	// List.
	files := cache.ListFiles(123)
	if len(files) != 1 {
		t.Errorf("len(files) = %d, want 1", len(files))
	}

	// Remove.
	cache.RemoveFile(123, "test.txt")
	if found := cache.LookupFile(123, "test.txt"); found != nil {
		t.Error("expected file to be removed")
	}
}

func TestChatInfoCaching(t *testing.T) {
	cache := api.NewCache("")

	// Not cached.
	if info := cache.GetChatInfo(123); info != nil {
		t.Error("expected nil for uncached chat info")
	}

	// Set and get.
	info := &api.ChatInfo{
		ID:             123,
		Title:          "Test Chat",
		Type:           api.ChatTypeGroup,
		RestrictSaving: true,
		IsAdmin:        false,
	}
	cache.SetChatInfo(info)

	got := cache.GetChatInfo(123)
	if got == nil {
		t.Fatal("expected cached chat info")
	}
	if got.Title != "Test Chat" {
		t.Errorf("title = %q, want %q", got.Title, "Test Chat")
	}
	if !got.RestrictSaving {
		t.Error("expected RestrictSaving=true")
	}
}
