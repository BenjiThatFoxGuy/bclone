# bclone Telegram Backend — Specification

## Overview

A native Telegram backend for bclone that talks directly to Telegram via MTProto
(no middleman server like teldrive). Supports both bot tokens and user sessions.
Read support lists and downloads files from chats; write support sends files to
chats. Heavy metadata caching, similar to the gotohp backend pattern.

**MTProto library:** [gotd/td](https://github.com/gotd/td) — pure Go, no cgo,
handles reconnection, flood-wait backoff, and session management natively.

---

## Authentication

Auth mode is **inferred** from the first input during `rclone config`:

- **Bot token** → bot mode. Simple, no interactive auth.
- **Phone number** → user mode. Full MTProto login flow: phone → code → optional
  2FA password.

### Config prompts (user mode)

1. `api_id` / `api_hash` — required for both modes, but user mode prompts
   explicitly. Sensible defaults can be offered (Telegram Desktop's public
   credentials, as gotd documents).
2. `phone` — phone number with country code.
3. `device_model` — default: `"bclone"`
4. `system_version` — default: current OS version string
5. `app_version` — default: bclone version string
6. Interactive login: phone code prompt, optional 2FA password prompt.

### Config prompts (bot mode)

1. `bot_token` — the BotFather token.
2. `api_id` / `api_hash` — with defaults.

### Session storage

MTProto sessions are serialized into the rclone config (encrypted by rclone's
config encryption). gotd supports pluggable session backends; we implement one
that reads/writes the session bytes as a base64 config value. No external session
files to manage.

---

## Path Layout

```
telegram:chats/<chat_id>/                     → list files in chat
telegram:chats/<chat_id>/filename-msgid.ext   → specific file
```

- Root does **not** list chats (no `telegram:` or `telegram:chats/` listing).
- Chat IDs are numeric, including negative IDs for groups/channels
  (e.g., `-1001234567890`).
- `chats/` is a virtual prefix — the only supported top-level directory.

### Filename rules

- **Documents:** use the filename Telegram provides in the document attribute.
- **Media without filenames** (photos, some videos): synthesize using Telegram
  client conventions, e.g., `photo_2024-01-15_12-34-56.jpg`,
  `video_2024-01-15_12-34-56.mp4`.
- **Collision handling:** when two files in the same chat produce the same name,
  append `-<message_id>` before the extension:
  `photo_2024-01-15_12-34-56-54321.jpg`.
- Never overwrite a filename Telegram already provides.

---

## Config Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `bot_token` | string (sensitive) | — | Bot token. Presence triggers bot mode. |
| `api_id` | int (sensitive) | — | Telegram API ID |
| `api_hash` | string (sensitive) | — | Telegram API hash |
| `phone` | string (sensitive) | — | Phone number. Presence triggers user mode. |
| `device_model` | string | `"bclone"` | Device model reported to Telegram |
| `system_version` | string | auto-detected | OS version reported to Telegram |
| `app_version` | string | bclone version | App version reported to Telegram |
| `media_mode` | string | `"documents"` | `"documents"` = only files with document type; `"all"` = documents + photos + videos |
| `restrict_saving_bypass` | bool | `false` | Allow access to files in chats with restrict-saving enabled (when not admin) |
| `cache_dir` | string | `"~/.cache/rclone/telegram"` | Metadata cache persistence directory |

Session data is stored in the rclone config itself (not a separate option).

---

## Operations

### List (read)

Lists files in a chat by scanning message history via `messages.getHistory`.

- Respects `media_mode`: filters to documents only, or includes all media.
- Returns `fs.Object` entries with: filename (per naming rules above), size,
  mod time (message date), MIME type.
- Each Object internally carries: message ID, chat ID, file_reference, access
  hash — needed for downloads and server-side operations.

### NewObject (read)

Looks up a specific file by its constructed filename. Resolved from the metadata
cache; falls back to scanning if not cached.

### Open / Read (download)

Downloads a file via `upload.getFile` using the stored file_reference.

- On `FILE_REFERENCE_EXPIRED`: refresh via `messages.getMessages`, retry once.
- **Restrict-saving check:** if the chat has restrict-saving enabled:
  - If admin in chat OR `restrict_saving_bypass` is true → allow download.
  - Otherwise → return error.

### Put (write)

Sends a file to a chat via `messages.sendMedia` or `messages.sendDocument`.

- **Default:** send as document (preserves original file, no compression).
- **Config/flag for media mode:** when enabled, detect content type and:
  - Images → send as photo with "send large image" (high-res, no compression).
  - Other → send as document.
- Sends appropriate **chat actions** during upload:
  - `sendUploadDocumentAction` for documents
  - `sendUploadPhotoAction` for photos
  - `sendUploadVideoAction` for videos
  - No typing action, ever.
- **Size limits:**
  - Non-premium: 2 GB max.
  - Premium: 4 GB max.
  - Detected via `users.getFullUser` on the authenticated account.
  - Oversize files → return error. No chunking.

### Copy (server-side)

Sends to the destination chat reusing the source file's `file_reference` — no
download/reupload, Telegram resolves the media server-side.

- **Restrict-saving on source chat:**
  - Bypass conditions not met → return `fs.ErrorCantCopy` (hard block).
  - Bypass conditions met (admin OR config flag) → still return
    `fs.ErrorCantCopy`. File references cannot be reused across chats when
    restrict-saving is active. rclone falls back to download + reupload
    automatically.
- **No restrict-saving** → server-side copy via file_reference reuse.

rclone's sync engine logs "server side copy" vs regular "copied" naturally based
on whether `Copy()` succeeded or fell back.

### Move (server-side)

Telegram has no native move. Implemented as:

1. Copy to destination (server-side if possible, reupload if restricted).
2. **Confirm** the new message landed — verify the send response.
3. Delete the original message.

Never delete before confirming the copy succeeded.

- Same restrict-saving rules as Copy for the copy step.
- Bypass not met → return `fs.ErrorCantMove`.

### Delete

- **Group chats:** edit caption to `"."` first (allows bridge bots like
  tedicross to propagate the edit to Discord), short delay (~2s), then
  attempt delete. If the message is already gone (bridge bot deleted it),
  swallow the error — this is expected, not a failure.
- **Channels and DMs:** delete directly, no caption edit.

### Mkdir / Rmdir

Not applicable. Chat "directories" are virtual and cannot be created or removed
from rclone. These return `nil` / appropriate no-op.

---

## Restrict-Saving Logic

Restrict-saving is a per-chat flag that limits content saving/forwarding.

| Condition | Download | Server-side Copy | Reupload Copy | Move |
|-----------|----------|------------------|---------------|------|
| No restrict-saving | ✅ | ✅ | ✅ | ✅ |
| Restrict-saving + admin | ✅ | ❌ (reupload) | ✅ | ✅ (via reupload) |
| Restrict-saving + bypass flag | ✅ | ❌ (reupload) | ✅ | ✅ (via reupload) |
| Restrict-saving + neither | ❌ error | ❌ error | ❌ error | ❌ error |

"Reupload" means `Copy()` returns `fs.ErrorCantCopy` and rclone handles the
fallback transparently.

---

## Caching

### Metadata cache

- Keyed by chat ID.
- Stores per-file: message ID, filename, size, MIME type, date,
  file_reference, access hash.
- **First access** to a chat: full paginated scan of `messages.getHistory`.
- **Subsequent access:** incremental — only fetch messages newer than the last
  known message ID.
- Persisted to `cache_dir` on disk between runs (JSON or gob).

### file_reference refresh

Telegram silently expires file_references. Standard retry pattern:

1. Attempt operation with cached file_reference.
2. On `FILE_REFERENCE_EXPIRED` error: call `messages.getMessages` for the
   specific message ID to get a fresh reference.
3. Update cache, retry operation once.

### Chat metadata cache

Per-chat flags (restrict-saving, admin status) cached with a reasonable TTL
(~5 min). Refreshed on access errors or explicit invalidation.

---

## Rate Limiting

Uses rclone's built-in `pacer` with defaults tuned per auth type:

- **Bot:** conservative — 20 messages/minute per chat, 30/second global.
- **User:** moderate — flood-wait-driven.
- **Premium user:** most permissive.

### Flood wait handling

- `FLOOD_WAIT`: always sleep the server-specified duration.
- `FLOOD_PREMIUM_WAIT`: if authenticated as premium, ignore (continue — haven't
  hit real ceiling). If not premium, treat as `FLOOD_WAIT`.

---

## fs.Features

```go
&fs.Features{
    CanHaveEmptyDirectories: false,
    DuplicateFiles:          true,   // same filename possible across messages
    ReadMimeType:            true,
    Copy:                    f.Copy,
    Move:                    f.Move,
}
```

---

## File Structure

```
backend/telegram/
├── telegram.go          # main backend: Fs, Object, config, NewFs
├── telegram_test.go     # unit tests
└── api/
    ├── client.go        # gotd/td wrapper: auth, session, API calls
    ├── types.go         # internal types (cached file metadata, chat info)
    └── cache.go         # metadata cache: load/save/refresh/invalidate
```

---

## Out of Scope (for now)

- **Chat listing at root** — may add later.
- **File chunking / splitting** — use teldrive for that. May revisit with
  merge-instruction templates.
- **Voice/audio message support** — documents and media only.
- **Inline bots / stickers** — not files.
- **Message text content** — only file attachments.
- **Editing sent files** — Put is append-only (new message per upload).
