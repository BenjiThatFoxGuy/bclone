// Package telegram implements a backend for Telegram using the MTProto
// protocol via gotd/td. It supports both bot tokens and user sessions,
// providing read and write access to files in Telegram chats.
//
// Path layout:
//
//	telegram:chats/<chat_id>/              — list files in a chat
//	telegram:chats/<chat_id>/filename.ext  — a specific file
//
// See SPEC.md for full design details.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/telegram/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
)

const (
	// bridgeEditDelay is how long we wait after editing a caption to "."
	// before attempting to delete, giving bridge bots time to react.
	bridgeEditDelay = 2 * time.Second
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "telegram",
		Description: "Telegram (MTProto, bot or user session)",
		NewFs:       NewFs,
		Config: func(ctx context.Context, name string, m configmap.Mapper, config fs.ConfigIn) (*fs.ConfigOut, error) {
			return configHandler(ctx, name, m, config)
		},
		Options: []fs.Option{{
			Name:      "login",
			Help:      "Bot token (from @BotFather) or phone number with country code (e.g. +1234567890).\nA value containing ':' is treated as a bot token; otherwise as a phone number.",
			Sensitive: true,
			Required:  true,
		}, {
			Name:      "api_id",
			Help:      "Telegram API ID from https://my.telegram.org",
			Sensitive: true,
			Required:  true,
		}, {
			Name:      "api_hash",
			Help:      "Telegram API Hash from https://my.telegram.org",
			Sensitive: true,
			Required:  true,
		}, {
			Name:     "device_model",
			Help:     "Device model reported to Telegram.",
			Default:  "bclone",
			Advanced: true,
		}, {
			Name:     "system_version",
			Help:     "OS version reported to Telegram.",
			Default:  "",
			Advanced: true,
		}, {
			Name:     "app_version",
			Help:     "App version reported to Telegram.",
			Default:  "",
			Advanced: true,
		}, {
			Name:     "media_mode",
			Help:     "Which files to show in listings.",
			Default:  "documents",
			Examples: []fs.OptionExample{{
				Value: "documents",
				Help:  "Only files sent as documents (preserves original filename and quality).",
			}, {
				Value: "all",
				Help:  "Documents, photos, and videos.",
			}},
		}, {
			Name:     "send_as_photo",
			Help:     "Send image files as photos instead of documents.\nUses Telegram's photo mode. Other files are always sent as documents.",
			Default:  false,
			Advanced: true,
		}, {
			Name:     "restrict_saving_bypass",
			Help:     "Allow access to files in chats with content protection (restrict saving).\nOnly effective when you are not an admin - admins always have access.\nServer-side copy is never available for protected chats; rclone will reupload.",
			Default:  false,
			Advanced: true,
		}, {
			Name:     "cache_dir",
			Help:     "Directory to persist the metadata cache between runs.",
			Default:  "~/.cache/rclone/telegram",
			Advanced: true,
		}, {
			Name:      "session",
			Help:      "MTProto session data (managed automatically, do not edit).",
			Sensitive: true,
			Advanced:  true,
			Hide:      fs.OptionHideBoth,
		}},
	})
}

// configHandler implements the interactive config flow.
// Auth mode is inferred from which credentials the user supplied in the
// standard option prompts (bot_token → bot, phone → user session).
//
// The state machine:
//
//	"" (entry) → detect mode, connect, start auth
//	"bot_auth" → bot token auth completed, save session
//	"user_code_sent" → code was sent, prompt user for the code
//	"user_code_entered" → verify code, maybe prompt for 2FA
//	"user_2fa" → verify 2FA password
//	"auth_done" → save session, finish
func configHandler(ctx context.Context, name string, m configmap.Mapper, conf fs.ConfigIn) (*fs.ConfigOut, error) {
	switch conf.State {
	case "":
		// Entry point: the standard options have been collected.
		// Determine auth mode from the login field.
		login, _ := m.Get("login")
		apiIDStr, _ := m.Get("api_id")
		apiHash, _ := m.Get("api_hash")

		if login == "" {
			return nil, fmt.Errorf("login is required (bot token or phone number)")
		}
		if apiIDStr == "" || apiHash == "" {
			return nil, fmt.Errorf("api_id and api_hash are required")
		}

		apiID, err := strconv.Atoi(apiIDStr)
		if err != nil {
			return nil, fmt.Errorf("invalid api_id: %w", err)
		}

		var botToken, phone string
		if strings.Contains(login, ":") {
			botToken = login
		} else {
			phone = login
		}

		deviceModel, _ := m.Get("device_model")
		systemVersion, _ := m.Get("system_version")
		appVersion, _ := m.Get("app_version")

		if botToken != "" {
			// Bot mode: reuse existing session if present.
			sessionData, _ := m.Get("session")
			client := api.NewClient(api.ClientOptions{
				AppID:         apiID,
				AppHash:       apiHash,
				BotToken:      botToken,
				DeviceModel:   deviceModel,
				SystemVersion: systemVersion,
				AppVersion:    appVersion,
				SessionData:   sessionData,
			})
			if err := client.ConnectAndAuthBot(ctx); err != nil {
				return nil, fmt.Errorf("bot auth failed: %w", err)
			}
			m.Set("session", client.SessionData())
			return fs.ConfigGoto("auth_done")
		}

		// User mode: always start fresh to avoid AUTH_RESTART from
		// a half-completed previous attempt.
		m.Set("session", "")
		client := api.NewClient(api.ClientOptions{
			AppID:         apiID,
			AppHash:       apiHash,
			Phone:         phone,
			DeviceModel:   deviceModel,
			SystemVersion: systemVersion,
			AppVersion:    appVersion,
		})

		codeHash, err := client.ConnectAndSendCode(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to send auth code: %w", err)
		}
		// Persist the session and code hash for the next state.
		m.Set("session", client.SessionData())
		m.Set("config_code_hash", codeHash)
		return fs.ConfigInput("user_code_entered", "config_auth_code",
			"Enter the login code sent to your Telegram account.")

	case "user_code_entered":
		code := conf.Result
		codeHash, _ := m.Get("config_code_hash")
		if code == "" {
			return nil, fmt.Errorf("auth code is required")
		}

		// Reconnect with the saved session and verify the code.
		client, err := reconnectClient(ctx, m)
		if err != nil {
			return nil, err
		}

		needsPassword, err := client.SignIn(ctx, code, codeHash)
		if err != nil {
			return nil, fmt.Errorf("sign in failed: %w", err)
		}
		m.Set("session", client.SessionData())

		if needsPassword {
			// Use ConfigInput not ConfigPassword here: ConfigPassword
			// runs rclone's obscure on the result, but we need the raw
			// password for Telegram's SRP auth.
			return fs.ConfigInput("user_2fa", "config_2fa_password",
				"Two-factor authentication password required.")
		}
		return fs.ConfigGoto("auth_done")

	case "user_2fa":
		password := conf.Result
		if password == "" {
			return nil, fmt.Errorf("2FA password is required")
		}

		client, err := reconnectClient(ctx, m)
		if err != nil {
			return nil, err
		}

		if err := client.CheckPassword(ctx, password); err != nil {
			return nil, fmt.Errorf("2FA verification failed: %w", err)
		}
		m.Set("session", client.SessionData())
		return fs.ConfigGoto("auth_done")

	case "auth_done":
		// Clean up ephemeral config keys.
		m.Set("config_code_hash", "")
		fs.Infof(nil, "Telegram authentication successful")
		return nil, nil
	}
	return nil, fmt.Errorf("unknown config state %q", conf.State)
}

// reconnectClient creates a fresh Client from the saved config and connects it.
func reconnectClient(ctx context.Context, m configmap.Mapper) (*api.Client, error) {
	apiIDStr, _ := m.Get("api_id")
	apiHash, _ := m.Get("api_hash")
	login, _ := m.Get("login")
	deviceModel, _ := m.Get("device_model")
	systemVersion, _ := m.Get("system_version")
	appVersion, _ := m.Get("app_version")
	sessionData, _ := m.Get("session")

	apiID, err := strconv.Atoi(apiIDStr)
	if err != nil {
		return nil, fmt.Errorf("invalid api_id: %w", err)
	}

	var botToken, phone string
	if strings.Contains(login, ":") {
		botToken = login
	} else {
		phone = login
	}

	client := api.NewClient(api.ClientOptions{
		AppID:         apiID,
		AppHash:       apiHash,
		BotToken:      botToken,
		Phone:         phone,
		DeviceModel:   deviceModel,
		SystemVersion: systemVersion,
		AppVersion:    appVersion,
		SessionData:   sessionData,
	})

	if err := client.ConnectRaw(ctx); err != nil {
		return nil, fmt.Errorf("reconnect failed: %w", err)
	}
	return client, nil
}

// Options defines the configuration for this backend.
type Options struct {
	Login                string `config:"login"`
	ApiID                string `config:"api_id"`
	ApiHash              string `config:"api_hash"`
	DeviceModel          string `config:"device_model"`
	SystemVersion        string `config:"system_version"`
	AppVersion           string `config:"app_version"`
	MediaMode            string `config:"media_mode"`
	SendAsPhoto          bool   `config:"send_as_photo"`
	RestrictSavingBypass bool   `config:"restrict_saving_bypass"`
	CacheDir             string `config:"cache_dir"`
	Session              string `config:"session"`
}

// isBotToken returns true if the login value looks like a bot token (contains ':').
func isBotToken(login string) bool {
	return strings.Contains(login, ":")
}

// Fs represents a Telegram remote.
type Fs struct {
	name     string
	root     string
	opt      Options
	features *fs.Features
	client   *api.Client
	cache    *api.Cache
	m        configmap.Mapper // for persisting session updates
}

// NewFs constructs a new Fs from the path, container:path.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	fs.Debugf(nil, "telegram: NewFs name=%q root=%q", name, root)
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}

	apiID, err := strconv.Atoi(opt.ApiID)
	if err != nil {
		return nil, fmt.Errorf("telegram: invalid api_id: %w", err)
	}

	var botToken, phone string
	if isBotToken(opt.Login) {
		botToken = opt.Login
	} else {
		phone = opt.Login
	}

	client := api.NewClient(api.ClientOptions{
		AppID:         apiID,
		AppHash:       opt.ApiHash,
		BotToken:      botToken,
		Phone:         phone,
		DeviceModel:   opt.DeviceModel,
		SystemVersion: opt.SystemVersion,
		AppVersion:    opt.AppVersion,
		SessionData:   opt.Session,
	})

	if err := client.Connect(ctx); err != nil {
		return nil, fmt.Errorf("telegram: connect failed: %w", err)
	}

	// Persist any updated session data back to the config.
	if newSession := client.SessionData(); newSession != opt.Session {
		m.Set("session", newSession)
	}

	cacheDir := api.FormatCacheDir(opt.CacheDir)
	cache := api.NewCache(cacheDir)
	cache.Load()

	f := &Fs{
		name:   name,
		root:   strings.Trim(root, "/"),
		opt:    *opt,
		client: client,
		cache:  cache,
		m:      m,
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: false,
		DuplicateFiles:          true,
		ReadMimeType:            true,
	}).Fill(ctx, f)

	// If the root points to a specific file, return fs.ErrorIsFile.
	if f.root != "" {
		chatID, fileName := f.parsePath(f.root)
		if chatID != 0 && fileName != "" {
			if err := f.ensureChat(ctx, chatID); err == nil {
				if cf := f.cache.LookupFile(chatID, fileName); cf != nil {
					// Root is a file — adjust root to parent dir.
					f.root = path.Dir(f.root)
					return f, fs.ErrorIsFile
				}
			}
		}
	}

	return f, nil
}

// Name of the remote.
func (f *Fs) Name() string { return f.name }

// Root of the remote.
func (f *Fs) Root() string { return f.root }

// String converts this Fs to a string.
func (f *Fs) String() string { return fmt.Sprintf("telegram root '%s'", f.root) }

// Precision of the remote: Telegram message dates are to the second.
func (f *Fs) Precision() time.Duration { return time.Second }

// Hashes returns the supported hash types (none for Telegram).
func (f *Fs) Hashes() hash.Set { return hash.NewHashSet() }

// Features returns the optional features of this Fs.
func (f *Fs) Features() *fs.Features { return f.features }

// List returns entries under dir.
//
// Path routing:
//   - "chats" or "chats/<id>": list files in that chat
//   - anything else: empty (no root listing)
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	fullDir := dir
	if f.root != "" {
		if fullDir == "" {
			fullDir = f.root
		} else {
			fullDir = f.root + "/" + fullDir
		}
	}

	parts := strings.SplitN(fullDir, "/", 3)

	// Must start with "chats"
	if len(parts) == 0 || parts[0] != "chats" {
		return nil, fs.ErrorDirNotFound
	}

	// "chats" alone - we don't list chats per spec.
	if len(parts) == 1 {
		return nil, fs.ErrorDirNotFound
	}

	// "chats/<id>" - list files in chat.
	chatID := api.ParseChatID(parts[1])
	if chatID == 0 {
		return nil, fs.ErrorDirNotFound
	}

	// No deeper nesting allowed.
	if len(parts) > 2 && parts[2] != "" {
		return nil, fs.ErrorDirNotFound
	}

	if err := f.ensureChat(ctx, chatID); err != nil {
		return nil, err
	}

	files := f.cache.ListFiles(chatID)
	if files == nil {
		return fs.DirEntries{}, nil
	}

	entries := make(fs.DirEntries, 0, len(files))
	for _, cf := range files {
		if f.opt.MediaMode == "documents" && !cf.IsDocument {
			continue
		}
		remote := cf.FileName
		if dir != "" {
			// Not needed - files are flat within the chat dir.
		}
		entries = append(entries, f.newObject(remote, cf))
	}
	return entries, nil
}

// NewObject finds an object by name.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	fullPath := f.fullPath(remote)
	chatID, fileName := f.parsePath(fullPath)
	fs.Debugf(f, "NewObject: remote=%q fullPath=%q chatID=%d fileName=%q", remote, fullPath, chatID, fileName)
	if chatID == 0 || fileName == "" {
		fs.Debugf(f, "NewObject: not found (invalid path)")
		return nil, fs.ErrorObjectNotFound
	}

	if err := f.ensureChat(ctx, chatID); err != nil {
		fs.Debugf(f, "NewObject: ensureChat failed: %v", err)
		return nil, err
	}

	cf := f.cache.LookupFile(chatID, fileName)
	if cf == nil {
		fs.Debugf(f, "NewObject: not found in cache")
		return nil, fs.ErrorObjectNotFound
	}
	fs.Debugf(f, "NewObject: found in cache msgID=%d", cf.MessageID)
	return f.newObject(remote, cf), nil
}

// Put uploads a file to Telegram.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	fullPath := f.fullPath(remote)
	fs.Debugf(f, "Put: remote=%q fullPath=%q", remote, fullPath)
	chatID, fileName := f.parsePath(fullPath)
	if chatID == 0 || fileName == "" {
		return nil, fmt.Errorf("telegram: invalid path %q - must be chats/<id>/<filename>", remote)
	}

	size := src.Size()
	fs.Debugf(f, "Put: chatID=%d fileName=%q size=%d", chatID, fileName, size)
	maxSize := f.client.MaxFileSize()
	if size > maxSize {
		limit := "2 GB"
		if f.client.IsPremium() {
			limit = "4 GB"
		}
		return nil, fmt.Errorf("telegram: file too large (%d bytes) - maximum is %s", size, limit)
	}

	// Check restrict-saving for the destination chat.
	if err := f.checkWriteAccess(ctx, chatID); err != nil {
		return nil, err
	}

	var cf *api.CachedFile
	var err error

	// Send as photo if configured and the file is an image.
	if f.opt.SendAsPhoto && isImageFile(fileName) {
		cf, err = f.client.SendPhoto(ctx, chatID, fileName, in)
	} else {
		cf, err = f.client.SendDocument(ctx, chatID, fileName, size, in)
	}
	if err != nil {
		return nil, err
	}

	// Update cache with the new file.
	nameSet := f.buildNameSet(chatID)
	cf.FileName = api.MakeFileName(cf.FileName, cf.MessageID, cf.Date, cf.IsPhoto, cf.IsVideo, nameSet)
	f.cache.AddFile(chatID, cf)
	f.cache.Save()

	// Persist session.
	f.persistSession()

	return f.newObject(remote, cf), nil
}

// Mkdir is a no-op: chat "directories" are virtual.
func (f *Fs) Mkdir(ctx context.Context, dir string) error { return nil }

// Rmdir is a no-op: chat "directories" are virtual and can't be removed.
func (f *Fs) Rmdir(ctx context.Context, dir string) error { return nil }

// Copy performs a server-side copy using file reference reuse.
// Returns fs.ErrorCantCopy if not possible (e.g., restrict-saving).
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantCopy
	}

	dstChatID, dstFileName := f.parsePath(f.fullPath(remote))
	if dstChatID == 0 || dstFileName == "" {
		return nil, fmt.Errorf("telegram: invalid destination %q", remote)
	}

	// Check if source chat has restrict-saving. If so, server-side copy
	// is never available regardless of bypass status.
	srcInfo := f.getChatInfo(ctx, srcObj.chatID)
	if srcInfo != nil && srcInfo.RestrictSaving {
		return nil, fs.ErrorCantCopy // force rclone to use download+reupload
	}

	// Check destination write access.
	if err := f.checkWriteAccess(ctx, dstChatID); err != nil {
		return nil, err
	}

	// Attempt server-side copy via file reference.
	cf := srcObj.cached
	newCf, err := f.client.SendWithReference(ctx, dstChatID, cf)
	if err != nil {
		// If file ref expired, refresh and retry once.
		if api.IsFileRefExpired(err) {
			if refreshErr := f.refreshFileRef(ctx, srcObj); refreshErr != nil {
				return nil, fs.ErrorCantCopy
			}
			cf = srcObj.cached
			newCf, err = f.client.SendWithReference(ctx, dstChatID, cf)
		}
		if err != nil {
			return nil, fmt.Errorf("telegram: server-side copy failed: %w", err)
		}
	}

	nameSet := f.buildNameSet(dstChatID)
	newCf.FileName = api.MakeFileName(newCf.FileName, newCf.MessageID, newCf.Date, newCf.IsPhoto, newCf.IsVideo, nameSet)
	f.cache.AddFile(dstChatID, newCf)
	f.cache.Save()
	return f.newObject(remote, newCf), nil
}

// Move performs a server-side move: copy + confirm + delete original.
// Returns fs.ErrorCantMove if not possible.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}

	dstChatID, _ := f.parsePath(f.fullPath(remote))
	srcInfo := f.getChatInfo(ctx, srcObj.chatID)

	// If restrict-saving on source: we can't use file references, so
	// we need rclone to do download+reupload for the copy part.
	if srcInfo != nil && srcInfo.RestrictSaving {
		return nil, fs.ErrorCantMove
	}

	// Perform the copy part.
	newObj, err := f.Copy(ctx, src, remote)
	if err != nil {
		return nil, fs.ErrorCantMove
	}

	// Confirm the new message landed by checking that we got a valid object back.
	if newObj == nil {
		return nil, fmt.Errorf("telegram: move failed - copy did not return an object")
	}

	// Delete the original message.
	if err := f.deleteMessage(ctx, srcObj.chatID, srcObj.cached.MessageID); err != nil {
		// Log but don't fail - the copy succeeded, we just couldn't clean up.
		fs.Errorf(f, "Move: copy succeeded to %d but failed to delete original msg %d in %d: %v",
			dstChatID, srcObj.cached.MessageID, srcObj.chatID, err)
	} else {
		f.cache.RemoveFile(srcObj.chatID, srcObj.cached.FileName)
		f.cache.Save()
	}

	return newObj, nil
}

// Shutdown saves the cache to disk.
func (f *Fs) Shutdown(ctx context.Context) error {
	f.cache.Save()
	f.persistSession()
	return nil
}

// --- internal helpers ---

// fullPath prepends the root to a remote path.
func (f *Fs) fullPath(remote string) string {
	if f.root == "" {
		return remote
	}
	if remote == "" {
		return f.root
	}
	return f.root + "/" + remote
}

// parsePath extracts chat ID and filename from a full path like "chats/12345/file.txt".
func (f *Fs) parsePath(p string) (chatID int64, fileName string) {
	parts := strings.SplitN(p, "/", 3)
	if len(parts) < 2 || parts[0] != "chats" {
		return 0, ""
	}
	chatID = api.ParseChatID(parts[1])
	if len(parts) == 3 {
		fileName = parts[2]
	}
	return chatID, fileName
}

// ensureChat makes sure we have a cached file listing for the chat.
// Does a full scan on first access, incremental on subsequent.
func (f *Fs) ensureChat(ctx context.Context, chatID int64) error {
	cc := f.cache.GetChatCache(chatID)
	offsetMsgID := 0
	if cc != nil {
		offsetMsgID = cc.LastMsgID
	}

	files, err := f.client.GetMessages(ctx, chatID, offsetMsgID, 100)
	if err != nil {
		// If we have a cache, use it stale rather than failing.
		if cc != nil {
			return nil
		}
		return fmt.Errorf("telegram: failed to scan chat %d: %w", chatID, err)
	}

	// Build name set from existing cache for dedup.
	nameSet := f.buildNameSet(chatID)

	for _, cf := range files {
		cf.FileName = api.MakeFileName(cf.FileName, cf.MessageID, cf.Date, cf.IsPhoto, cf.IsVideo, nameSet)
		f.cache.AddFile(chatID, cf)
	}

	f.cache.Save()
	return nil
}

// buildNameSet returns a set of filenames already in the cache for a chat.
func (f *Fs) buildNameSet(chatID int64) map[string]bool {
	existing := f.cache.ListFiles(chatID)
	nameSet := make(map[string]bool, len(existing))
	for name := range existing {
		nameSet[name] = true
	}
	return nameSet
}

// getChatInfo returns cached or freshly-fetched chat metadata.
func (f *Fs) getChatInfo(ctx context.Context, chatID int64) *api.ChatInfo {
	info := f.cache.GetChatInfo(chatID)
	if info != nil {
		return info
	}
	info, err := f.client.GetChatInfo(ctx, chatID)
	if err != nil {
		return nil
	}
	f.cache.SetChatInfo(info)
	return info
}

// checkWriteAccess verifies we can write to a chat, respecting restrict-saving rules.
func (f *Fs) checkWriteAccess(ctx context.Context, chatID int64) error {
	fs.Debugf(f, "checkWriteAccess: chatID=%d", chatID)
	info := f.getChatInfo(ctx, chatID)
	if info == nil {
		fs.Debugf(f, "checkWriteAccess: no chat info, allowing")
		return nil // can't determine, allow
	}
	if !info.RestrictSaving {
		return nil // no restriction
	}
	// Restrict-saving is on. Admin or bypass flag needed.
	if info.IsAdmin || f.opt.RestrictSavingBypass {
		return nil
	}
	return fmt.Errorf("telegram: chat %d has content protection enabled - set restrict_saving_bypass or use an admin account", chatID)
}

// checkReadAccess verifies we can read from a chat, respecting restrict-saving rules.
func (f *Fs) checkReadAccess(ctx context.Context, chatID int64) error {
	return f.checkWriteAccess(ctx, chatID) // same logic
}

// refreshFileRef refreshes the file_reference for an object.
func (f *Fs) refreshFileRef(ctx context.Context, obj *Object) error {
	files, err := f.client.RefreshFileRef(ctx, obj.chatID, obj.cached.MessageID)
	if err != nil {
		return err
	}
	for _, cf := range files {
		if cf.FileID == obj.cached.FileID || cf.PhotoID == obj.cached.PhotoID {
			obj.cached.FileRef = cf.FileRef
			f.cache.UpdateFileRef(obj.chatID, obj.cached.FileName, cf.FileRef)
			return nil
		}
	}
	return fmt.Errorf("file ref refresh did not return matching file for msg %d", obj.cached.MessageID)
}

// deleteMessage handles the delete flow - with bridge bot caption edit for groups.
func (f *Fs) deleteMessage(ctx context.Context, chatID int64, msgID int) error {
	info := f.getChatInfo(ctx, chatID)

	// Group chats: edit caption to "." first for bridge bot compatibility.
	if info != nil && info.Type == api.ChatTypeGroup {
		_ = f.client.EditMessageCaption(ctx, chatID, msgID, ".")
		time.Sleep(bridgeEditDelay)

		// Try to delete - if already gone (bridge cleaned it up), that's fine.
		err := f.client.DeleteMessage(ctx, chatID, msgID)
		if err != nil && isMessageGoneError(err) {
			return nil // bridge bot already deleted it
		}
		return err
	}

	// Channels and DMs: delete directly.
	return f.client.DeleteMessage(ctx, chatID, msgID)
}

// persistSession writes the current session back to the rclone config.
func (f *Fs) persistSession() {
	if newSession := f.client.SessionData(); newSession != "" {
		f.m.Set("session", newSession)
	}
}

// newObject creates an Object from cache data.
func (f *Fs) newObject(remote string, cf *api.CachedFile) *Object {
	return &Object{
		f:      f,
		remote: remote,
		cached: cf,
		chatID: cf.ChatID,
	}
}

// isImageFile checks if a filename looks like an image.
func isImageFile(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".jpg") ||
		strings.HasSuffix(lower, ".jpeg") ||
		strings.HasSuffix(lower, ".png") ||
		strings.HasSuffix(lower, ".webp")
}

// isMessageGoneError checks if an error means the message was already deleted.
func isMessageGoneError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "MESSAGE_DELETE_FORBIDDEN") ||
		strings.Contains(msg, "MESSAGE_ID_INVALID")
}

// Object describes a Telegram file.
type Object struct {
	f      *Fs
	remote string
	cached *api.CachedFile
	chatID int64
}

// Fs returns the parent Fs.
func (o *Object) Fs() fs.Info { return o.f }

// String returns the remote path.
func (o *Object) String() string { return o.remote }

// Remote returns the remote path.
func (o *Object) Remote() string { return o.remote }

// ModTime returns the message date.
func (o *Object) ModTime(ctx context.Context) time.Time { return o.cached.Date }

// Size returns the file size.
func (o *Object) Size() int64 { return o.cached.Size }

// Storable returns whether this object can be stored.
func (o *Object) Storable() bool { return true }

// SetModTime is not supported: Telegram messages have immutable timestamps.
func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	return fs.ErrorCantSetModTime
}

// Hash is not supported for Telegram files.
func (o *Object) Hash(ctx context.Context, ht hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

// MimeType returns the MIME type of the file.
func (o *Object) MimeType(ctx context.Context) string {
	return o.cached.MimeType
}

// Open reads the object content from Telegram.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// Check read access.
	if err := o.f.checkReadAccess(ctx, o.chatID); err != nil {
		return nil, err
	}

	rc, err := o.f.client.DownloadFile(ctx, o.cached)
	if err != nil {
		// On file ref expired, refresh and retry once.
		if api.IsFileRefExpired(err) {
			if refreshErr := o.f.refreshFileRef(ctx, o); refreshErr == nil {
				rc, err = o.f.client.DownloadFile(ctx, o.cached)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("telegram: download failed: %w", err)
		}
	}

	// Handle range requests.
	var offset, limit int64 = 0, -1
	for _, opt := range options {
		switch x := opt.(type) {
		case *fs.SeekOption:
			offset = x.Offset
		case *fs.RangeOption:
			offset, limit = x.Decode(o.cached.Size)
		}
	}
	if offset > 0 || limit >= 0 {
		// We need to skip/limit the stream. For simplicity, read and discard
		// the offset bytes, then limit the remainder.
		if offset > 0 {
			if _, err := io.CopyN(io.Discard, rc, offset); err != nil {
				rc.Close()
				return nil, err
			}
		}
		if limit >= 0 {
			return &limitedReadCloser{Reader: io.LimitReader(rc, limit), Closer: rc}, nil
		}
	}
	return rc, nil
}

// Update replaces this object's content (sends a new message).
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	newObj, err := o.f.Put(ctx, in, src, options...)
	if err != nil {
		return err
	}
	*o = *(newObj.(*Object))
	return nil
}

// Remove deletes this file from Telegram.
func (o *Object) Remove(ctx context.Context) error {
	if err := o.f.checkWriteAccess(ctx, o.chatID); err != nil {
		return err
	}
	if err := o.f.deleteMessage(ctx, o.chatID, o.cached.MessageID); err != nil {
		return err
	}
	o.f.cache.RemoveFile(o.chatID, o.cached.FileName)
	o.f.cache.Save()
	return nil
}

// limitedReadCloser pairs a limited Reader with the underlying Closer.
type limitedReadCloser struct {
	io.Reader
	io.Closer
}

// Check interface satisfaction.
var (
	_ fs.Fs         = (*Fs)(nil)
	_ fs.Copier     = (*Fs)(nil)
	_ fs.Mover      = (*Fs)(nil)
	_ fs.Shutdowner = (*Fs)(nil)
	_ fs.Object     = (*Object)(nil)
	_ fs.MimeTyper  = (*Object)(nil)
)

// Ensure errors package is used (for future use).
var _ = errors.New
