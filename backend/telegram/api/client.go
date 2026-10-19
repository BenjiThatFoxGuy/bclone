package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/rclone/rclone/fs"
)

// Client wraps gotd/td into a high-level interface for the Telegram
// backend. It handles authentication, session management, and provides
// methods for file operations.
type Client struct {
	appID   int
	appHash string

	// Auth credentials (exactly one set is populated).
	botToken string // bot mode
	phone    string // user mode

	// Device details for initConnection.
	deviceModel   string
	systemVersion string
	appVersion    string

	// Session storage backed by rclone config.
	sessionStorage *ConfigSessionStorage

	// The gotd client and raw API, set after Connect().
	client *telegram.Client
	api    *tg.Client

	// Account info, populated after auth.
	isPremium bool
	selfID    int64
	isBot     bool

	// Concurrency guard for Connect().
	connectOnce sync.Once
	connectErr  error

	// Upload/download helpers.
	uploader   *uploader.Uploader
	downloader *downloader.Downloader

	// cancelRun shuts down a config-time connection (set by ConnectRaw).
	cancelRun context.CancelFunc
}

// ConfigSessionStorage adapts gotd's session.Storage interface to store
// session bytes as a base64 string in the rclone config map.
type ConfigSessionStorage struct {
	mu   sync.Mutex
	data []byte
}

// NewConfigSessionStorage creates session storage, optionally pre-loaded
// from an existing base64 config value.
func NewConfigSessionStorage(encoded string) *ConfigSessionStorage {
	s := &ConfigSessionStorage{}
	if encoded != "" {
		if data, err := base64.StdEncoding.DecodeString(encoded); err == nil {
			s.data = data
		}
	}
	return s
}

// LoadSession implements session.Storage.
func (s *ConfigSessionStorage) LoadSession(_ context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		return nil, session.ErrNotFound
	}
	cp := make([]byte, len(s.data))
	copy(cp, s.data)
	return cp, nil
}

// StoreSession implements session.Storage.
func (s *ConfigSessionStorage) StoreSession(_ context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = make([]byte, len(data))
	copy(s.data, data)
	return nil
}

// Encode returns the session as a base64 string for config persistence.
func (s *ConfigSessionStorage) Encode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(s.data)
}

// ClientOptions configures a new Client.
type ClientOptions struct {
	AppID         int
	AppHash       string
	BotToken      string // mutually exclusive with Phone
	Phone         string // mutually exclusive with BotToken
	DeviceModel   string
	SystemVersion string
	AppVersion    string
	SessionData   string // base64-encoded session from prior run
}

// NewClient creates a Telegram client. Call Connect() to establish the
// connection and authenticate.
func NewClient(opts ClientOptions) *Client {
	if opts.DeviceModel == "" {
		opts.DeviceModel = "bclone"
	}
	if opts.SystemVersion == "" {
		opts.SystemVersion = runtime.GOOS + "/" + runtime.GOARCH
	}
	if opts.AppVersion == "" {
		opts.AppVersion = "bclone/1.0"
	}

	c := &Client{
		appID:          opts.AppID,
		appHash:        opts.AppHash,
		botToken:       opts.BotToken,
		phone:          opts.Phone,
		deviceModel:    opts.DeviceModel,
		systemVersion:  opts.SystemVersion,
		appVersion:     opts.AppVersion,
		sessionStorage: NewConfigSessionStorage(opts.SessionData),
		isBot:          opts.BotToken != "",
	}
	return c
}

// Connect establishes the MTProto connection and authenticates.
// Safe to call multiple times; only the first call does work.
func (c *Client) Connect(ctx context.Context) error {
	c.connectOnce.Do(func() {
		c.connectErr = c.doConnect(ctx)
	})
	return c.connectErr
}

func (c *Client) doConnect(ctx context.Context) error {
	fs.Debugf("telegram", "Connecting to Telegram (bot=%v)", c.isBot)
	c.client = telegram.NewClient(c.appID, c.appHash, telegram.Options{
		SessionStorage: c.sessionStorage,
		Device: telegram.DeviceConfig{
			DeviceModel:    c.deviceModel,
			SystemVersion:  c.systemVersion,
			AppVersion:     c.appVersion,
			SystemLangCode: "en",
			LangPack:       "",
			LangCode:       "en",
		},
	})

	// Run establishes the connection. We need it to stay alive, so we
	// run it in a background goroutine and use the API handle it gives us.
	ready := make(chan struct{})
	errCh := make(chan error, 1)

	go func() {
		errCh <- c.client.Run(ctx, func(ctx context.Context) error {
			c.api = c.client.API()

			// Authenticate.
			if c.isBot {
				fs.Debugf("telegram", "Authenticating as bot")
				if _, err := c.client.Auth().Bot(ctx, c.botToken); err != nil {
					// Might already be authorized from session.
					if !isAlreadyAuthorized(err) {
						return fmt.Errorf("bot auth: %w", err)
					}
					fs.Debugf("telegram", "Bot already authorized from session")
				}
			} else if c.phone != "" {
				fs.Debugf("telegram", "Checking user session auth status")
				// For user auth, try to use existing session first.
				status, err := c.client.Auth().Status(ctx)
				if err != nil {
					return fmt.Errorf("auth status: %w", err)
				}
				if !status.Authorized {
					return fmt.Errorf("user session not authorized - run rclone config to complete login")
				}
				fs.Debugf("telegram", "User session authorized")
			}

			// Fetch self info for premium detection.
			fs.Debugf("telegram", "Fetching account info")
			if err := c.fetchSelfInfo(ctx); err != nil {
				return fmt.Errorf("fetch self: %w", err)
			}
			fs.Debugf("telegram", "Connected (premium=%v)", c.isPremium)

			// Initialize helpers.
			c.uploader = uploader.NewUploader(c.api)
			c.downloader = downloader.NewDownloader()

			close(ready)

			// Block until context is cancelled to keep the connection alive.
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-ready:
		return nil
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SessionData returns the current session as a base64 string for persistence.
func (c *Client) SessionData() string {
	return c.sessionStorage.Encode()
}

// --- Config-time auth methods (used during rclone config, not runtime) ---

// initClient creates the gotd client without running it yet.
func (c *Client) initClient() {
	if c.client != nil {
		return
	}
	c.client = telegram.NewClient(c.appID, c.appHash, telegram.Options{
		SessionStorage: c.sessionStorage,
		Device: telegram.DeviceConfig{
			DeviceModel:    c.deviceModel,
			SystemVersion:  c.systemVersion,
			AppVersion:     c.appVersion,
			SystemLangCode: "en",
			LangPack:       "",
			LangCode:       "en",
		},
	})
}

// runClient starts the gotd client and executes fn inside the connection.
// The connection is kept alive until the returned cancel function is called
// or the context is cancelled.
func (c *Client) runClient(ctx context.Context, fn func(ctx context.Context) error) error {
	c.initClient()

	// Create a cancellable child context so we can shut down the connection
	// after the work function completes.
	runCtx, cancel := context.WithCancel(ctx)

	ready := make(chan error, 1)
	done := make(chan error, 1)

	go func() {
		done <- c.client.Run(runCtx, func(ctx context.Context) error {
			c.api = c.client.API()
			err := fn(ctx)
			ready <- err
			if err != nil {
				cancel()
				return err
			}
			// Keep connection alive until cancelled.
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case err := <-ready:
		if err != nil {
			cancel()
			<-done
			return err
		}
		// Connection is alive. Store the cancel for later cleanup.
		// For config-time operations we just cancel immediately after
		// the auth completes - we don't need the connection to stay up.
		cancel()
		<-done
		return nil
	case err := <-done:
		cancel()
		return err
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
}

// ConnectAndAuthBot connects and authenticates as a bot in one shot.
// Used during rclone config.
func (c *Client) ConnectAndAuthBot(ctx context.Context) error {
	return c.runClient(ctx, func(ctx context.Context) error {
		if _, err := c.client.Auth().Bot(ctx, c.botToken); err != nil {
			if !isAlreadyAuthorized(err) {
				return fmt.Errorf("bot auth: %w", err)
			}
		}
		return nil
	})
}

// ConnectAndSendCode connects and sends an auth code to the phone number.
// Returns the code hash needed for SignIn. Used during rclone config.
func (c *Client) ConnectAndSendCode(ctx context.Context) (codeHash string, err error) {
	err = c.runClient(ctx, func(ctx context.Context) error {
		sentCode, err := c.client.Auth().SendCode(ctx, c.phone, auth.SendCodeOptions{})
		if err != nil {
			return fmt.Errorf("send code: %w", err)
		}
		// Extract the phone code hash from the response.
		switch sc := sentCode.(type) {
		case *tg.AuthSentCode:
			codeHash = sc.GetPhoneCodeHash()
		case *tg.AuthSentCodeSuccess:
			// Already authorized somehow.
			codeHash = ""
		default:
			return fmt.Errorf("unexpected SendCode response type: %T", sentCode)
		}
		return nil
	})
	return codeHash, err
}

// ConnectRaw connects to Telegram without authenticating.
// Used to resume a partially-completed auth flow.
func (c *Client) ConnectRaw(ctx context.Context) error {
	c.initClient()

	// We need the client running to make API calls. Start it in background.
	runCtx, cancel := context.WithCancel(ctx)
	c.cancelRun = cancel

	ready := make(chan struct{})
	go func() {
		_ = c.client.Run(runCtx, func(ctx context.Context) error {
			c.api = c.client.API()
			close(ready)
			<-ctx.Done()
			return ctx.Err()
		})
	}()

	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		cancel()
		return ctx.Err()
	}
}

// SignIn verifies the phone code. Returns true if 2FA is needed.
func (c *Client) SignIn(ctx context.Context, code, codeHash string) (needs2FA bool, err error) {
	req := &tg.AuthSignInRequest{
		PhoneNumber:   c.phone,
		PhoneCodeHash: codeHash,
	}
	req.SetPhoneCode(code)

	_, signInErr := c.api.AuthSignIn(ctx, req)
	if signInErr != nil {
		if tgerr.Is(signInErr, "SESSION_PASSWORD_NEEDED") {
			return true, nil
		}
		return false, fmt.Errorf("sign in: %w", signInErr)
	}
	return false, nil
}

// CheckPassword verifies the 2FA password using gotd's auth client,
// which handles SRP parameter fetching and hash computation internally.
func (c *Client) CheckPassword(ctx context.Context, password string) error {
	authClient := auth.NewClient(c.api, rand.Reader, c.appID, c.appHash)
	_, err := authClient.Password(ctx, password)
	if err != nil {
		return fmt.Errorf("check password: %w", err)
	}
	return nil
}

// Close shuts down the connection (for config-time clients).
func (c *Client) Close() {
	if c.cancelRun != nil {
		c.cancelRun()
	}
}

// IsPremium returns whether the authenticated account has Telegram Premium.
func (c *Client) IsPremium() bool {
	return c.isPremium
}

// IsBot returns whether the client is authenticated as a bot.
func (c *Client) IsBot() bool {
	return c.isBot
}

// MaxFileSize returns the maximum upload size based on account status.
func (c *Client) MaxFileSize() int64 {
	if c.isPremium {
		return 4 * 1024 * 1024 * 1024 // 4 GB
	}
	return 2 * 1024 * 1024 * 1024 // 2 GB
}

func (c *Client) fetchSelfInfo(ctx context.Context) error {
	if c.isBot {
		me, err := c.api.BotsGetBotInfo(ctx, &tg.BotsGetBotInfoRequest{})
		if err != nil {
			// Fallback: bots are never premium.
			c.isPremium = false
			return nil
		}
		_ = me
		c.isPremium = false
		return nil
	}

	self, err := c.api.UsersGetFullUser(ctx, &tg.InputUserSelf{})
	if err != nil {
		return err
	}
	if user, ok := self.GetUsers()[0].(*tg.User); ok {
		c.isPremium = user.GetPremium()
		c.selfID = user.GetID()
	}
	return nil
}

// SelfID returns the authenticated user's Telegram ID.
func (c *Client) SelfID() int64 {
	return c.selfID
}

// GetChatInfo fetches metadata about a chat by its numeric ID.
func (c *Client) GetChatInfo(ctx context.Context, chatID int64) (*ChatInfo, error) {
	info := &ChatInfo{ID: chatID}

	// Determine chat type from the ID sign convention.
	// Channels/supergroups: -100xxxxxxxxxx
	// Basic groups: -xxxxxxxxxx (no -100 prefix)
	// Users: positive
	switch {
	case chatID > 0:
		// User / DM
		info.Type = ChatTypeUser
		inputUser := &tg.InputUser{UserID: chatID}
		users, err := c.api.UsersGetUsers(ctx, []tg.InputUserClass{inputUser})
		if err != nil {
			return nil, fmt.Errorf("get user %d: %w", chatID, err)
		}
		if len(users) > 0 {
			if u, ok := users[0].(*tg.User); ok {
				if ah, ok := u.GetAccessHash(); ok {
					info.AccessHash = ah
				}
				first, _ := u.GetFirstName()
				last, _ := u.GetLastName()
				name := strings.TrimSpace(first + " " + last)
				if name != "" {
					info.Title = name
				}
			}
		}

	case isChannelID(chatID):
		// Channel or supergroup
		realID := toChannelID(chatID)
		info.Type = ChatTypeChannel
		inputChannel := &tg.InputChannel{ChannelID: realID}
		result, err := c.api.ChannelsGetChannels(ctx, []tg.InputChannelClass{inputChannel})
		if err != nil {
			return nil, fmt.Errorf("get channel %d: %w", chatID, err)
		}
		if chats := extractChats(result); len(chats) > 0 {
			if ch, ok := chats[0].(*tg.Channel); ok {
				if ah, ok := ch.GetAccessHash(); ok {
					info.AccessHash = ah
				}
				info.Title = ch.GetTitle()
				info.RestrictSaving = ch.GetNoforwards()
				if rights, ok := ch.GetAdminRights(); ok {
					info.IsAdmin = rights.Flags != 0
				}
			}
		}

	default:
		// Basic group
		realID := -chatID
		info.Type = ChatTypeGroup
		result, err := c.api.MessagesGetChats(ctx, []int64{realID})
		if err != nil {
			return nil, fmt.Errorf("get chat %d: %w", chatID, err)
		}
		if chats := extractChats(result); len(chats) > 0 {
			if ch, ok := chats[0].(*tg.Chat); ok {
				info.Title = ch.GetTitle()
				info.RestrictSaving = ch.GetNoforwards()
				if rights, ok := ch.GetAdminRights(); ok {
					info.IsAdmin = rights.Flags != 0
				}
			}
		}
	}

	return info, nil
}

// GetMessages fetches message history for a chat. If offsetMsgID > 0,
// only messages newer than that ID are returned (incremental scan).
// Returns messages with file attachments only.
func (c *Client) GetMessages(ctx context.Context, chatID int64, offsetMsgID int, limit int) ([]*CachedFile, error) {
	fs.Debugf("telegram", "GetMessages: chatID=%d offsetMsgID=%d limit=%d", chatID, offsetMsgID, limit)
	peer, err := c.resolvePeer(ctx, chatID)
	if err != nil {
		return nil, err
	}

	var allFiles []*CachedFile
	offsetID := 0 // for pagination
	if offsetMsgID > 0 {
		// For incremental: we want messages with ID > offsetMsgID.
		// getHistory returns messages BEFORE offsetID, so we start from 0
		// (newest) and stop when we see offsetMsgID.
		offsetID = 0
	}

	// Cap the number of API pages to avoid hanging on chats with huge history.
	// 50 pages × 100 messages = 5000 most recent messages scanned.
	const maxPages = 50
	pages := 0

	for {
		history, err := c.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:     peer,
			OffsetID: offsetID,
			Limit:    min(limit, 100),
		})
		if err != nil {
			if ok := c.handleFloodWait(ctx, err); ok {
				continue // retry after flood wait
			}
			return nil, fmt.Errorf("get history for %d: %w", chatID, err)
		}

		msgs := extractMessages(history)
		if len(msgs) == 0 {
			break
		}

		hitOld := false
		for _, msg := range msgs {
			m, ok := msg.(*tg.Message)
			if !ok {
				continue
			}
			if offsetMsgID > 0 && m.GetID() <= offsetMsgID {
				hitOld = true
				break
			}
			files := extractFilesFromMessage(m, chatID)
			allFiles = append(allFiles, files...)
		}

		pages++
		if hitOld || len(msgs) < 100 || pages >= maxPages {
			if pages >= maxPages {
				fs.Debugf("telegram", "GetMessages: hit page limit (%d pages), stopping scan", maxPages)
			}
			break
		}
		// Set offset for next page - use the oldest message ID from this batch.
		if lastMsg, ok := msgs[len(msgs)-1].(*tg.Message); ok {
			offsetID = lastMsg.GetID()
		} else {
			break
		}
	}

	fs.Debugf("telegram", "GetMessages: found %d files in %d pages", len(allFiles), pages)
	return allFiles, nil
}

// RefreshFileRef fetches a fresh file_reference for a specific message.
func (c *Client) RefreshFileRef(ctx context.Context, chatID int64, msgID int) ([]*CachedFile, error) {
	peer, err := c.resolvePeer(ctx, chatID)
	if err != nil {
		return nil, err
	}

	inputMsg := []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}}

	var result tg.MessagesMessagesClass
	switch peer.(type) {
	case *tg.InputPeerChannel:
		ch := peer.(*tg.InputPeerChannel)
		result, err = c.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
			ID:      inputMsg,
		})
	default:
		result, err = c.api.MessagesGetMessages(ctx, inputMsg)
	}
	if err != nil {
		return nil, fmt.Errorf("refresh file ref for msg %d in %d: %w", msgID, chatID, err)
	}

	msgs := extractMessages(result)
	for _, msg := range msgs {
		if m, ok := msg.(*tg.Message); ok {
			return extractFilesFromMessage(m, chatID), nil
		}
	}
	return nil, fmt.Errorf("message %d not found in chat %d", msgID, chatID)
}

// DownloadFile downloads a file from Telegram.
func (c *Client) DownloadFile(ctx context.Context, cf *CachedFile) (io.ReadCloser, error) {
	loc := c.buildInputFileLocation(cf)

	pr, pw := io.Pipe()
	go func() {
		_, err := c.downloader.Download(c.api, loc).Stream(ctx, pw)
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// SendDocument uploads and sends a file as a document to a chat.
func (c *Client) SendDocument(ctx context.Context, chatID int64, fileName string, size int64, r io.Reader) (*CachedFile, error) {
	fs.Debugf("telegram", "SendDocument: chatID=%d fileName=%q size=%d", chatID, fileName, size)
	peer, err := c.resolvePeer(ctx, chatID)
	if err != nil {
		return nil, err
	}
	fs.Debugf("telegram", "SendDocument: peer resolved, sending upload action")

	// Send upload action.
	_ = c.sendChatAction(ctx, peer, &tg.SendMessageUploadDocumentAction{})

	// Upload the file.
	fs.Debugf("telegram", "SendDocument: uploading file")
	f, err := c.uploader.FromReader(ctx, fileName, r)
	if err != nil {
		return nil, fmt.Errorf("upload %q: %w", fileName, err)
	}
	fs.Debugf("telegram", "SendDocument: upload complete, sending message")

	result, err := c.api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:     peer,
		RandomID: randomID(),
		Media: &tg.InputMediaUploadedDocument{
			File:     f,
			MimeType: DetectMime(fileName),
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: fileName},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("send document to %d: %w", chatID, err)
	}

	return c.extractSentFile(result, chatID)
}

// SendPhoto uploads and sends a file as a high-res photo to a chat.
func (c *Client) SendPhoto(ctx context.Context, chatID int64, fileName string, r io.Reader) (*CachedFile, error) {
	peer, err := c.resolvePeer(ctx, chatID)
	if err != nil {
		return nil, err
	}

	_ = c.sendChatAction(ctx, peer, &tg.SendMessageUploadPhotoAction{})

	f, err := c.uploader.FromReader(ctx, fileName, r)
	if err != nil {
		return nil, fmt.Errorf("upload photo %q: %w", fileName, err)
	}

	result, err := c.api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:     peer,
		RandomID: randomID(),
		Media: &tg.InputMediaUploadedPhoto{
			File: f,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("send photo to %d: %w", chatID, err)
	}

	return c.extractSentFile(result, chatID)
}

// SendWithReference sends a file to a chat using an existing file reference
// (server-side copy - no download/reupload).
func (c *Client) SendWithReference(ctx context.Context, chatID int64, cf *CachedFile) (*CachedFile, error) {
	peer, err := c.resolvePeer(ctx, chatID)
	if err != nil {
		return nil, err
	}

	var media tg.InputMediaClass
	if cf.IsPhoto {
		media = &tg.InputMediaPhoto{
			ID: &tg.InputPhoto{
				ID:            cf.PhotoID,
				AccessHash:    cf.AccessHash,
				FileReference: cf.FileRef,
			},
		}
	} else {
		media = &tg.InputMediaDocument{
			ID: &tg.InputDocument{
				ID:            cf.FileID,
				AccessHash:    cf.AccessHash,
				FileReference: cf.FileRef,
			},
		}
	}

	result, err := c.api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:     peer,
		RandomID: randomID(),
		Media:    media,
	})
	if err != nil {
		return nil, fmt.Errorf("send with reference to %d: %w", chatID, err)
	}

	return c.extractSentFile(result, chatID)
}

// DeleteMessage deletes a message in a chat.
func (c *Client) DeleteMessage(ctx context.Context, chatID int64, msgID int) error {
	if isChannelID(chatID) {
		ch := &tg.InputChannel{ChannelID: toChannelID(chatID)}
		_, err := c.api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: ch,
			ID:      []int{msgID},
		})
		return err
	}
	_, err := c.api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{
		ID:     []int{msgID},
		Revoke: true,
	})
	return err
}

// EditMessageCaption edits the text/caption of a message.
func (c *Client) EditMessageCaption(ctx context.Context, chatID int64, msgID int, newText string) error {
	peer, err := c.resolvePeer(ctx, chatID)
	if err != nil {
		return err
	}
	_, err = c.api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
		Peer:    peer,
		ID:      msgID,
		Message: newText,
	})
	return err
}

// sendChatAction sends a typing/upload action to a chat.
func (c *Client) sendChatAction(ctx context.Context, peer tg.InputPeerClass, action tg.SendMessageActionClass) error {
	_, err := c.api.MessagesSetTyping(ctx, &tg.MessagesSetTypingRequest{
		Peer:   peer,
		Action: action,
	})
	return err
}

// resolvePeer converts a numeric chat ID to an InputPeerClass.
func (c *Client) resolvePeer(ctx context.Context, chatID int64) (tg.InputPeerClass, error) {
	switch {
	case chatID > 0:
		// For the authenticated user's own ID, use InputPeerSelf (saved messages).
		// InputPeerUser requires an access hash we don't have for arbitrary users.
		if chatID == c.selfID {
			fs.Debugf("telegram", "resolvePeer: chatID=%d is self, using InputPeerSelf", chatID)
			return &tg.InputPeerSelf{}, nil
		}
		fs.Debugf("telegram", "resolvePeer: chatID=%d -> InputPeerUser", chatID)
		return &tg.InputPeerUser{UserID: chatID}, nil
	case isChannelID(chatID):
		realID := toChannelID(chatID)
		fs.Debugf("telegram", "resolvePeer: chatID=%d -> InputPeerChannel (realID=%d)", chatID, realID)
		return &tg.InputPeerChannel{ChannelID: realID}, nil
	default:
		fs.Debugf("telegram", "resolvePeer: chatID=%d -> InputPeerChat (chatID=%d)", chatID, -chatID)
		return &tg.InputPeerChat{ChatID: -chatID}, nil
	}
}

// randomID generates a cryptographically random int64 for Telegram's
// message dedup. Every MessagesSendMedia call requires a unique RandomID.
func randomID() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]))
}

func (c *Client) buildInputFileLocation(cf *CachedFile) tg.InputFileLocationClass {
	if cf.IsPhoto {
		return &tg.InputPhotoFileLocation{
			ID:            cf.PhotoID,
			AccessHash:    cf.AccessHash,
			FileReference: cf.FileRef,
			ThumbSize:     cf.ThumbSize,
		}
	}
	return &tg.InputDocumentFileLocation{
		ID:            cf.FileID,
		AccessHash:    cf.AccessHash,
		FileReference: cf.FileRef,
	}
}

// handleFloodWait checks if err is a FLOOD_WAIT and sleeps if so.
// For FLOOD_PREMIUM_WAIT, premium users continue without sleeping.
// Returns true if the caller should retry.
func (c *Client) handleFloodWait(ctx context.Context, err error) bool {
	// Check for FLOOD_PREMIUM_WAIT specifically first.
	if rpcErr, ok := tgerr.AsType(err, tgerr.ErrPremiumFloodWait); ok {
		if c.isPremium {
			return true // premium user: ignore this limit, retry immediately
		}
		// Not premium: treat like regular flood wait.
		d := time.Duration(rpcErr.Argument) * time.Second
		select {
		case <-time.After(d + time.Second):
			return true
		case <-ctx.Done():
			return false
		}
	}

	if d, ok := tgerr.AsFloodWait(err); ok {
		select {
		case <-time.After(d + time.Second):
			return true
		case <-ctx.Done():
			return false
		}
	}
	return false
}

// extractSentFile extracts file metadata from a sent message result.
func (c *Client) extractSentFile(result tg.UpdatesClass, chatID int64) (*CachedFile, error) {
	updates := extractUpdates(result)
	for _, u := range updates {
		switch upd := u.(type) {
		case *tg.UpdateNewMessage:
			if m, ok := upd.GetMessage().(*tg.Message); ok {
				files := extractFilesFromMessage(m, chatID)
				if len(files) > 0 {
					return files[0], nil
				}
			}
		case *tg.UpdateNewChannelMessage:
			if m, ok := upd.GetMessage().(*tg.Message); ok {
				files := extractFilesFromMessage(m, chatID)
				if len(files) > 0 {
					return files[0], nil
				}
			}
		}
	}
	return nil, fmt.Errorf("could not extract file info from send result")
}

// IsFileRefExpired checks if an error is a FILE_REFERENCE_EXPIRED error.
func IsFileRefExpired(err error) bool {
	return tgerr.Is(err, "FILE_REFERENCE_EXPIRED")
}

// --- helpers ---

func isAlreadyAuthorized(err error) bool {
	return tgerr.Is(err, "SESSION_PASSWORD_NEEDED") ||
		err == nil ||
		strings.Contains(err.Error(), "already authorized")
}

// IsChannelID returns true for channel/supergroup IDs (-100xxxxxxxxxx).
func IsChannelID(id int64) bool {
	return id < -1000000000000
}

// isChannelID is a package-internal alias for use in this file.
func isChannelID(id int64) bool { return IsChannelID(id) }

// ToChannelID strips the -100 prefix to get the raw channel ID.
func ToChannelID(id int64) int64 {
	return -id - 1000000000000
}

// toChannelID is a package-internal alias.
func toChannelID(id int64) int64 { return ToChannelID(id) }

// extractChats pulls the chat list from various response types.
func extractChats(result tg.MessagesChatsClass) []tg.ChatClass {
	switch r := result.(type) {
	case *tg.MessagesChats:
		return r.GetChats()
	case *tg.MessagesChatsSlice:
		return r.GetChats()
	}
	return nil
}

// extractMessages pulls messages from various history response types.
func extractMessages(result tg.MessagesMessagesClass) []tg.MessageClass {
	switch r := result.(type) {
	case *tg.MessagesMessages:
		return r.GetMessages()
	case *tg.MessagesMessagesSlice:
		return r.GetMessages()
	case *tg.MessagesChannelMessages:
		return r.GetMessages()
	}
	return nil
}

// extractUpdates pulls the update list from various UpdatesClass types.
func extractUpdates(result tg.UpdatesClass) []tg.UpdateClass {
	switch r := result.(type) {
	case *tg.Updates:
		return r.GetUpdates()
	case *tg.UpdatesCombined:
		return r.GetUpdates()
	case *tg.UpdateShortMessage:
		return nil
	case *tg.UpdateShortSentMessage:
		return nil
	}
	return nil
}

// extractFilesFromMessage extracts file metadata from a single message.
func extractFilesFromMessage(m *tg.Message, chatID int64) []*CachedFile {
	var files []*CachedFile
	date := time.Unix(int64(m.GetDate()), 0)

	// Check for document (files, videos sent as document, etc.)
	if media, ok := m.GetMedia(); ok {
		switch med := media.(type) {
		case *tg.MessageMediaDocument:
			if doc, ok := med.GetDocument(); ok {
				if d, ok := doc.(*tg.Document); ok {
					cf := &CachedFile{
						MessageID:  m.GetID(),
						ChatID:     chatID,
						Size:       d.GetSize(),
						MimeType:   d.GetMimeType(),
						Date:       date,
						FileRef:    d.GetFileReference(),
						AccessHash: d.GetAccessHash(),
						FileID:     d.GetID(),
						DCid:       d.GetDCID(),
						IsDocument: true,
					}
					// Extract filename from attributes.
					for _, attr := range d.GetAttributes() {
						switch a := attr.(type) {
						case *tg.DocumentAttributeFilename:
							cf.FileName = a.GetFileName()
						case *tg.DocumentAttributeVideo:
							cf.IsVideo = true
						}
					}
					files = append(files, cf)
				}
			}

		case *tg.MessageMediaPhoto:
			if photo, ok := med.GetPhoto(); ok {
				if p, ok := photo.(*tg.Photo); ok {
					// Find the largest photo size.
					var bestSize *tg.PhotoSize
					var bestPixels int
					for _, sz := range p.GetSizes() {
						if ps, ok := sz.(*tg.PhotoSize); ok {
							px := ps.GetW() * ps.GetH()
							if px > bestPixels {
								bestPixels = px
								bestSize = ps
							}
						}
					}
					if bestSize != nil {
						cf := &CachedFile{
							MessageID:  m.GetID(),
							ChatID:     chatID,
							Size:       int64(bestSize.GetSize()),
							MimeType:   "image/jpeg",
							Date:       date,
							FileRef:    p.GetFileReference(),
							AccessHash: p.GetAccessHash(),
							FileID:     p.GetID(),
							PhotoID:    p.GetID(),
							DCid:       p.GetDCID(),
							ThumbSize:  bestSize.GetType(),
							IsPhoto:    true,
						}
						files = append(files, cf)
					}
				}
			}
		}
	}

	return files
}

// DetectMime returns a MIME type based on file extension.
func DetectMime(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(lower, ".zip"):
		return "application/zip"
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "application/gzip"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".mp4"):
		return "video/mp4"
	case strings.HasSuffix(lower, ".webm"):
		return "video/webm"
	case strings.HasSuffix(lower, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(lower, ".ogg"):
		return "audio/ogg"
	case strings.HasSuffix(lower, ".txt"):
		return "text/plain"
	case strings.HasSuffix(lower, ".json"):
		return "application/json"
	default:
		return "application/octet-stream"
	}
}

// min returns the smaller of a and b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ParseChatID extracts a numeric chat ID from a path component.
// Returns 0 if the string is not a valid chat ID.
func ParseChatID(s string) int64 {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return id
}
